package scan

import (
	"fmt"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/parse"
)

func TestSelectorKey(t *testing.T) {
	m := func(l string, op parse.MatchOp, v string) parse.Matcher {
		return parse.Matcher{Label: l, Op: op, Value: v}
	}
	for _, tc := range []struct {
		ms   []parse.Matcher
		key  string
		want bool
	}{
		// The Asserts shape: sorted, __name__ left out.
		{[]parse.Matcher{m("asserts_metric_request", "=", "total"), m("asserts_env", "!=", ""), m("__name__", "!~", "ALERTS.*")},
			`{asserts_env!="",asserts_metric_request="total"}`, true},
		{[]parse.Matcher{m("job", "=~", "shop|node")}, `{job=~"shop|node"}`, true},
		// Only matchers that accept the empty value: Prometheus refuses these.
		{[]parse.Matcher{m("job", "=~", ".*")}, "", false},
		{[]parse.Matcher{m("job", "!=", "x"), m("env", "=", "")}, "", false},
		{[]parse.Matcher{m("__name__", "=~", "node_.*")}, "", false},
		// A variable can't be sent; what's left must still exclude empty.
		{[]parse.Matcher{{Label: "job", Op: "=", Value: "$job", Variable: true}}, "", false},
		{[]parse.Matcher{{Label: "job", Op: "=", Value: "$job", Variable: true}, m("team", "=", "pay")}, `{team="pay"}`, true},
		{[]parse.Matcher{m("job", "=~", "(")}, "", false}, // invalid regex: treated as matching empty
	} {
		key, ok := SelectorKey(tc.ms)
		if key != tc.key || ok != tc.want {
			t.Errorf("SelectorKey(%v) = %q, %v; want %q, %v", tc.ms, key, ok, tc.key, tc.want)
		}
	}
}

func TestLabelOnlySelectors(t *testing.T) {
	got := LabelOnlySelectors([]string{
		`sum by (job) (rate({asserts_metric_request="total", asserts_env!=""}[5m]))`,
		`rate({asserts_env!="", asserts_metric_request="total"}[1m])`, // same selector, other order
		`up{job="shop"}`,           // names its metric
		`{__name__=~"node_.+"}`,    // names metrics by __name__ only
		`rate({team="pay"}[5m]) +`, // doesn't parse
	})
	want := []string{`{asserts_env!="",asserts_metric_request="total"}`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A rule selecting by label protects the metrics the backend says carry
// the label, read exactly; without the backend's answer it still reads
// every metric, assumed.
func TestLabelOnlySelectorNarrowed(t *testing.T) {
	rule := collect.Rule{Source: "prometheus", Namespace: "kg.yml", Group: "kg", Name: "kg:requests:rate5m", Kind: collect.RuleRecord, Language: "promql",
		Expr: `sum(rate({asserts_metric_request="total", __name__!~"ALERTS.*|.+:.+"}[5m]))`}
	id := "rule:prometheus:kg.yml/kg/kg:requests:rate5m"
	sel := `{asserts_metric_request="total"}`

	in := baseInputs()
	in.Rules = append(in.Rules, rule)
	in.SelectorMetrics = map[string][]string{sel: {"shop_http_requests_total", "ALERTS", "route:shop_http_request_duration_seconds:p99_5m"}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for k, r := range refIndex(out) {
		if strings.HasSuffix(k, "<- "+id) && strings.Contains(k, ":metric:") {
			got = append(got, k)
			if r.Assumed {
				t.Errorf("%s is assumed; the backend resolved the selector", k)
			}
		}
	}
	// The __name__ matcher still excludes ALERTS and the recorded series.
	if want := []string{"prom:metric:shop_http_requests_total <- " + id}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("refs %v, want %v", got, want)
	}

	in.SelectorMetrics = nil
	out, err = Build(in)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for k := range refIndex(out) {
		if strings.HasSuffix(k, "<- "+id) && strings.Contains(k, ":metric:") {
			n++
		}
	}
	if n < 4 {
		t.Errorf("unresolved selector reads %d metrics, want every collected one but the excluded", n)
	}
}

// A query_result variable without a regex holds result lines, never a
// metric name: used as a value it reads nothing. With a regex it could
// hold anything, so it still reads every metric, assumed.
func TestQueryResultVariableIsAValue(t *testing.T) {
	panelRefs := func(regex string) int {
		in := baseInputs()
		in.Dashboards = []collect.Dashboard{{UID: "billing", Title: "Billing",
			Variables: []collect.Variable{{Name: "partner", Type: "query", Query: "query_result(1-$customer)", DatasourceType: "prometheus", Regex: regex}},
			Panels: []collect.Panel{{ID: 1, Title: "usage", Queries: []collect.PanelQuery{{DatasourceType: "prometheus",
				Expr: "shop_http_requests_total - (9999999999 * $partner) >= 0"}}}},
		}}
		out, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for k := range refIndex(out) {
			if strings.Contains(k, "<- panel:1:billing:") && strings.Contains(k, ":metric:") {
				n++
			}
		}
		return n
	}
	if n := panelRefs(""); n != 1 {
		t.Errorf("without a regex the panel reads %d metrics, want 1 (shop_http_requests_total)", n)
	}
	if n := panelRefs("{} (.*) .*"); n != 1 {
		t.Errorf("with a sample-value regex the panel reads %d metrics, want 1", n)
	}
	if n := panelRefs(`/(\d+)/`); n < 4 {
		t.Errorf("with a regex the panel reads %d metrics, want every metric (assumed)", n)
	}
}

func TestCapturesSampleValue(t *testing.T) {
	for re, want := range map[string]bool{
		"{} (.*) .*":        true,
		"/{} (.*) .*/":      true,
		`.*} (\d+) .*`:      true,
		`{}\s+(.*)\s.*`:     true,
		`/__name__="(.*)"/`: false,
		"(.*)":              false,
		`/(?:x)(.*)/`:       false,
	} {
		if got := capturesSampleValue(re); got != want {
			t.Errorf("capturesSampleValue(%q) = %v, want %v", re, got, want)
		}
	}
}
