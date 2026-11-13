package scan

import (
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// Sprint 2 item 7: SLOs and runbook steps are declared hard keeps.
func TestSLOsAndRunbooks(t *testing.T) {
	in := baseInputs()
	in.Metrics = append(in.Metrics,
		collect.MetricCardinality{Metric: "shop_checkout_attempts_total", ActiveSeries: 2},
		collect.MetricCardinality{Metric: "shop_payment_gateway_retries_total", ActiveSeries: 1},
		collect.MetricCardinality{Metric: "shop_legacy_queue_depth", ActiveSeries: 1},
		collect.MetricCardinality{Metric: "kubectl", ActiveSeries: 1}, // a word a runbook block can hold
	)
	in.SLOsConfigured = true
	in.SLOs = []collect.SLO{
		{Format: "sloth", Name: "shop/checkout", Origin: "slos/shop.yml", Queries: []collect.SLOQuery{
			{Expr: `sum(rate(shop_checkout_attempts_total{outcome="failure"}[5m]))`, Language: "promql"},
			{Expr: `sum(rate(shop_checkout_attempts_total[5m]))`, Language: "promql"},
		}},
		{Format: "sloth", Name: "shop/plugin", Origin: "slos/shop.yml", Unreadable: "Sloth SLI plugin \"x\""},
		// An SLI over a recording rule protects its inputs too.
		{Format: "openslo", Name: "shop/latency", Origin: "slos/o.yaml", Queries: []collect.SLOQuery{
			{Expr: "route:shop_http_request_duration_seconds:p99_5m", Language: "promql"},
		}},
	}
	in.Dashboards = []collect.Dashboard{{UID: "legacy-queue", Title: "Legacy", Panels: []collect.Panel{{ID: 1, Title: "q",
		Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: "shop_legacy_queue_depth"}}}}}}
	in.Runbooks = []collect.Runbook{
		{URL: "https://rb/backlog.md", Alerts: []string{"prometheus/ShopPaymentQueueBacklog"}, Steps: []parse.RunbookStep{
			{Kind: parse.StepCode, Expr: "sum(rate(shop_payment_gateway_retries_total[5m]))", Language: "promql"},
			{Kind: parse.StepCode, Expr: "shop_not_collected_total", Language: "promql"}, // never invents a signal
			{Kind: parse.StepDashboard, DashboardUID: "legacy-queue"},
			{Kind: parse.StepDashboard, DashboardUID: "gone"},
		}},
		{URL: "https://rb/private.md", Alerts: []string{"prometheus/A"}, Err: "fetch: 403 Forbidden"},
		{URL: "https://rb/empty.md", Alerts: []string{"prometheus/B"}},
	}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	sigs := signalIDs(out)
	for id, class := range map[string]string{
		"prom:metric:shop_checkout_attempts_total":       "hard_keep", // SLO only
		"prom:metric:shop_payment_gateway_retries_total": "hard_keep", // runbook code step only
		"prom:metric:shop_legacy_queue_depth":            "hard_keep", // runbook dashboard link
		p99:                                              "hard_keep",
		bucket:                                           "hard_keep",
		"prom:metric:kubectl":                            "candidate",
	} {
		if sigs[id].Class != class {
			t.Errorf("%s: class %q, want %q", id, sigs[id].Class, class)
		}
	}
	if _, ok := sigs["prom:metric:shop_not_collected_total"]; ok {
		t.Error("a runbook code step created a signal for a metric nobody collected")
	}

	kinds := map[string]graph.ConsumerKind{}
	for _, c := range out.Consumers {
		kinds[c.ID] = c.Kind
	}
	for id, kind := range map[string]graph.ConsumerKind{
		"slo:sloth:slos/shop.yml:shop/checkout": graph.ConsumerSLO,
		"slo:sloth:slos/shop.yml:shop/plugin":   graph.ConsumerSLO,
		"runbook:https://rb/backlog.md":         graph.ConsumerRunbook,
		"runbook:https://rb/empty.md":           graph.ConsumerRunbook,
	} {
		if kinds[id] != kind {
			t.Errorf("consumer %s: kind %q, want %q", id, kinds[id], kind)
		}
	}
	if _, ok := kinds["runbook:https://rb/private.md"]; ok {
		t.Error("an unreadable runbook became a consumer")
	}
	declared := 0
	for _, r := range out.References {
		if strings.HasPrefix(r.ConsumerID, "slo:") || strings.HasPrefix(r.ConsumerID, "runbook:") {
			declared++
			if r.Evidence != graph.EvidenceDeclared {
				t.Errorf("%s -> %s: evidence %s", r.ConsumerID, r.SignalID, r.Evidence)
			}
		}
	}
	if declared == 0 {
		t.Error("no SLO or runbook references")
	}

	src := map[string]graph.SourceCoverage{}
	for _, s := range out.Sources {
		src[s.Source] = s
	}
	slo := src[SourceSLOs]
	if !slo.Configured || slo.Coverage < 0.66 || slo.Coverage > 0.67 || !strings.Contains(slo.Gap, "shop/plugin") {
		t.Errorf("slo source %+v", slo)
	}
	rb := src[SourceRunbooks]
	if !rb.Configured || rb.Coverage < 0.33 || rb.Coverage > 0.34 ||
		!strings.Contains(rb.Gap, "403 Forbidden") || !strings.Contains(rb.Gap, "dashboard gone wasn't collected") {
		t.Errorf("runbook source %+v", rb)
	}
	if out.LogCoverage != 0 {
		t.Errorf("LogCoverage %v: definition sources mustn't count", out.LogCoverage)
	}
}

func TestDefinitionSourcesUnconfigured(t *testing.T) {
	out, err := Build(baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range out.Sources {
		if (s.Source == SourceSLOs || s.Source == SourceRunbooks) && (s.Configured || s.Coverage != 0 || s.Gap != "") {
			t.Errorf("%+v: want unconfigured", s)
		}
	}
	if len(out.Sources) != 2 {
		t.Errorf("sources %+v, want the two definition sources", out.Sources)
	}
}

func TestDefinitionSourceGapCap(t *testing.T) {
	var gaps []string
	for range 8 {
		gaps = append(gaps, "x")
	}
	s := definitionSource("runbooks", true, 8, 8, gaps)
	if s.Coverage != 0 || strings.Count(s.Gap, "\n") != maxGapLines || !strings.HasSuffix(s.Gap, "and 3 more") {
		t.Errorf("%+v", s)
	}
	if s := definitionSource(SourceSLOs, true, 0, 0, nil); s.Coverage != 1 {
		t.Errorf("configured, nothing found: coverage %v, want 1", s.Coverage)
	}
}
