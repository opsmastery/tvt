package parse

// UsesAbsence reports whether a PromQL or LogQL query calls absent() or
// absent_over_time() (Sprint 2 item 18). Such a query returns no series
// by design while its metric is healthy, so "the rule returned nothing for
// 30 days" (S6) must not call it broken. It still reads its metric and the
// labels it matches like any query: those are hard keeps, and dropping a
// matched label would fire the alert.
//
// It scans tokens rather than parsing, so it also answers for queries the
// parsers reject (MetricsQL, templates); names inside strings and comments
// don't count.
func UsesAbsence(query string) bool {
	for i := 0; i < len(query); {
		c := query[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			i = stringEnd(query, i)
		case c == '#':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case isIdentStart(c):
			j := i
			for j < len(query) && isIdentPart(query[j]) {
				j++
			}
			word := query[i:j]
			k := j
			for k < len(query) && (query[k] == ' ' || query[k] == '\t' || query[k] == '\n' || query[k] == '\r') {
				k++
			}
			if (word == "absent" || word == "absent_over_time") && k < len(query) && query[k] == '(' &&
				(i == 0 || !isNamePrefix(query[i-1])) {
				return true
			}
			i = j
		default:
			i++
		}
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == ':'
}

// isNamePrefix: a byte that would make the word part of a longer name.
func isNamePrefix(c byte) bool { return isIdentPart(c) || c == '.' }
