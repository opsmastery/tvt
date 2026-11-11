package parse

import (
	"regexp"
	"strconv"
	"strings"
)

// Grafana template variables, the same three forms Grafana's own
// interpolation regex accepts: $var, [[var]] / [[var:fmt]], and
// ${var} / ${var.field} / ${var:fmt}.
var grafanaVarRe = regexp.MustCompile(`\$(\w+)|\[\[(\w+?)(?::(\w+))?\]\]|\$\{(\w+)(?:\.([^:^}]+))?(?::([^}]+))?\}`)

// grafanaVar is one variable occurrence that substitution replaced.
type grafanaVar struct {
	text string // as written, e.g. "${job:pipe}"
	name string // "job"
}

// placeholderPrefix marks text that substitution wrote. It is a valid
// identifier fragment, so it survives as part of a metric or label name.
const placeholderPrefix = "__tvtvar"

func placeholder(i int) string { return placeholderPrefix + strconv.Itoa(i) + "__" }

var placeholderRe = regexp.MustCompile(placeholderPrefix + `(\d+)__`)

// Built-in Grafana time variables. They change the window a query reads,
// never which series or labels it reads, so substituting them does not
// make a reference assumed.
var grafanaDurationVars = map[string]bool{
	"__interval": true, "__rate_interval": true, "__range": true, "__auto": true,
}

var grafanaNumberVars = map[string]bool{
	"__interval_ms": true, "__rate_interval_ms": true, "__range_s": true, "__range_ms": true,
}

// substituteGrafanaVars replaces Grafana variables so the query parses as
// PromQL. Built-in time variables and any variable inside [...] or after
// offset become a 5m duration; after @ a number. Every other variable
// becomes a placeholder identifier (or string content, inside quotes) that
// the caller maps back with restoreVars, so where it lands (metric name,
// label name, matcher value) decides what the reference loses.
//
// Names that are all digits ($1 in a label_replace replacement) are not
// variables. A query with no variables comes back unchanged.
func substituteGrafanaVars(query string) (string, []grafanaVar) {
	return substitute(query, false)
}

// placeholderVars replaces every variable with a placeholder, whatever its
// context, so the text keeps one token per variable for fallbackRefs.
func placeholderVars(query string) (string, []grafanaVar) {
	return substitute(query, true)
}

func substitute(query string, allPlaceholders bool) (string, []grafanaVar) {
	locs := grafanaVarRe.FindAllStringSubmatchIndex(query, -1)
	if len(locs) == 0 {
		return query, nil
	}
	ctx := scanContexts(query)
	var b strings.Builder
	var vars []grafanaVar
	last := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		name := ""
		for g := 2; g+1 < len(loc); g += 2 {
			if loc[g] >= 0 && (g == 2 || g == 4 || g == 8) {
				name = query[loc[g]:loc[g+1]]
				break
			}
		}
		if name == "" || isDigits(name) {
			continue
		}
		b.WriteString(query[last:start])
		last = end
		c := contextAt(query, ctx, start)
		switch {
		case allPlaceholders || c == ctxName || c == ctxString:
			b.WriteString(placeholder(len(vars)))
			vars = append(vars, grafanaVar{text: query[start:end], name: name})
		case grafanaDurationVars[name]:
			b.WriteString("5m")
		case grafanaNumberVars[name]:
			b.WriteString("300")
		case c == ctxDuration:
			b.WriteString("5m")
		case c == ctxNumber:
			b.WriteString("0")
		default:
			b.WriteString(placeholder(len(vars)))
			vars = append(vars, grafanaVar{text: query[start:end], name: name})
		}
	}
	b.WriteString(query[last:])
	return b.String(), vars
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

type textCtx uint8

const (
	ctxCode textCtx = iota
	ctxString
	ctxDuration // inside [...], or the token after offset
	ctxNumber   // the token after @
	ctxName     // directly after a name character: part of that name
)

// scanContexts labels each byte of a query as code, string (strings and
// # comments) or duration (inside [...]); contextAt refines code positions
// after offset or @.
func scanContexts(q string) []textCtx {
	ctx := make([]textCtx, len(q)+1)
	// [[var]] tokens are variables, not range brackets: they take the
	// context around them and leave the bracket depth alone.
	bracketVars := map[int]int{}
	for _, loc := range grafanaVarRe.FindAllStringIndex(q, -1) {
		if strings.HasPrefix(q[loc[0]:], "[[") {
			bracketVars[loc[0]] = loc[1]
		}
	}
	brackets := 0
	for i := 0; i < len(q); i++ {
		if end, ok := bracketVars[i]; ok {
			if brackets > 0 {
				for j := i; j < end; j++ {
					ctx[j] = ctxDuration
				}
			}
			i = end - 1
			continue
		}
		switch c := q[i]; c {
		case '#': // comment to end of line; treated like a string
			end := len(q)
			if nl := strings.IndexByte(q[i:], '\n'); nl >= 0 {
				end = i + nl
			}
			for j := i; j < end; j++ {
				ctx[j] = ctxString
			}
			i = end - 1
			continue
		case '"', '\'', '`':
			end := stringEnd(q, i)
			for j := i; j < end; j++ {
				ctx[j] = ctxString
			}
			i = end - 1
			continue
		case '[':
			brackets++
		case ']':
			if brackets > 0 {
				brackets--
			}
		}
		if brackets > 0 {
			ctx[i] = ctxDuration
		}
	}
	return ctx
}

// contextAt is the context of a variable starting at i: a code position
// right after "offset" (optionally "offset -") is a duration, right after
// "@" a number.
func contextAt(q string, ctx []textCtx, i int) textCtx {
	if ctx[i] != ctxString && i > 0 && isIdentByte(q[i-1]) && !(ctx[i] == ctxDuration && q[i-1] == ':') {
		return ctxName // offset$v and A$__interval are names
	}
	if ctx[i] != ctxCode {
		return ctx[i]
	}
	before := strings.TrimRight(q[:i], " \t\n")
	before = strings.TrimRight(strings.TrimSuffix(before, "-"), " \t\n")
	switch {
	case strings.HasSuffix(before, "@"):
		return ctxNumber
	case strings.HasSuffix(before, "offset") && (len(before) == 6 || !isIdentByte(before[len(before)-7])):
		return ctxDuration
	}
	return ctxCode
}

// stringEnd returns the index just past the string literal starting at i,
// or len(q) when it is unterminated.
func stringEnd(q string, i int) int {
	quote := q[i]
	for j := i + 1; j < len(q); j++ {
		switch {
		case q[j] == '\\' && quote != '`':
			j++
		case q[j] == quote:
			return j + 1
		}
	}
	return len(q)
}

func isIdentByte(c byte) bool {
	return c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// restoreVars puts the original variable text back in place of
// placeholders and returns the variables it found.
func restoreVars(s string, vars []grafanaVar) (string, []grafanaVar) {
	if len(vars) == 0 || !strings.Contains(s, placeholderPrefix) {
		return s, nil
	}
	var found []grafanaVar
	out := placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		i, err := strconv.Atoi(placeholderRe.FindStringSubmatch(m)[1])
		if err != nil || i >= len(vars) {
			return m
		}
		found = append(found, vars[i])
		return vars[i].text
	})
	return out, found
}

// nameRegex turns a name containing placeholders into a regex over the
// names it could be: literal parts quoted, each variable ".*" (a variable
// can be empty: ${prefix}requests_total may select requests_total).
func nameRegex(s string) string {
	parts := placeholderRe.Split(s, -1)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return strings.Join(parts, ".*")
}
