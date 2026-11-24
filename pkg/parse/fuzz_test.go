package parse

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/parse/internal/corpus"
)

// FuzzLogQL: never panics; a query that doesn't parse still references
// something (assumed, every label), never nothing; placeholder text never
// leaks; and an accepted selector's canonical form parses back to the same
// matchers. Seeds include the stopgap extractor's corpus.
func FuzzLogQL(f *testing.F) {
	for _, s := range []string{
		// LogStreamSelectors corpus
		`sum(count_over_time({app="shop", component="audit"} |= "result=denied" [5m])) > 20`,
		`{app="shop"} | line_format "{{.msg}}"`,
		"{app=~`a|b`, level!=\"debug\"}",
		`sum by (app) (rate({app="a"}[1m])) / sum(rate({app!~"b"}[1m]))`,
		"# {x=\"y\"}\n{app=\"shop\"}",
		`{app="a\"b"}`, `{}`, `{app}`, `}`, `"{`,
		// pipelines
		`{app="shop"} | json code="r.code" | code="500" | line_format "{{.code}}"`,
		`sum_over_time({app="shop"} | logfmt | unwrap bytes(size) | __error__="" [5m]) by (route)`,
		"{app=\"shop\"} | regexp `(?P<m>\\w+)` | label_format x=m, y=\"{{.z}}\" | keep x",
		`quantile_over_time(0.99, {app="$app"} | pattern "<a> <_>" | unwrap a [$__auto]) by (a)`,
		`{app="shop"} | (level="error" or level="warn"), size > 20MB |= ip("10.0.0.0/8")`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		refs, err := LogQL(q)
		if strings.TrimSpace(q) == "" {
			return
		}
		if len(refs) == 0 {
			t.Fatalf("%q: no refs (err %v)", q, err)
		}
		for _, r := range refs {
			if err != nil && (!r.Assumed || !r.AllLabels) {
				t.Fatalf("%q: fallback ref not assumed/all labels: %+v", q, r)
			}
			all := strings.Join(r.Grouping, " ") + strings.Join(r.Without, " ") + strings.Join(r.Created, " ") +
				strings.Join(r.Filters, " ") + strings.Join(r.StreamLabels, " ") + strings.Join(r.Extracted, " ")
			for _, m := range r.Matchers {
				all += m.Label + m.Value
			}
			if (strings.Contains(all, placeholderPrefix) && !strings.Contains(q, placeholderPrefix)) ||
				(strings.Contains(all, logPlaceholderPrefix) && !strings.Contains(q, logPlaceholderPrefix)) {
				t.Fatalf("%q: placeholder leaked: %+v", q, r)
			}
			if err != nil || len(r.Matchers) == 0 {
				continue
			}
			canon := CanonicalSelector(r.Matchers)
			back, berr := LogQL(canon)
			if berr != nil || len(back) != 1 {
				t.Fatalf("%q: canonical %q does not parse back: %v", q, canon, berr)
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
	// MetricsQL (Sprint 2 item 17): WITH templates and operators.
	f.Add(`WITH (cf = {job="api"}, r(m) = rate(m{cf}[5m])) r(x) default 0 keep_metric_names`)
	f.Add(`with (a = {b="c"}, ) a`)
	f.Add(`WITH (f(x = {`)
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

// FuzzRedactLogQL: redacting a query that parses never changes its refs,
// apart from matcher values.
func FuzzRedactLogQL(f *testing.F) {
	for _, q := range logqlTableQueries() {
		f.Add(q)
	}
	blank := func(refs []Ref) []Ref {
		for i := range refs {
			ms := make([]Matcher, len(refs[i].Matchers))
			for j, m := range refs[i].Matchers {
				ms[j] = Matcher{Label: m.Label, Op: m.Op, Variable: m.Variable}
			}
			refs[i].Matchers = ms
		}
		return refs
	}
	f.Fuzz(func(t *testing.T, q string) {
		want, err := LogQL(q)
		if err != nil {
			return
		}
		red := corpus.RedactLogQL(q)
		got, err := LogQL(red)
		if err != nil {
			t.Fatalf("%q redacted to %q, which doesn't parse: %v", q, red, err)
		}
		if !reflect.DeepEqual(blank(got), blank(want)) {
			t.Fatalf("%q redacted to %q changed refs:\n got %+v\nwant %+v", q, red, got, want)
		}
	})
}
