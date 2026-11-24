package scan

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/opsmastery/tvt/pkg/parse"
)

// MaxSelectorLookups caps how many label-only selectors a scan resolves
// through the backend; the rest keep the conservative reading (every
// collected metric).
const MaxSelectorLookups = 1000

// SelectorKey is the backend selector for a reference that names no
// metric: its label matchers other than __name__ and those holding a
// template variable, sorted, as {l="v",...}. ok is false when no matcher
// is left that excludes the empty value: Prometheus refuses such a
// selector, and it would select nearly everything anyway.
//
// tvt asks the backend which metric names carry those labels
// (Inputs.SelectorMetrics), so a rule selecting by label, such as
// {asserts_metric_request="total"}, protects the metrics that have the
// label instead of every metric.
func SelectorKey(ms []parse.Matcher) (key string, ok bool) {
	var parts []string
	for _, m := range ms {
		if m.Label == "__name__" || m.Variable || strings.Contains(m.Value, "$") {
			continue
		}
		if !matchesEmpty(m) {
			ok = true
		}
		parts = append(parts, m.Label+string(m.Op)+strconv.Quote(m.Value))
	}
	if !ok {
		return "", false
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ",") + "}", true
}

// matchesEmpty reports whether a matcher accepts a series without the
// label (its value is empty). An invalid regex counts as matching.
func matchesEmpty(m parse.Matcher) bool {
	switch m.Op {
	case parse.MatchEqual:
		return m.Value == ""
	case parse.MatchNotEqual:
		return m.Value != ""
	case parse.MatchRegexp, parse.MatchNotRegex:
		re, err := regexp.Compile("^(?:" + m.Value + ")$")
		if err != nil {
			return true
		}
		return re.MatchString("") == (m.Op == parse.MatchRegexp)
	}
	return true
}

// LabelOnlySelectors returns the distinct SelectorKeys of the selectors
// in exprs (PromQL) that name no metric outright, sorted. Expressions
// that don't parse are skipped: they keep the conservative reading.
func LabelOnlySelectors(exprs []string) []string {
	seen := map[string]bool{}
	for _, e := range exprs {
		refs, err := parse.PromQL(e)
		if err != nil {
			continue
		}
		for _, r := range refs {
			if r.Metric != "" {
				continue
			}
			if k, ok := SelectorKey(r.Matchers); ok {
				seen[k] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > MaxSelectorLookups {
		out = out[:MaxSelectorLookups]
	}
	return out
}
