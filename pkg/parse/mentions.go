package parse

import (
	"html"
	"regexp"
	"slices"
	"strings"
)

// mentionRe finds words shaped like Prometheus metric names. A name needs
// an underscore or a colon so plain prose ("latency", "queue") never
// counts; callers keep only the words that name a collected metric.
var mentionRe = regexp.MustCompile(`[A-Za-z_:][A-Za-z0-9_:]*[_:][A-Za-z0-9_:]*`)

// Mentions returns the distinct metric-shaped names in a runbook or
// postmortem, sorted: prose, code blocks and links alike. A name a runbook
// only mentions isn't a declared dependency (only a query step is, see
// RunbookSteps), but it says someone expects to look at the signal in an
// incident, so an unread signal it names is dormant, not a drop
// candidate (Sprint 3/S4 "dormant: confirm"). isHTML strips tags first.
func Mentions(text string, isHTML bool) []string {
	if len(text) > maxRunbook {
		text = text[:maxRunbook]
	}
	if isHTML {
		text = html.UnescapeString(htmlTag.ReplaceAllString(text, " "))
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range mentionRe.FindAllString(text, -1) {
		m = strings.TrimRight(m, ":") // "words:" ends a sentence, not a name
		if len(m) < 4 || !strings.ContainsAny(m, "_:") || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}
