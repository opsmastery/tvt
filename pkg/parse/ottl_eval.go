package parse

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Tri is a three-valued truth: what tvt can say about a condition over a
// signal it knows only partly.
type Tri int8

const (
	False Tri = iota
	True
	Unknown
)

func (t Tri) String() string { return [...]string{"false", "true", "unknown"}[t] }

func (t Tri) and(u Tri) Tri {
	switch {
	case t == False || u == False:
		return False
	case t == True && u == True:
		return True
	}
	return Unknown
}

func (t Tri) or(u Tri) Tri {
	switch {
	case t == True || u == True:
		return True
	case t == False && u == False:
		return False
	}
	return Unknown
}

func (t Tri) not() Tri {
	switch t {
	case True:
		return False
	case False:
		return True
	}
	return Unknown
}

// OTTLEnv answers paths for EvalWhere. Path gets a path as OTTL.Reads
// spells it and returns its value (a string, int64, float64, bool, or nil
// for an absent key) and whether it's known.
type OTTLEnv interface {
	Path(path string) (any, bool)
}

// EvalWhere is what a statement's where clause, or a condition, says
// about one item: True when it surely holds, False when it surely
// doesn't, Unknown otherwise. A statement with no where clause runs on
// every item: True. Functions tvt doesn't evaluate, enums, math and
// unknown paths make the parts that use them Unknown, so a caller that
// refuses on "can be true" (anything but False) fails closed.
func (o OTTL) EvalWhere(env OTTLEnv) Tri {
	if o.where == nil {
		return True
	}
	return evalBool(o.where, env)
}

// ArgString is the editor's i-th argument when it's a string literal.
func (o OTTL) ArgString(i int) (string, bool) {
	if i >= len(o.args) {
		return "", false
	}
	l, ok := o.args[i].(*ottlLit)
	if !ok || !l.str {
		return "", false
	}
	return l.s, true
}

// ArgStrings is the editor's i-th argument when it's a list of string
// literals (keep_keys' keys).
func (o OTTL) ArgStrings(i int) ([]string, bool) {
	if i >= len(o.args) {
		return nil, false
	}
	l, ok := o.args[i].(*ottlList)
	if !ok {
		return nil, false
	}
	var out []string
	for _, it := range l.items {
		s, ok := it.(*ottlLit)
		if !ok || !s.str {
			return nil, false
		}
		out = append(out, s.s)
	}
	return out, true
}

type unknownValue struct{}

func evalBool(n ottlNode, env OTTLEnv) Tri {
	switch n := n.(type) {
	case *ottlBin:
		switch n.op {
		case "and":
			return evalBool(n.l, env).and(evalBool(n.r, env))
		case "or":
			return evalBool(n.l, env).or(evalBool(n.r, env))
		case "==", "!=", "<", ">", "<=", ">=":
			return compare(n.op, evalValue(n.l, env), evalValue(n.r, env))
		}
	case *ottlUnary:
		if n.op == "not" {
			return evalBool(n.x, env).not()
		}
	}
	switch v := evalValue(n, env).(type) {
	case bool:
		if v {
			return True
		}
		return False
	}
	return Unknown
}

func evalValue(n ottlNode, env OTTLEnv) any {
	switch n := n.(type) {
	case *ottlLit:
		if n.str {
			return n.s
		}
		switch n.s {
		case "true":
			return true
		case "false":
			return false
		case "nil":
			return nil
		}
		if i, err := strconv.ParseInt(n.s, 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(n.s, 64); err == nil {
			return f
		}
		return unknownValue{}
	case *ottlPath:
		for _, ks := range n.keys {
			for _, k := range ks {
				if _, ok := k.(*ottlLit); !ok {
					return unknownValue{}
				}
			}
		}
		if v, ok := env.Path(n.text()); ok {
			return v
		}
		return unknownValue{}
	case *ottlCall:
		if len(n.keys) > 0 {
			return unknownValue{}
		}
		return evalCall(n, env)
	case *ottlBin:
		switch n.op {
		case "and", "or", "==", "!=", "<", ">", "<=", ">=":
			switch evalBool(n, env) {
			case True:
				return true
			case False:
				return false
			}
		}
	case *ottlUnary:
		if n.op == "not" {
			switch evalBool(n, env) {
			case True:
				return true
			case False:
				return false
			}
		}
	}
	return unknownValue{}
}

var regexCache = map[string]*regexp.Regexp{}

func evalCall(c *ottlCall, env OTTLEnv) any {
	arg := func(i int) any {
		if i >= len(c.args) || c.args[i].name != "" {
			return unknownValue{}
		}
		return evalValue(c.args[i].value, env)
	}
	switch c.name {
	case "IsMatch":
		v, p := arg(0), arg(1)
		pat, ok := p.(string)
		if !ok {
			return unknownValue{}
		}
		re, ok := regexCache[pat]
		if !ok {
			var err error
			if re, err = regexp.Compile(pat); err != nil {
				return unknownValue{}
			}
			regexCache[pat] = re
		}
		switch v := v.(type) {
		case string:
			return re.MatchString(v)
		case nil:
			return false
		case unknownValue:
			return unknownValue{}
		default:
			return re.MatchString(fmt.Sprint(v))
		}
	case "IsString":
		switch arg(0).(type) {
		case string:
			return true
		case unknownValue:
			return unknownValue{}
		}
		return false
	case "HasPrefix", "HasSuffix":
		v, ok1 := arg(0).(string)
		x, ok2 := arg(1).(string)
		if !ok1 || !ok2 {
			return unknownValue{}
		}
		if c.name == "HasPrefix" {
			return strings.HasPrefix(v, x)
		}
		return strings.HasSuffix(v, x)
	case "Len":
		if v, ok := arg(0).(string); ok {
			return int64(len(v))
		}
	case "String":
		switch v := arg(0).(type) {
		case string:
			return v
		case unknownValue:
			return v
		case nil:
			return unknownValue{}
		default:
			return fmt.Sprint(v)
		}
	}
	return unknownValue{}
}

func compare(op string, a, b any) Tri {
	if _, ok := a.(unknownValue); ok {
		return Unknown
	}
	if _, ok := b.(unknownValue); ok {
		return Unknown
	}
	if fa, ok := num(a); ok {
		if fb, ok := num(b); ok {
			return triOf(cmpNum(op, fa, fb))
		}
	}
	switch op {
	case "==":
		return triOf(sameValue(a, b))
	case "!=":
		return triOf(!sameValue(a, b))
	}
	sa, ok1 := a.(string)
	sb, ok2 := b.(string)
	if ok1 && ok2 {
		return triOf(cmpNum(op, float64(strings.Compare(sa, sb)), 0))
	}
	// OTTL: ordering across types is false.
	return False
}

func sameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a == b
}

func num(v any) (float64, bool) {
	switch v := v.(type) {
	case int64:
		return float64(v), true
	case float64:
		return v, true
	}
	return 0, false
}

func cmpNum(op string, a, b float64) bool {
	switch op {
	case "==":
		return a == b
	case "!=":
		return a != b
	case "<":
		return a < b
	case ">":
		return a > b
	case "<=":
		return a <= b
	}
	return a >= b
}

func triOf(b bool) Tri {
	if b {
		return True
	}
	return False
}

// Strings are the string literals in the where clause (or condition).
func (o OTTL) Strings() []string {
	var out []string
	var walk func(n ottlNode)
	walk = func(n ottlNode) {
		switch n := n.(type) {
		case *ottlLit:
			if n.str {
				out = append(out, n.s)
			}
		case *ottlPath:
			for _, ks := range n.keys {
				for _, k := range ks {
					walk(k)
				}
			}
		case *ottlCall:
			for _, a := range n.args {
				walk(a.value)
			}
		case *ottlList:
			for _, x := range n.items {
				walk(x)
			}
		case *ottlBin:
			walk(n.l)
			walk(n.r)
		case *ottlUnary:
			walk(n.x)
		}
	}
	if o.where != nil {
		walk(o.where)
	}
	return out
}

// Equalities are the path == literal terms a condition's top-level ands
// require, by path as Reads spells it: what an item matching it surely
// has. A condition with or or not at the top gives none.
func (o OTTL) Equalities() map[string]any {
	out := map[string]any{}
	var walk func(n ottlNode)
	walk = func(n ottlNode) {
		b, ok := n.(*ottlBin)
		if !ok {
			return
		}
		switch b.op {
		case "and":
			walk(b.l)
			walk(b.r)
		case "==":
			p, l := b.l, b.r
			if _, ok := p.(*ottlPath); !ok {
				p, l = l, p
			}
			pt, ok1 := p.(*ottlPath)
			if lit, ok2 := l.(*ottlLit); ok1 && ok2 {
				out[pt.text()] = evalValue(lit, nil)
			}
		}
	}
	if o.where != nil {
		walk(o.where)
	}
	return out
}
