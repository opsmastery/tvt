package parse

import "strings"

// MetricsQL (VictoriaMetrics) extends PromQL. When the PromQL parser
// rejects a MetricsQL query, fallbackRefs reads it with the two MetricsQL
// constructs that would otherwise turn into wrong candidates (Sprint 2
// item 17):
//
//   - WITH templates. In WITH (cf = {job="api"}, f(m) = rate(m[5m])) the
//     names cf, f and m are bindings, not metrics, and {job="api"} is a
//     label filter spliced into selectors like foo{cf}, not a selector of
//     every metric.
//   - Operators and modifiers that follow an operand: default, if, ifnot,
//     keep_metric_names, limit.

// metricsqlOperators are MetricsQL words that only follow an operand
// (x default 0, sum(...) by (a) limit 5, rate(...) keep_metric_names).
// After an operand a name can't be a metric, so they are skipped there and
// still candidates anywhere else.
var metricsqlOperators = map[string]bool{
	"default": true, "if": true, "ifnot": true, "keep_metric_names": true, "limit": true,
}

// withBindings is what a query's WITH templates bind.
type withBindings struct {
	// names are template, function and parameter names.
	names map[string]bool
	// filters are names bound to a bare {...} label filter.
	filters map[string]bool
	// filterAt holds the offsets of those filters' opening braces.
	filterAt map[int]bool
	// defAt holds the offsets of binding names at their definition.
	defAt map[int]bool
}

// parseWith finds every WITH (...) block in text (outside strings and
// comments) and what it binds. Malformed blocks bind what could be read.
func parseWith(text string) withBindings {
	w := withBindings{names: map[string]bool{}, filters: map[string]bool{}, filterAt: map[int]bool{}, defAt: map[int]bool{}}
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			i = stringEnd(text, i)
		case c == '#':
			i = lineEnd(text, i)
		case isIdentByte(c) && !(c >= '0' && c <= '9'):
			start := i
			for i < len(text) && isIdentByte(text[i]) {
				i++
			}
			if !strings.EqualFold(text[start:i], "with") || (start > 0 && isIdentByte(text[start-1])) {
				continue
			}
			open := skipSpace(text, i)
			if open >= len(text) || text[open] != '(' {
				continue
			}
			end := matchClose(text, open)
			w.bindings(text, open+1, end)
		default:
			i++
		}
	}
	return w
}

// bindings reads the comma-separated name[(params)] = value list in
// text[from:to].
func (w *withBindings) bindings(text string, from, to int) {
	for _, span := range splitTop(text, from, to) {
		i := skipSpace(text, span[0])
		start := i
		for i < span[1] && isIdentByte(text[i]) {
			i++
		}
		if i == start {
			continue
		}
		name := text[start:i]
		w.names[name] = true
		w.defAt[start] = true
		i = skipSpace(text, i)
		if i < span[1] && text[i] == '(' {
			pend := matchClose(text, i)
			for _, p := range splitTop(text, i+1, min(pend, span[1])) {
				if param := strings.TrimSpace(text[p[0]:p[1]]); param != "" {
					w.names[param] = true
				}
			}
			i = skipSpace(text, min(pend+1, span[1]))
		}
		if i >= span[1] || text[i] != '=' {
			continue
		}
		v := skipSpace(text, i+1)
		if v < span[1] && text[v] == '{' {
			if end := matchClose(text, v); end < span[1] && strings.TrimSpace(text[end+1:span[1]]) == "" {
				w.filters[name] = true
				w.filterAt[v] = true
			}
		}
	}
}

// splitTop splits text[from:to] at commas outside brackets and strings.
func splitTop(text string, from, to int) [][2]int {
	var out [][2]int
	depth, start := 0, from
	for i := from; i < to; i++ {
		switch text[i] {
		case '"', '\'', '`':
			i = min(stringEnd(text, i), to) - 1
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, [2]int{start, i})
				start = i + 1
			}
		}
	}
	return append(out, [2]int{start, to})
}

// matchClose returns the offset of the bracket closing the one at open,
// counting every bracket kind and skipping strings, or len(text).
func matchClose(text string, open int) int {
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '"', '\'', '`':
			i = stringEnd(text, i) - 1
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(text)
}

// operandBefore: what ends right before offset i (spaces allowed) is an
// operand: a closing bracket, a number, or a name that isn't a keyword.
func operandBefore(s string, i int) bool {
	j := i
	for j > 0 && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n') {
		j--
	}
	if j == 0 {
		return false
	}
	switch c := s[j-1]; {
	case c == ')' || c == ']' || c == '}':
		return true
	case c >= '0' && c <= '9':
		return true
	}
	k := j
	for k > 0 && isIdentByte(s[k-1]) {
		k--
	}
	word := strings.ToLower(s[k:j])
	return word != "" && !promqlKeywords[word] && !metricsqlOperators[word]
}

func lineEnd(s string, i int) int {
	if nl := strings.IndexByte(s[i:], '\n'); nl >= 0 {
		return i + nl + 1
	}
	return len(s)
}
