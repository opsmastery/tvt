package parse

import (
	"fmt"
	"strconv"
	"strings"
)

// The OTTL grammar tvt reads (pkg/ottl LANGUAGE.md, v1):
//
//	statement  = editor-call [ "where" bool ]
//	bool       = and { "or" and } ; and = not { "and" not } ; not = "not" not | cmp
//	cmp        = math [ ("=="|"!="|"<"|">"|"<="|">=") math ]
//	math       = term { ("+"|"-") term } ; term = unary { ("*"|"/") unary }
//	unary      = ("-"|"+") unary | primary
//	primary    = literal | enum | path | call {key} | list | map | lambda | "(" bool ")"
//	call       = ident "(" [ arg { "," arg } ] ")" ; arg = [ ident "=" ] bool
//	path       = ident { "." ident | key } ; key = "[" bool "]"
//	lambda     = "(" [ ident { "," ident } ] ")" "=>" bool
//
// Permissive by design: any value is accepted where a boolean is, and
// arguments, list items and lambda bodies are parsed as booleans.

type ottlTokKind int

const (
	tEOF ottlTokKind = iota
	tIdent
	tString
	tNumber
	tBytes
	tPunct
)

type ottlTok struct {
	kind ottlTokKind
	s    string // the identifier, punctuation, number text, or unquoted string
	pos  int
}

func lexOTTL(src string) ([]ottlTok, error) {
	var toks []ottlTok
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"':
			var b strings.Builder
			j := i + 1
			for ; j < len(src) && src[j] != '"'; j++ {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					case '"', '\\':
						b.WriteByte(src[j])
					default:
						b.WriteByte('\\')
						b.WriteByte(src[j])
					}
					continue
				}
				b.WriteByte(src[j])
			}
			if j >= len(src) {
				return nil, fmt.Errorf("OTTL: unterminated string at %d", i)
			}
			toks = append(toks, ottlTok{tString, b.String(), i})
			i = j + 1
		case c == '0' && i+1 < len(src) && (src[i+1] == 'x' || src[i+1] == 'X'):
			j := i + 2
			for j < len(src) && isHex(src[j]) {
				j++
			}
			toks = append(toks, ottlTok{tBytes, src[i:j], i})
			i = j
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			j := i
			for j < len(src) && (isDigit(src[j]) || src[j] == '.' || src[j] == 'e' || src[j] == 'E' ||
				((src[j] == '+' || src[j] == '-') && (src[j-1] == 'e' || src[j-1] == 'E'))) {
				j++
			}
			toks = append(toks, ottlTok{tNumber, src[i:j], i})
			i = j
		case isIdentStart(c):
			j := i + 1
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			toks = append(toks, ottlTok{tIdent, src[i:j], i})
			i = j
		default:
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "==", "!=", "<=", ">=", "=>":
				toks = append(toks, ottlTok{tPunct, two, i})
				i += 2
				continue
			}
			if !strings.ContainsRune("()[]{},.:=<>+-*/", rune(c)) {
				return nil, fmt.Errorf("OTTL: unexpected %q at %d", c, i)
			}
			toks = append(toks, ottlTok{tPunct, string(c), i})
			i++
		}
	}
	return append(toks, ottlTok{tEOF, "", len(src)}), nil
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isHex(c byte) bool        { return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }
func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) }

// The syntax tree. Only what the walk needs is kept.
type ottlNode interface{}

type (
	ottlLit struct {
		s   string
		str bool
	}
	ottlEnum struct{ name string }
	ottlPath struct {
		segs []string     // identifiers, in order
		keys [][]ottlNode // keys after each identifier (same length as segs)
	}
	ottlArg struct {
		name       string
		value      ottlNode
		start, end int // byte offsets in the source
	}
	ottlCall struct {
		name string
		args []ottlArg
		keys []ottlNode
	}
	ottlList   struct{ items []ottlNode }
	ottlMap    struct{ pairs []ottlNode } // key, value, key, value, ...
	ottlLambda struct {
		params []string
		body   ottlNode
	}
	ottlBin struct {
		op   string // ==, !=, <, >, <=, >=, +, -, *, /, and, or
		l, r ottlNode
	}
	ottlUnary struct {
		op string // not, -, +
		x  ottlNode
	}
)

// text is the path as tvt keys it: segments joined with dots, string keys
// quoted, int keys as written, any other key [*].
func (p *ottlPath) text() string {
	var b strings.Builder
	for i, s := range p.segs {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s)
		for _, k := range p.keys[i] {
			b.WriteString(keyText(k))
		}
	}
	return b.String()
}

func keyText(k ottlNode) string {
	if l, ok := k.(*ottlLit); ok {
		if l.str {
			return "[" + strconv.Quote(l.s) + "]"
		}
		return "[" + l.s + "]"
	}
	return "[*]"
}

type ottlParser struct {
	src  string
	toks []ottlTok
	i    int
}

func newOTTLParser(s string) (*ottlParser, error) {
	toks, err := lexOTTL(s)
	if err != nil {
		return nil, err
	}
	return &ottlParser{src: s, toks: toks}, nil
}

func (p *ottlParser) peek() ottlTok { return p.toks[p.i] }
func (p *ottlParser) peekPunct(s string) bool {
	t := p.peek()
	return t.kind == tPunct && t.s == s
}
func (p *ottlParser) peekWord(s string) bool {
	t := p.peek()
	return t.kind == tIdent && t.s == s
}
func (p *ottlParser) expect(s string) error {
	if !p.peekPunct(s) {
		return p.errorf("want %q", s)
	}
	p.i++
	return nil
}
func (p *ottlParser) end() error {
	if p.peek().kind != tEOF {
		return p.errorf("unexpected text")
	}
	return nil
}
func (p *ottlParser) errorf(format string, a ...any) error {
	t := p.peek()
	near := t.s
	if t.kind == tEOF {
		near = "end"
	}
	return fmt.Errorf("OTTL: %s at %d (near %q)", fmt.Sprintf(format, a...), t.pos, near)
}

var ottlKeywords = setOf("and", "or", "not", "where")

func (p *ottlParser) boolExpr() (ottlNode, error) {
	l, err := p.andExpr()
	for err == nil && p.peekWord("or") {
		p.i++
		var r ottlNode
		if r, err = p.andExpr(); err == nil {
			l = &ottlBin{"or", l, r}
		}
	}
	return l, err
}

func (p *ottlParser) andExpr() (ottlNode, error) {
	l, err := p.notExpr()
	for err == nil && p.peekWord("and") {
		p.i++
		var r ottlNode
		if r, err = p.notExpr(); err == nil {
			l = &ottlBin{"and", l, r}
		}
	}
	return l, err
}

func (p *ottlParser) notExpr() (ottlNode, error) {
	if p.peekWord("not") {
		p.i++
		x, err := p.notExpr()
		return &ottlUnary{"not", x}, err
	}
	l, err := p.math()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind == tPunct {
		switch t.s {
		case "==", "!=", "<", ">", "<=", ">=":
			p.i++
			r, err := p.math()
			return &ottlBin{t.s, l, r}, err
		}
	}
	return l, nil
}

// value is a math expression: what an editor call or argument starts as.
func (p *ottlParser) value() (ottlNode, error) { return p.math() }

func (p *ottlParser) math() (ottlNode, error) {
	l, err := p.term()
	for err == nil && (p.peekPunct("+") || p.peekPunct("-")) {
		op := p.peek().s
		p.i++
		var r ottlNode
		if r, err = p.term(); err == nil {
			l = &ottlBin{op, l, r}
		}
	}
	return l, err
}

func (p *ottlParser) term() (ottlNode, error) {
	l, err := p.unary()
	for err == nil && (p.peekPunct("*") || p.peekPunct("/")) {
		op := p.peek().s
		p.i++
		var r ottlNode
		if r, err = p.unary(); err == nil {
			l = &ottlBin{op, l, r}
		}
	}
	return l, err
}

func (p *ottlParser) unary() (ottlNode, error) {
	if p.peekPunct("-") || p.peekPunct("+") {
		op := p.peek().s
		p.i++
		x, err := p.unary()
		return &ottlUnary{op, x}, err
	}
	return p.primary()
}

func (p *ottlParser) primary() (ottlNode, error) {
	t := p.peek()
	switch t.kind {
	case tString:
		p.i++
		return &ottlLit{s: t.s, str: true}, nil
	case tNumber, tBytes:
		p.i++
		return &ottlLit{s: t.s}, nil
	case tIdent:
		return p.identValue()
	case tPunct:
		switch t.s {
		case "(":
			if params, ok := p.lambdaParams(); ok {
				body, err := p.boolExpr()
				return &ottlLambda{params, body}, err
			}
			p.i++
			x, err := p.boolExpr()
			if err != nil {
				return nil, err
			}
			return x, p.expect(")")
		case "[":
			p.i++
			l := &ottlList{}
			items, err := p.items("]")
			l.items = items
			return l, err
		case "{":
			p.i++
			m := &ottlMap{}
			for !p.peekPunct("}") {
				k, err := p.boolExpr()
				if err != nil {
					return nil, err
				}
				if err := p.expect(":"); err != nil {
					return nil, err
				}
				v, err := p.boolExpr()
				if err != nil {
					return nil, err
				}
				m.pairs = append(m.pairs, k, v)
				if !p.peekPunct(",") {
					break
				}
				p.i++
			}
			return m, p.expect("}")
		}
	}
	return nil, p.errorf("want a value")
}

// lambdaParams consumes "(a, b) =>" when that's what comes next.
func (p *ottlParser) lambdaParams() ([]string, bool) {
	j := p.i + 1
	var params []string
	for p.toks[j].kind == tIdent {
		params = append(params, p.toks[j].s)
		j++
		if p.toks[j].kind == tPunct && p.toks[j].s == "," {
			j++
			continue
		}
		break
	}
	if p.toks[j].kind == tPunct && p.toks[j].s == ")" && p.toks[j+1].kind == tPunct && p.toks[j+1].s == "=>" {
		p.i = j + 2
		return params, true
	}
	return nil, false
}

func (p *ottlParser) items(closer string) ([]ottlNode, error) {
	var items []ottlNode
	for !p.peekPunct(closer) {
		x, err := p.boolExpr()
		if err != nil {
			return nil, err
		}
		items = append(items, x)
		if !p.peekPunct(",") {
			break
		}
		p.i++
	}
	return items, p.expect(closer)
}

func (p *ottlParser) identValue() (ottlNode, error) {
	t := p.peek()
	switch t.s {
	case "true", "false", "nil":
		p.i++
		return &ottlLit{s: t.s}, nil
	}
	if ottlKeywords[t.s] {
		return nil, p.errorf("want a value")
	}
	p.i++
	if p.peekPunct("(") {
		p.i++
		c := &ottlCall{name: t.s}
		for !p.peekPunct(")") {
			a := ottlArg{start: p.peek().pos}
			if p.peek().kind == tIdent && p.toks[p.i+1].kind == tPunct && p.toks[p.i+1].s == "=" {
				a.name = p.peek().s
				p.i += 2
			}
			v, err := p.boolExpr()
			if err != nil {
				return nil, err
			}
			a.value, a.end = v, p.peek().pos
			c.args = append(c.args, a)
			if !p.peekPunct(",") {
				break
			}
			p.i++
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		keys, err := p.keys()
		c.keys = keys
		return c, err
	}
	if isEnum(t.s) {
		return &ottlEnum{t.s}, nil
	}
	path := &ottlPath{segs: []string{t.s}}
	keys, err := p.keys()
	if err != nil {
		return nil, err
	}
	path.keys = append(path.keys, keys)
	for p.peekPunct(".") && p.toks[p.i+1].kind == tIdent {
		path.segs = append(path.segs, p.toks[p.i+1].s)
		p.i += 2
		keys, err := p.keys()
		if err != nil {
			return nil, err
		}
		path.keys = append(path.keys, keys)
	}
	return path, nil
}

func (p *ottlParser) keys() ([]ottlNode, error) {
	var keys []ottlNode
	for p.peekPunct("[") {
		p.i++
		k, err := p.boolExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// isEnum: an all-uppercase identifier (SEVERITY_NUMBER_WARN, STATUS_CODE_OK).
func isEnum(s string) bool {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			return false
		}
	}
	return true
}

// ottlWalk collects reads and functions from a tree.
type ottlWalk struct {
	reads  []string
	funcs  []string
	lambda bool
	params [][]string // lambda parameters in scope
}

func (w *ottlWalk) fn(name string) {
	for _, f := range w.funcs {
		if f == name {
			return
		}
	}
	w.funcs = append(w.funcs, name)
}

func (w *ottlWalk) read(path string) {
	for _, r := range w.reads {
		if r == path {
			return
		}
	}
	w.reads = append(w.reads, path)
}

func (w *ottlWalk) isParam(name string) bool {
	for _, ps := range w.params {
		for _, p := range ps {
			if p == name {
				return true
			}
		}
	}
	return false
}

// keys walks a path's computed keys without reading the path itself.
func (w *ottlWalk) keys(p *ottlPath) {
	for _, ks := range p.keys {
		for _, k := range ks {
			w.node(k)
		}
	}
}

func (w *ottlWalk) node(n ottlNode) {
	switch n := n.(type) {
	case *ottlPath:
		if !(len(n.segs) >= 1 && w.isParam(n.segs[0])) {
			w.read(n.text())
		}
		w.keys(n)
	case *ottlCall:
		w.fn(n.name)
		for _, a := range n.args {
			w.node(a.value)
		}
		for _, k := range n.keys {
			w.node(k)
		}
	case *ottlList:
		for _, x := range n.items {
			w.node(x)
		}
	case *ottlMap:
		for _, x := range n.pairs {
			w.node(x)
		}
	case *ottlLambda:
		w.lambda = true
		w.params = append(w.params, n.params)
		w.node(n.body)
		w.params = w.params[:len(w.params)-1]
	case *ottlBin:
		w.node(n.l)
		w.node(n.r)
	case *ottlUnary:
		w.node(n.x)
	}
}
