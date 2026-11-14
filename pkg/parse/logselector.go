package parse

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// scanStreamSelectors finds the stream selectors in text without parsing
// the rest, for LogQL's fallback when a query doesn't parse. It skips
// quoted strings and # comments (so braces in a line_format template are
// not selectors). A selector it can't read comes back with no matchers,
// meaning every stream.
func scanStreamSelectors(query string) [][]Matcher {
	var out [][]Matcher
	for i := 0; i < len(query); {
		switch query[i] {
		case '"', '`':
			end, err := skipString(query, i)
			if err != nil {
				return out
			}
			i = end
		case '#':
			if nl := strings.IndexByte(query[i:], '\n'); nl >= 0 {
				i += nl + 1
			} else {
				i = len(query)
			}
		case '{':
			ms, end, err := parseSelector(query, i)
			if err != nil {
				// Unreadable: a selector with no matchers, which stands
				// for every stream.
				out = append(out, nil)
				i++
				continue
			}
			out = append(out, ms)
			i = end
		default:
			i++
		}
	}
	return out
}

// CanonicalSelector formats matchers as a stream selector with matchers
// sorted by label, operator and value, so the same selector written in any
// order gets the same signal ID.
func CanonicalSelector(ms []Matcher) string {
	s := append([]Matcher(nil), ms...)
	sort.Slice(s, func(i, j int) bool {
		if s[i].Label != s[j].Label {
			return s[i].Label < s[j].Label
		}
		if s[i].Op != s[j].Op {
			return s[i].Op < s[j].Op
		}
		return s[i].Value < s[j].Value
	})
	parts := make([]string, len(s))
	for i, m := range s {
		parts[i] = m.Label + string(m.Op) + strconv.Quote(m.Value)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// parseSelector reads the selector starting at q[start] == '{' and returns
// its matchers and the offset just past the closing brace.
func parseSelector(q string, start int) ([]Matcher, int, error) {
	i := start + 1
	var ms []Matcher
	for {
		i = skipSpace(q, i)
		if i < len(q) && q[i] == '}' && len(ms) > 0 {
			return ms, i + 1, nil
		}
		// label
		j := i
		for j < len(q) && isIdent(q[j], j == i) {
			j++
		}
		if j == i {
			return nil, 0, fmt.Errorf("logql: selector at offset %d: expected a label name at offset %d", start, i)
		}
		label := q[i:j]
		i = skipSpace(q, j)
		// operator
		var op MatchOp
		switch {
		case strings.HasPrefix(q[i:], "=~"):
			op = MatchRegexp
		case strings.HasPrefix(q[i:], "!~"):
			op = MatchNotRegex
		case strings.HasPrefix(q[i:], "!="):
			op = MatchNotEqual
		case strings.HasPrefix(q[i:], "="):
			op = MatchEqual
		default:
			return nil, 0, fmt.Errorf("logql: selector at offset %d: expected a matcher operator after %q", start, label)
		}
		i = skipSpace(q, i+len(op))
		// value
		if i >= len(q) || (q[i] != '"' && q[i] != '`') {
			return nil, 0, fmt.Errorf("logql: selector at offset %d: expected a quoted value for %q", start, label)
		}
		end, err := skipString(q, i)
		if err != nil {
			return nil, 0, err
		}
		val, err := strconv.Unquote(q[i:end])
		if err != nil {
			return nil, 0, fmt.Errorf("logql: selector at offset %d: value for %q: %w", start, label, err)
		}
		ms = append(ms, Matcher{Label: label, Op: op, Value: val})
		i = skipSpace(q, end)
		if i < len(q) && q[i] == ',' {
			i++
			continue
		}
		if i < len(q) && q[i] == '}' {
			return ms, i + 1, nil
		}
		return nil, 0, fmt.Errorf("logql: selector at offset %d: expected ',' or '}'", start)
	}
}

// skipString returns the offset just past the string literal at q[i].
func skipString(q string, i int) (int, error) {
	quote := q[i]
	for j := i + 1; j < len(q); j++ {
		switch q[j] {
		case '\\':
			if quote == '"' {
				j++
			}
		case quote:
			return j + 1, nil
		}
	}
	return 0, fmt.Errorf("logql: unterminated string at offset %d", i)
}

// skipSpace skips whitespace and # comments.
func skipSpace(q string, i int) int {
	for i < len(q) {
		switch q[i] {
		case ' ', '\t', '\n', '\r':
			i++
		case '#':
			nl := strings.IndexByte(q[i:], '\n')
			if nl < 0 {
				return len(q)
			}
			i += nl + 1
		default:
			return i
		}
	}
	return i
}

func isIdent(c byte, first bool) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || (!first && '0' <= c && c <= '9')
}

// MatchesLabels reports whether a stream (or series) with labels is
// selected by every matcher, as Loki and Prometheus evaluate them: a
// missing label is the empty string, and regexes are fully anchored.
func MatchesLabels(ms []Matcher, labels map[string]string) (bool, error) {
	for _, m := range ms {
		v := labels[m.Label]
		var ok bool
		switch m.Op {
		case MatchEqual:
			ok = v == m.Value
		case MatchNotEqual:
			ok = v != m.Value
		case MatchRegexp, MatchNotRegex:
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			if err != nil {
				return false, fmt.Errorf("matcher %s%s%q: %w", m.Label, m.Op, m.Value, err)
			}
			ok = re.MatchString(v) == (m.Op == MatchRegexp)
		default:
			return false, fmt.Errorf("matcher %s: unknown operator %q", m.Label, m.Op)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}
