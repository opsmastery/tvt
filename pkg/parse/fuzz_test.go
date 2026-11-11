package parse

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// FuzzLogStreamSelectors: the stopgap extractor never panics; whatever it
// accepts has matchers with label names, and its canonical form parses back
// to the same matchers. (A rejected query references every stream, so
// rejecting is always safe; accepting wrongly is not.)
func FuzzLogStreamSelectors(f *testing.F) {
	for _, s := range []string{
		`sum(count_over_time({app="shop", component="audit"} |= "result=denied" [5m])) > 20`,
		`{app="shop"} | line_format "{{.msg}}"`,
		"{app=~`a|b`, level!=\"debug\"}",
		`sum by (app) (rate({app="a"}[1m])) / sum(rate({app!~"b"}[1m]))`,
		"# {x=\"y\"}\n{app=\"shop\"}",
		`{app="a\"b"}`, `{}`, `{app}`, `}`, `"{`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		refs, err := LogStreamSelectors(q)
		if err != nil {
			return
		}
		if len(refs) == 0 {
			t.Fatalf("%q: no error and no selectors", q)
		}
		for _, r := range refs {
			if len(r.Matchers) == 0 {
				t.Fatalf("%q: empty selector accepted", q)
			}
			canon := CanonicalSelector(r.Matchers)
			back, err := LogStreamSelectors(canon)
			if err != nil || len(back) != 1 {
				t.Fatalf("%q: canonical %q does not parse back: %v", q, canon, err)
			}
			want := append([]Matcher(nil), r.Matchers...)
			sort.Slice(want, func(i, j int) bool { return CanonicalSelector(want[i:i+1]) < CanonicalSelector(want[j:j+1]) })
			got := append([]Matcher(nil), back[0].Matchers...)
			sort.Slice(got, func(i, j int) bool { return CanonicalSelector(got[i:i+1]) < CanonicalSelector(got[j:j+1]) })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%q: canonical %q parses back to %+v, want %+v", q, canon, got, want)
			}
		}
	})
}

// FuzzPromQL: never panics; a query that doesn't parse still references
// something (assumed, every label), never nothing; placeholder text never
// leaks; and a metric name comes from the query text.
func FuzzPromQL(f *testing.F) {
	for _, s := range []string{
		`sum by (job) (rate(http_requests_total{job="shop"}[5m]))`,
		`a * on (job) group_left (team) b`,
		`histogram_quantile(0.99, sum by (le) (rate(h_bucket[$__rate_interval])))`,
		`label_replace(x, "svc", "$1", "service", "(.*)")`,
		`max_over_time(rate(x[1m])[1h:5m] offset $shift)`,
		`sum by ($group) (${prefix}_total{job=~"$job"})`,
		`x{a="[[b:csv]]"} @ ${__to:date:seconds}`,
		`count_values("v", build_info) unless absent(up)`,
		`{__name__=~"node_.+", job!=""}`,
		`sum(rate(x[5m]) by (code)`, `$var`, `)`, `{`, `"`, ``,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		refs, err := PromQL(q)
		if strings.TrimSpace(q) == "" {
			return
		}
		if err != nil {
			if len(refs) == 0 {
				t.Fatalf("%q: parse error and no refs", q)
			}
			for _, r := range refs {
				if !r.Assumed || !r.AllLabels {
					t.Fatalf("%q: fallback ref not assumed/all labels: %+v", q, r)
				}
			}
		}
		for _, r := range refs {
			if r.Metric == "" && len(r.Matchers) == 0 && err != nil {
				t.Fatalf("%q: ref names nothing: %+v", q, r)
			}
			if r.Metric != "" && !strings.ContainsRune(q, '\\') && !strings.Contains(q, r.Metric) {
				t.Fatalf("%q: metric %q is not in the query", q, r.Metric)
			}
			all := r.Metric + strings.Join(r.Grouping, " ") + strings.Join(r.Without, " ") + strings.Join(r.Created, " ")
			for _, m := range r.Matchers {
				all += m.Label + m.Value
			}
			if strings.Contains(all, placeholderPrefix) && !strings.Contains(q, placeholderPrefix) {
				t.Fatalf("%q: placeholder leaked: %+v", q, r)
			}
		}
	})
}
