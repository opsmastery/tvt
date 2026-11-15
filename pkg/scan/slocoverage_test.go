package scan

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func TestMultiwindow(t *testing.T) {
	for expr, want := range map[string]bool{
		// Sloth's page alert shape, hand-written.
		"(sum(rate(e[1h])) / sum(rate(t[1h])) > (14.4 * 0.001))\nand\n(sum(rate(e[5m])) / sum(rate(t[5m])) > (14.4 * 0.001))": true,
		"(x[1h] > 1) and ignoring(window) (x[5m] > 1)":         true,
		"(x[1h] > 1) and on(job) (x[5m] > 1) and (x[30m] > 1)": true,
		"(x[5m] > 1) and (x[5m] > 1)":                          false, // one window
		"(x[1h] > 1) and (y[5m] > 1)":                          false, // different ratios
		"rate(x[1h]) > 1":                                      false,
		`count_over_time({a="b"} |= "and" [1h]) > 1`:           false,
		`(x{a="(and)"}[1h] > 1) and (x{a="(and)"}[5m] > 1)`:    true,
		"(sum(x[1h]) > 1) and (sum(x[5m]) > 2)":                false, // thresholds differ: not one ratio
	} {
		if got := multiwindow(expr); got != want {
			t.Errorf("multiwindow(%q) = %v, want %v", expr, got, want)
		}
	}
}

func TestSLORule(t *testing.T) {
	slos := []collect.SLO{{Format: "sloth", Name: "shop/a", Service: "shop", Short: "a", Alert: "ShopABurn"}}
	for _, c := range []struct {
		r    collect.Rule
		want bool
	}{
		{collect.Rule{Kind: collect.RuleAlert, Name: "ShopABurn", Expr: "x > 1"}, true},
		{collect.Rule{Kind: collect.RuleRecord, Name: "ShopABurn", Expr: "x"}, false}, // a recording rule isn't the alert
		{collect.Rule{Kind: collect.RuleAlert, Name: "X", Expr: "x > 1", Labels: map[string]string{"sloth_slo": "a"}}, true},
		{collect.Rule{Kind: collect.RuleAlert, Name: "X", Expr: `slo:sli_error:ratio_rate5m{sloth_id="a"} > 0.0144`}, true},
		{collect.Rule{Kind: collect.RuleRecord, Name: "slo:sli_error:ratio_rate6h", Expr: "sum_over_time(x[6h])"}, true},
		{collect.Rule{Kind: collect.RuleAlert, Name: "X", Expr: "http_requests:burnrate1h > 14"}, true},
		{collect.Rule{Kind: collect.RuleAlert, Name: "X", Expr: "rate(x[5m]) > 1", Labels: map[string]string{"severity": "critical"}}, false},
	} {
		if got := sloRule(c.r, slos); got != c.want {
			t.Errorf("sloRule(%+v) = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestPaging(t *testing.T) {
	for labels, want := range map[string]bool{
		"severity=critical": true, "severity=Critical": true, "severity=page": true, "priority=P1": true,
		"page=true": true, "severity=warning": false, "": false, "severity=info": false,
	} {
		r := collect.Rule{Kind: collect.RuleAlert, Labels: map[string]string{}}
		if k, v, ok := strings.Cut(labels, "="); ok {
			r.Labels[k] = v
		}
		if got := paging(r); got != want {
			t.Errorf("paging(%s) = %v, want %v", labels, got, want)
		}
	}
	if paging(collect.Rule{Kind: collect.RuleRecord, Labels: map[string]string{"severity": "critical"}}) {
		t.Error("a recording rule doesn't page")
	}
}

// TestSLOCoverageCases covers what the lab doesn't: a service covered only
// through a signal it owns, a Pyrra SLO alerted by its generated alert's
// slo label, an SLO alerted by an unnamed burn-rate alert on its signals,
// and a burn-rate alert exempt from long_range_window.
func TestSLOCoverageCases(t *testing.T) {
	in := baseInputs()
	in.SLOsConfigured = true
	in.Owners = []config.Owner{
		{Name: "web", Metrics: []string{"shop_http_.*"}},
		{Name: "nodes", Metrics: []string{"node_.*"}},
		{Name: "cart", Metrics: []string{"shop_cart_.*"}},
	}
	in.SLOs = []collect.SLO{
		// No service named, but reads web's metric.
		{Format: "openslo", Name: "availability", Short: "availability", Queries: []collect.SLOQuery{{Expr: `sum(rate(shop_http_requests_total{code=~"5.."}[5m]))`, Language: "promql"}}},
		{Format: "pyrra", Name: "monitoring/nodes-up", Short: "nodes-up", Alert: "ErrorBudgetBurn", Queries: []collect.SLOQuery{{Expr: "sum(rate(node_cpu_seconds_total[5m]))", Language: "promql"}}},
		{Format: "pyrra", Name: "monitoring/cart", Short: "cart", Alert: "ErrorBudgetBurn", Queries: []collect.SLOQuery{{Expr: "sum(rate(shop_cart_operations_total[5m]))", Language: "promql"}}},
	}
	in.Rules = append(in.Rules,
		collect.Rule{Source: "prometheus", Group: "slo", Name: "WebBurn", Kind: collect.RuleAlert, Language: "promql", Interval: 30 * time.Second,
			Labels: map[string]string{"severity": "critical"},
			Expr:   `(sum(rate(shop_http_requests_total{code=~"5.."}[1h])) > 0.0144) and (sum(rate(shop_http_requests_total{code=~"5.."}[5m])) > 0.0144)`},
		collect.Rule{Source: "prometheus", Group: "slo", Name: "ErrorBudgetBurn", Kind: collect.RuleAlert, Language: "promql", Interval: time.Minute,
			Labels: map[string]string{"severity": "critical", "slo": "nodes-up"}, Expr: "node_cpu_seconds_total:burnrate5m > 14"},
		collect.Rule{Source: "prometheus", Group: "x", Name: "NodeLoad", Kind: collect.RuleAlert, Language: "promql", Interval: time.Minute,
			Labels: map[string]string{"severity": "page"}, Expr: "sum(rate(node_cpu_seconds_total[5m])) > 4"},
	)
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, f := range out.SLOFindings {
		got[f.Kind] = append(got[f.Kind], f.Name)
		if f.Detail == "" || f.Fix == "" || f.Subject == "" {
			t.Errorf("%+v: detail, fix and subject are required", f)
		}
	}
	if len(got[graph.SLOServiceWithout]) != 0 {
		t.Errorf("services without SLO %v: web is covered through its metric, nodes and cart by Pyrra", got[graph.SLOServiceWithout])
	}
	if !slices.Equal(got[graph.SLOPagingNotBurnRate], []string{"NodeLoad"}) {
		t.Errorf("paging not burn-rate %v, want [NodeLoad]", got[graph.SLOPagingNotBurnRate])
	}
	// cart's ErrorBudgetBurn would carry slo=cart; the one loaded is nodes-up's.
	if !slices.Equal(got[graph.SLOWithoutAlert], []string{"monitoring/cart"}) {
		t.Errorf("SLOs without alert %v, want [monitoring/cart]", got[graph.SLOWithoutAlert])
	}
	for _, f := range out.SLOFindings {
		if f.Kind == graph.SLOPagingNotBurnRate && !strings.Contains(f.Detail, "SLO monitoring/nodes-up reads the same signals") {
			t.Errorf("NodeLoad detail %q", f.Detail)
		}
	}
	for _, f := range out.RuleFindings {
		if f.Kind == graph.RuleLongRange && strings.HasSuffix(f.ConsumerID, "/WebBurn") {
			t.Errorf("a burn-rate alert's 1h window is a finding: %+v", f)
		}
	}

	// Without owners: no service findings, and the gap says why.
	in.Owners = nil
	out, err = Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range out.Sources {
		if s.Source == SourceSLOs && (!strings.Contains(s.Gap, "no ownership.owners") || s.Coverage != 1) {
			t.Errorf("slo source %+v", s)
		}
	}
}

func FuzzSplitAnd(f *testing.F) {
	for _, s := range []string{"(a)and(b)", `(x{a="and"})and on(j)(y)`, "and", "(a)and", "((", `"\`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, p := range splitAnd(s) {
			if len(p) > len(s) {
				t.Fatalf("part %q longer than %q", p, s)
			}
		}
		_ = multiwindow(s)
	})
}
