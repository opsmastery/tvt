package parse

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// LogStreamSelectors returns the stream selectors ({app="shop", ...}) in a
// LogQL query, one Ref per selector with only Matchers set.
//
// It is a stopgap until the LogQL extractor (ADR 0005) exists: it finds
// selectors but not the labels a query filters, groups or formats by, so a
// caller must treat each Ref as reading every label of its streams. It skips
// quoted strings and # comments, so the braces in a line_format template
// are not mistaken for selectors.
//
// A query with no selector, or with a selector it can't read, is an error;
// the caller must then assume the query reads every stream.
func LogStreamSelectors(query string) ([]Ref, error) {
	var refs []Ref
	for i := 0; i < len(query); {
		switch c := query[i]; c {
		case '"', '`':
			end, err := skipString(query, i)
			if err != nil {
				return nil, err
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
				return nil, err
			}
			refs = append(refs, Ref{Matchers: ms})
			i = end
		case '}':
			return nil, fmt.Errorf("logql: unexpected '}' at offset %d", i)
		default:
			i++
		}
	}
	if len(refs) == 0 {
		return nil, errors.New("logql: no stream selector")
	}
	return refs, nil
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

func skipSpace(q string, i int) int {
	for i < len(q) && (q[i] == ' ' || q[i] == '\t' || q[i] == '\n' || q[i] == '\r') {
		i++
	}
	return i
}

func isIdent(c byte, first bool) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || (!first && '0' <= c && c <= '9')
}
