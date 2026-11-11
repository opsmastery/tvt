package parse

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPromQL(t *testing.T) {
	eq := func(l, v string) Matcher { return Matcher{Label: l, Op: MatchEqual, Value: v} }
	re := func(l, v string) Matcher { return Matcher{Label: l, Op: MatchRegexp, Value: v} }
	tests := []struct {
		name  string
		query string
		want  []Ref
	}{
		{"bare metric", `up`, []Ref{{Metric: "up"}}},
		{"matchers", `http_requests_total{job="shop", code!~"2.."}`, []Ref{{
			Metric:   "http_requests_total",
			Matchers: []Matcher{eq("job", "shop"), {Label: "code", Op: MatchNotRegex, Value: "2.."}},
		}}},
		{"name by matcher", `{__name__="up", job="a"}`, []Ref{{Metric: "up", Matchers: []Matcher{eq("job", "a")}}}},
		{"name regex", `{__name__=~"node_.+"}`, []Ref{{Matchers: []Matcher{re("__name__", "node_.+")}}}},
		{"sum by", `sum by (job, instance) (rate(x_total[5m]))`, []Ref{{Metric: "x_total", Grouping: []string{"instance", "job"}}}},
		{"sum without", `sum without (pod) (x)`, []Ref{{Metric: "x", Without: []string{"pod"}}}},
		{"aggregation resets outer needs", `max by (cluster) (sum by (job) (x))`, []Ref{{Metric: "x", Grouping: []string{"job"}}}},
		{"sum then join", `sum(a) / sum(b)`, []Ref{{Metric: "a"}, {Metric: "b"}}},
		{"join under sum by", `sum by (job) (a / b)`, []Ref{{Metric: "a", Grouping: []string{"job"}, AllLabels: true}, {Metric: "b", Grouping: []string{"job"}, AllLabels: true}}},
		{"without passes through", `sum without (pod) (a) / b`, []Ref{{Metric: "a", Without: []string{"pod"}, AllLabels: true}, {Metric: "b", AllLabels: true}}},
		{"topk passes through", `sum by (job) (topk by (pod) (3, x))`, []Ref{{Metric: "x", Grouping: []string{"job", "pod"}}}},
		{"topk param outside grouping", `topk by (job) (scalar(k), x)`, []Ref{
			{Metric: "k"},
			{Metric: "x", Grouping: []string{"job"}},
		}},
		{"on group_left", `a * on (job) group_left (team) b`, []Ref{
			{Metric: "a", Grouping: []string{"job"}},
			{Metric: "b", Grouping: []string{"job", "team"}},
		}},
		{"group_right", `a * on (job) group_right (team) b`, []Ref{
			{Metric: "a", Grouping: []string{"job", "team"}},
			{Metric: "b", Grouping: []string{"job"}},
		}},
		{"ignoring", `a / ignoring (code) b`, []Ref{
			{Metric: "a", Without: []string{"code"}, AllLabels: true},
			{Metric: "b", Without: []string{"code"}, AllLabels: true},
		}},
		{"default match", `a / b`, []Ref{{Metric: "a", AllLabels: true}, {Metric: "b", AllLabels: true}}},
		{"unless", `a unless b`, []Ref{{Metric: "a", AllLabels: true}, {Metric: "b", AllLabels: true}}},
		{"and on", `a and on (job) b`, []Ref{{Metric: "a", Grouping: []string{"job"}}, {Metric: "b", Grouping: []string{"job"}}}},
		{"scalar comparison", `a > 5`, []Ref{{Metric: "a"}}},
		{"vector and scalar()", `a / scalar(b)`, []Ref{{Metric: "a"}, {Metric: "b"}}},
		{"subquery", `max_over_time(rate(x[1m])[1h:5m])`, []Ref{{Metric: "x", Subquery: true}}},
		{"label_replace", `sum by (svc) (label_replace(x, "svc", "$1", "service", "(.*)"))`, []Ref{{
			Metric: "x", Grouping: []string{"service", "svc"}, Created: []string{"svc"},
		}}},
		{"label_join", `label_join(x, "id", "-", "a", "b")`, []Ref{{Metric: "x", Grouping: []string{"a", "b"}, Created: []string{"id"}}}},
		{"count_values", `count_values("version", build_info)`, []Ref{{Metric: "build_info", Created: []string{"version"}}}},
		{"histogram_quantile", `histogram_quantile(0.99, sum by (le) (rate(h_bucket[5m])))`, []Ref{{Metric: "h_bucket", Grouping: []string{"le"}}}},
		{"histogram_quantile adds le", `histogram_quantile(0.9, rate(h_bucket[5m]))`, []Ref{{Metric: "h_bucket", Grouping: []string{"le"}}}},
		{"info default", `info(rate(http_requests_total[5m]))`, []Ref{
			{Metric: "http_requests_total", Grouping: []string{"instance", "job"}},
			{Metric: "target_info", Grouping: []string{"instance", "job"}, AllLabels: true},
		}},
		{"info data-label selector", `info(up, {k8s_cluster_name=~".+"})`, []Ref{
			{Metric: "up", Grouping: []string{"instance", "job"}},
			{Metric: "target_info", Matchers: []Matcher{re("k8s_cluster_name", ".+")}, Grouping: []string{"instance", "job"}},
		}},
		{"info other metric", `info(up, {__name__="build_info", version=~".+"})`, []Ref{
			{Metric: "up", Grouping: []string{"instance", "job"}},
			{Metric: "build_info", Matchers: []Matcher{re("version", ".+")}, Grouping: []string{"instance", "job"}},
		}},
		{"offset and @", `x offset 1h + x @ 1700000000`, []Ref{{Metric: "x", AllLabels: true}, {Metric: "x", AllLabels: true}}},
		{"recording rule name", `job:http_requests:rate5m{job="a"}`, []Ref{{Metric: "job:http_requests:rate5m", Matchers: []Matcher{eq("job", "a")}}}},

		// Grafana variables.
		{"rate interval not assumed", `rate(x[$__rate_interval])`, []Ref{{Metric: "x"}}},
		{"range vars", `increase(x[$__range]) / $__range_s`, []Ref{{Metric: "x"}}},
		{"interval in subquery", `max_over_time(x[$__range:$__interval])`, []Ref{{Metric: "x", Subquery: true}}},
		{"user var range", `rate(x[$window] offset $shift)`, []Ref{{Metric: "x"}}},
		{"matcher value var", `x{job="$job", instance=~"${instance:pipe}"}`, []Ref{{
			Metric: "x",
			Matchers: []Matcher{
				{Label: "job", Op: MatchEqual, Value: "$job", Variable: true},
				{Label: "instance", Op: MatchRegexp, Value: "${instance:pipe}", Variable: true},
			},
		}}},
		{"bracket var", `x{job="[[job]]"}`, []Ref{{
			Metric: "x", Matchers: []Matcher{{Label: "job", Op: MatchEqual, Value: "[[job]]", Variable: true}},
		}}},
		{"bracket var as whole query", `[[metric]]`, []Ref{{
			Matchers: []Matcher{{Label: "__name__", Op: MatchEqual, Value: "[[metric]]", Variable: true}},
			Assumed:  true, Reasons: []string{"metric name [[metric]] comes from variable [[metric]]"},
		}}},
		{"bracket var grouping", `sum by ([[label]]) (up)`, []Ref{{
			Metric: "up", AllLabels: true, Assumed: true,
			Reasons: []string{"grouping label [[label]] comes from variable [[label]]"},
		}}},
		{"bracket var as range", `rate(x[[[win]]])`, []Ref{{Metric: "x"}}},
		{"label name var", `x{$label="a"}`, []Ref{{
			Metric: "x", AllLabels: true, Assumed: true,
			Reasons: []string{"matcher label $label comes from variable $label"},
		}}},
		{"metric name var", `rate(${prefix}_requests_total[5m])`, []Ref{{
			Matchers: []Matcher{{Label: "__name__", Op: MatchEqual, Value: "${prefix}_requests_total", Variable: true}},
			Assumed:  true, Reasons: []string{"metric name ${prefix}_requests_total comes from variable ${prefix}"},
		}}},
		{"empty-able prefix", `${prefix}requests_total`, []Ref{{
			Matchers: []Matcher{{Label: "__name__", Op: MatchEqual, Value: "${prefix}requests_total", Variable: true}},
			Assumed:  true, Reasons: []string{"metric name ${prefix}requests_total comes from variable ${prefix}"},
		}}},
		{"empty-able suffix", `node_cpu$suffix`, []Ref{{
			Matchers: []Matcher{{Label: "__name__", Op: MatchEqual, Value: "node_cpu$suffix", Variable: true}},
			Assumed:  true, Reasons: []string{"metric name node_cpu$suffix comes from variable $suffix"},
		}}},
		{"grouping var", `sum by ($group) (x)`, []Ref{{
			Metric: "x", AllLabels: true, Assumed: true,
			Reasons: []string{"grouping label $group comes from variable $group"},
		}}},
		{"label_replace $1 is not a var", `label_replace(x, "a", "$1", "b", "(.*)")`, []Ref{{Metric: "x", Grouping: []string{"b"}, Created: []string{"a"}}}},
		{"apostrophe in a comment", "# don't drop\nsum by (job) (x{a=\"$a\"})", []Ref{{
			Metric: "x", Grouping: []string{"job"}, Matchers: []Matcher{{Label: "a", Op: MatchEqual, Value: "$a", Variable: true}},
		}}},
		{"regex dollar", `x{path=~"/api$"}`, []Ref{{Metric: "x", Matchers: []Matcher{re("path", "/api$")}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PromQL(tt.query)
			if err != nil {
				t.Fatalf("PromQL(%q): %v", tt.query, err)
			}
			if !reflect.DeepEqual(clearResultLabels(got), tt.want) {
				t.Errorf("PromQL(%q)\n got %+v\nwant %+v", tt.query, got, tt.want)
			}
		})
	}
}

func TestPromQLEmpty(t *testing.T) {
	refs, err := PromQL("  \n")
	if err != nil || refs != nil {
		t.Fatalf("empty query: %v, %v", refs, err)
	}
}

// A query that doesn't parse references every metric it could name,
// assumed, never nothing.
func TestPromQLParseFailure(t *testing.T) {
	tests := []struct {
		query   string
		metrics []string
	}{
		{`sum(rate(http_requests_total{job="a"}[5m]) by (code)`, []string{"http_requests_total"}},
		{`a + on(job) group_left(team) b +`, []string{"a", "b"}},
		{`rate(foo[5m]) / bar{x="y" ) unless baz`, []string{"foo", "bar", "x", "unless", "baz"}}, // unclosed {: label names and keywords count too,
		{`$var +`, nil},
		{`{__name__="up"} + down +`, []string{"up", "down"}},
		{`foo{__name__="bar"} +`, []string{"foo"}},
		{`range + up +`, []string{"range", "up"}},
		{`start * end offset 5m +`, []string{"start", "end", "offset"}},
		{`a + on (job) group_left (t) b > bool 1 +`, []string{"a", "b"}},
		{`)`, nil},
	}
	for _, tt := range tests {
		refs, err := PromQL(tt.query)
		if err == nil {
			t.Fatalf("%q parsed", tt.query)
		}
		if len(refs) == 0 {
			t.Fatalf("%q: parse failure returned no refs", tt.query)
		}
		var got []string
		for _, r := range refs {
			if !r.Assumed || !r.AllLabels || len(r.Reasons) == 0 || !strings.Contains(strings.Join(r.Reasons, ";"), "did not parse") {
				t.Errorf("%q: ref %+v not marked assumed/all labels", tt.query, r)
			}
			if r.Metric != "" {
				got = append(got, r.Metric)
			} else if len(r.Matchers) != 1 || r.Matchers[0].Label != "__name__" {
				t.Errorf("%q: nameless ref without a __name__ matcher: %+v", tt.query, r)
			}
		}
		if !reflect.DeepEqual(got, tt.metrics) {
			t.Errorf("%q: metrics %v, want %v", tt.query, got, tt.metrics)
		}
	}
}

// Selectors that name no metric, or name it by regex, in a query that
// doesn't parse.
func TestPromQLParseFailureSelectors(t *testing.T) {
	tests := []struct {
		query string
		want  []string // metric, or "re:<regex>" for a __name__ matcher
	}{
		{`{job="api"} + down +`, []string{"re:.+", "down"}},
		{`{__name__=~"node_.+"} + down +`, []string{"re:node_.+", "down"}},
		{`{__name__!="up"} +`, []string{"re:.+"}},
		{`{job="a"} + {job="b"} +`, []string{"re:.+"}},
		{`sum(x{job="a"}) +`, []string{"x"}},
		{`a and {job="b"} +`, []string{"a", "and", "re:.+"}},
	}
	for _, tt := range tests {
		refs, err := PromQL(tt.query)
		if err == nil {
			t.Fatalf("%q parsed", tt.query)
		}
		var got []string
		for _, r := range refs {
			if r.Metric != "" {
				got = append(got, r.Metric)
			} else {
				got = append(got, "re:"+r.Matchers[0].Value)
			}
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestSubstituteGrafanaVars(t *testing.T) {
	tests := []struct{ in, want string }{
		{`rate(x[$__rate_interval])`, `rate(x[5m])`},
		{`x offset $shift`, `x offset 5m`},
		{`x offset -$shift`, `x offset -5m`},
		{`x @ ${__to:date:seconds}`, `x @ 0`},
		{`x{a="$a"}`, `x{a="__tvtvar0__"}`},
		{`$m{a="[[b:csv]]"}`, `__tvtvar0__{a="__tvtvar1__"}`},
		{`[[metric]]`, `__tvtvar0__`},
		{`rate(x[[[win]]])`, `rate(x[5m])`},
		{`label_replace(x, "a", "$1", "b", "(.*)")`, `label_replace(x, "a", "$1", "b", "(.*)")`},
		{`my_offset_$var`, `my_offset___tvtvar0__`},
	}
	for _, tt := range tests {
		got, _ := substituteGrafanaVars(tt.in)
		if got != tt.want {
			t.Errorf("substitute(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// clearResultLabels drops ResultLabels so the extractor tables can leave
// it out; TestPromQLResultLabels and TestLogQLResultLabels cover it.
func clearResultLabels(refs []Ref) []Ref {
	out := slices.Clone(refs)
	for i := range out {
		out[i].ResultLabels = false
	}
	return out
}

func TestPromQLResultLabels(t *testing.T) {
	tests := []struct {
		query string
		want  map[string]bool // metric -> ResultLabels
	}{
		{`up{job="shop"}`, map[string]bool{"up": true}},
		{`rate(up[5m])`, map[string]bool{"up": true}},
		{`sum by (job) (up)`, map[string]bool{"up": false}},
		{`sum without (instance) (up)`, map[string]bool{"up": true}},
		{`topk(3, up)`, map[string]bool{"up": true}},
		{`topk(scalar(k), up)`, map[string]bool{"up": true, "k": false}},
		{`histogram_quantile(0.9, rate(b[5m]))`, map[string]bool{"b": true}},
		{`label_replace(up, "a", "$1", "b", "(.*)")`, map[string]bool{"up": true}},
		{`a / b`, map[string]bool{"a": true, "b": false}},
		{`a / ignoring(x) b`, map[string]bool{"a": true, "b": false}},
		{`a / on(job) b`, map[string]bool{"a": false, "b": false}},
		{`a * on(job) group_left(team) b`, map[string]bool{"a": true, "b": false}},
		{`a * on(job) group_right(team) b`, map[string]bool{"a": false, "b": true}},
		{`a and b`, map[string]bool{"a": true, "b": false}},
		{`a unless on(job) b`, map[string]bool{"a": true, "b": false}},
		{`a or b`, map[string]bool{"a": true, "b": true}},
		{`a > 0.5`, map[string]bool{"a": true}},
		{`absent(up{job="x"})`, map[string]bool{"up": false}},
		{`scalar(up)`, map[string]bool{"up": false}},
		{`max_over_time(up[1h:5m])`, map[string]bool{"up": true}},
		{`sum(rate(a[5m])) / sum(rate(b[5m]))`, map[string]bool{"a": false, "b": false}},
		{`info(up)`, map[string]bool{"up": true, "target_info": true}},
	}
	for _, tt := range tests {
		refs, err := PromQL(tt.query)
		if err != nil {
			t.Fatalf("%s: %v", tt.query, err)
		}
		got := map[string]bool{}
		for _, r := range refs {
			got[r.Metric] = r.ResultLabels
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: ResultLabels %v, want %v", tt.query, got, tt.want)
		}
	}
}
