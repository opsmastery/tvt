package parse

import (
	"reflect"
	"strings"
	"testing"
)

// The stopgap's cases: stream selectors only, canonical form.
func TestLogQLSelectors(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  []string // canonical selectors
	}{
		{`sum(count_over_time({app="shop", component="audit"} |= "result=denied" [5m])) > 20`,
			[]string{`{app="shop", component="audit"}`}},
		{`{component="audit",app="shop"}`, []string{`{app="shop", component="audit"}`}},
		{"{app=~`shop|cart`, level!=\"debug\"}", []string{`{app=~"shop|cart", level!="debug"}`}},
		{`{app="shop"} | line_format "{{.msg}} {not=\"a selector\"}"`, []string{`{app="shop"}`}},
		{`sum by (app) (rate({app="a"}[1m])) / sum(rate({app!~"b"}[1m]))`,
			[]string{`{app="a"}`, `{app!~"b"}`}},
		{"# {commented=\"out\"}\n{app=\"shop\"}", []string{`{app="shop"}`}},
		{`{ app = "a\"b" }`, []string{`{app="a\"b"}`}},
	} {
		refs, err := LogQL(tc.query)
		if err != nil {
			t.Errorf("%s: %v", tc.query, err)
			continue
		}
		var got []string
		for _, r := range refs {
			got = append(got, CanonicalSelector(r.Matchers))
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.query, got, tc.want)
		}
	}
}

type logqlCase struct {
	name  string
	query string
	want  []Ref
}

// logqlTableQueries returns the queries in TestLogQL, for other tests.
func logqlTableQueries() []string {
	var qs []string
	for _, c := range logqlCases() {
		qs = append(qs, c.query)
	}
	return qs
}

func TestLogQL(t *testing.T) {
	for _, tt := range logqlCases() {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LogQL(tt.query)
			if err != nil {
				t.Fatalf("LogQL(%q): %v", tt.query, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LogQL(%q)\n got %+v\nwant %+v", tt.query, got, tt.want)
			}
		})
	}
}

func logqlCases() []logqlCase {
	eq := func(l, v string) Matcher { return Matcher{Label: l, Op: MatchEqual, Value: v} }
	app := []Matcher{eq("app", "shop")}
	return []logqlCase{
		{"bare selector", `{app="shop"}`, []Ref{{Matchers: app, StreamLabels: []string{"app"}}}},
		{"line filters read no labels", "{app=\"shop\"} |= \"error\" != `debug` |~ \"5..\" or \"timeout\" |= ip(\"10.0.0.0/8\")",
			[]Ref{{Matchers: app, StreamLabels: []string{"app"}}}},
		{"label filter without a parser is a stream label", `{app="shop"} | level="error"`, []Ref{{
			Matchers: app, Filters: []string{"level"}, StreamLabels: []string{"app", "level"},
		}}},
		{"logfmt: everything after is maybe extracted", `{app="shop"} | logfmt | status >= 500 and duration > 1.5s`, []Ref{{
			Matchers: app, Filters: []string{"duration", "status"},
			StreamLabels: []string{"app"}, Extracted: []string{"duration", "status"},
		}}},
		{"filter before the parser stays a stream label", `{app="shop"} | env="prod" | json | code="500"`, []Ref{{
			Matchers: app, Filters: []string{"code", "env"},
			StreamLabels: []string{"app", "env"}, Extracted: []string{"code"},
		}}},
		{"json params declare names", `{app="shop"} | json code="response.code", route | code="500" | team="a"`, []Ref{{
			Matchers: app, Filters: []string{"code", "team"}, Created: []string{"code", "route"},
			StreamLabels: []string{"app", "team"}, Extracted: []string{"code"},
		}}},
		{"regexp named groups", "{app=\"shop\"} | regexp `(?P<method>\\w+) (?P<path>\\S+)` | method=\"GET\"", []Ref{{
			Matchers: app, Filters: []string{"method"}, Created: []string{"method", "path"},
			StreamLabels: []string{"app"}, Extracted: []string{"method"},
		}}},
		{"pattern captures", `sum by (status) (count_over_time({app="shop"} | pattern "<ip> - <_> \"<method> <_>\" <status>" [5m]))`, []Ref{{
			Matchers: app, Grouping: []string{"status"}, Created: []string{"ip", "method", "status"},
			StreamLabels: []string{"app"}, Extracted: []string{"status"},
		}}},
		{"unpack", `{app="shop"} | unpack | pod=~"web.*"`, []Ref{{
			Matchers: app, Filters: []string{"pod"}, StreamLabels: []string{"app"}, Extracted: []string{"pod"},
		}}},
		{"_extracted suffix", `sum by (level_extracted) (count_over_time({app="shop"} | json code="c" [1m]))`, []Ref{{
			Matchers: app, Grouping: []string{"level_extracted"}, Created: []string{"code"},
			StreamLabels: []string{"app"}, Extracted: []string{"level_extracted"},
		}}},
		{"line_format template", `{app="shop"} | line_format "{{.level}} {{ .msg | ToUpper }} {{ __line__ }}"`, []Ref{{
			Matchers: app, Filters: []string{"level", "msg"}, StreamLabels: []string{"app", "level", "msg"},
		}}},
		{"label_format rename and template", `sum by (lvl, who) (count_over_time({app="shop"} | label_format lvl=level, who="{{.user}}" [5m]))`, []Ref{{
			Matchers: app, Grouping: []string{"lvl", "who"}, Created: []string{"lvl", "who"},
			Filters: []string{"level", "user"}, StreamLabels: []string{"app", "level", "user"},
		}}},
		{"keep and drop", `{app="shop"} | keep app, level | drop __error__, pod="x"`, []Ref{{
			Matchers: app, Filters: []string{"app", "level", "pod"}, StreamLabels: []string{"app", "level", "pod"},
		}}},
		{"unwrap with conversion", `sum_over_time({app="shop"} | logfmt | unwrap bytes(size) | __error__="" [5m]) by (route)`, []Ref{{
			Matchers: app, Grouping: []string{"route"}, Filters: []string{"size"},
			StreamLabels: []string{"app"}, Extracted: []string{"route", "size"},
		}}},
		{"quantile_over_time with trailing by", `quantile_over_time(0.99, {app="shop"} | json | unwrap latency [5m]) by (route)`, []Ref{{
			Matchers: app, Grouping: []string{"route"}, Filters: []string{"latency"},
			StreamLabels: []string{"app"}, Extracted: []string{"latency", "route"},
		}}},
		{"range before pipeline and offset", `count_over_time({app="shop"}[5m] |= "x" offset 1h)`, []Ref{{Matchers: app, StreamLabels: []string{"app"}}}},
		{"bytes_rate stands in", `sum(bytes_rate({app="shop"}[1m]))`, []Ref{{Matchers: app, StreamLabels: []string{"app"}}}},
		{"sum without", `sum without (pod) (rate({app="shop"}[1m]))`, []Ref{{
			Matchers: app, Without: []string{"pod"}, StreamLabels: []string{"app", "pod"},
		}}},
		{"binary on two log queries", `sum by (app) (rate({app="shop"} |= "error" [5m])) / on (app) sum by (app) (rate({app="shop"}[5m]))`, []Ref{
			{Matchers: app, Grouping: []string{"app"}, StreamLabels: []string{"app"}},
			{Matchers: app, Grouping: []string{"app"}, StreamLabels: []string{"app"}},
		}},
		{"default vector match", `rate({app="a"}[1m]) / rate({app="b"}[1m])`, []Ref{
			{Matchers: []Matcher{eq("app", "a")}, AllLabels: true, StreamLabels: []string{"app"}},
			{Matchers: []Matcher{eq("app", "b")}, AllLabels: true, StreamLabels: []string{"app"}},
		}},
		{"label_replace output is not a stream label", `sum by (svc) (label_replace(rate({app="shop"}[1m]), "svc", "$1", "service", "(.*)"))`, []Ref{{
			Matchers: app, Grouping: []string{"service", "svc"}, Created: []string{"svc"}, StreamLabels: []string{"app", "service"},
		}}},
		{"topk", `topk(5, sum by (pod) (rate({app="shop"}[1m])))`, []Ref{{
			Matchers: app, Grouping: []string{"pod"}, StreamLabels: []string{"app", "pod"},
		}}},
		{"or vector(0) after sum", `sum(count_over_time({app="shop"}[5m])) or vector(0)`, []Ref{{Matchers: app, StreamLabels: []string{"app"}}}},
		{"parenthesised label filter", `{app="shop"} | (level="error" or level="warn"), status=~"5.." | logfmt`, []Ref{{
			Matchers: app, Filters: []string{"level", "status"}, StreamLabels: []string{"app", "level", "status"},
		}}},
		{"comparison values", `{app="shop"} | logfmt | size > 20MB and latency <= 250ms or retries == -1`, []Ref{{
			Matchers: app, Filters: []string{"latency", "retries", "size"},
			StreamLabels: []string{"app"}, Extracted: []string{"latency", "retries", "size"},
		}}},
		{"logfmt flags", `{app="shop"} | logfmt --strict --keep-empty | level="error"`, []Ref{{
			Matchers: app, Filters: []string{"level"}, StreamLabels: []string{"app"}, Extracted: []string{"level"},
		}}},

		{"apostrophe in a comment", "# don't page on this\nsum(count_over_time({app=\"shop\"}[5m]))", []Ref{{Matchers: app, StreamLabels: []string{"app"}}}},
		{"comments between stages and matchers", "{app=\"shop\" # the app\n} # errors only\n|= \"error\"\n# then parse\n| logfmt | level=\"x\"", []Ref{{
			Matchers: app, Filters: []string{"level"}, StreamLabels: []string{"app"}, Extracted: []string{"level"},
		}}},

		// Grafana variables.
		{"matcher value var", `sum(count_over_time({app="$app", env=~"${env:regex}"} [$__auto]))`, []Ref{{
			Matchers: []Matcher{
				{Label: "app", Op: MatchEqual, Value: "$app", Variable: true},
				{Label: "env", Op: MatchRegexp, Value: "${env:regex}", Variable: true},
			},
			StreamLabels: []string{"app", "env"},
		}}},
		{"grouping var", `sum by ($by) (rate({app="shop"}[$__interval]))`, []Ref{{
			Matchers: app, StreamLabels: []string{"app"}, AllLabels: true, Assumed: true,
			Reasons: []string{"grouping label $by comes from variable $by"},
		}}},
		{"filter label var", `{app="shop"} | $field="x"`, []Ref{{
			Matchers: app, StreamLabels: []string{"app"}, AllLabels: true, Assumed: true,
			Reasons: []string{"label $field comes from variable $field"},
		}}},
	}
}

func TestLogQLEmpty(t *testing.T) {
	if refs, err := LogQL(" \n"); err != nil || refs != nil {
		t.Fatalf("empty query: %v, %v", refs, err)
	}
}

// A query that doesn't parse references every selector it holds,
// assumed and reading every label; with none, every stream.
func TestLogQLParseFailure(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  []string // canonical selectors; "" is every stream
	}{
		{``[0:0] + `count_over_time([5m])`, []string{""}},
		{`{}`, []string{""}},
		{`{app}`, []string{""}},
		{`{app="shop"`, []string{""}},
		{`{app=shop}`, []string{""}},
		{`{app="shop" component="x"}`, []string{""}},
		{`{app="shop"} |= "unterminated`, []string{`{app="shop"}`}},
		{`{1app="x"}`, []string{""}},
		{`app="x"}`, []string{""}},
		{`up`, []string{""}},
		{`{app="shop"} | json | unknown_stage foo`, []string{`{app="shop"}`}},
		{`sum(rate({app="a"}[1m]) by (x)`, []string{`{app="a"}`}},
		{`{app="a"} |`, []string{`{app="a"}`}},
		{`{app="a"} != ip(")`, []string{`{app="a"}`}},
		{`count_over_time({app="a"}[="5m])`, []string{`{app="a"}`}},
		{`{app="a"} | addr = ip("10.0.0.0/8"`, []string{`{app="a"}`}},
		{`rate({app="a"}[1m]) + rate({app="b"}[1m]) +`, []string{`{app="a"}`, `{app="b"}`}},
	} {
		refs, err := LogQL(tc.query)
		if err == nil {
			t.Errorf("%q parsed: %+v", tc.query, refs)
			continue
		}
		var got []string
		for _, r := range refs {
			if !r.Assumed || !r.AllLabels || !strings.Contains(strings.Join(r.Reasons, ";"), "did not parse") {
				t.Errorf("%q: ref %+v not assumed/all labels", tc.query, r)
			}
			if len(r.Matchers) == 0 {
				got = append(got, "")
			} else {
				got = append(got, CanonicalSelector(r.Matchers))
			}
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %q, want %q", tc.query, got, tc.want)
		}
	}
}
