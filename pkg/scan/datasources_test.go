package scan

import (
	"slices"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// Sprint 2 item 19: a panel or Grafana-managed rule on a datasource tvt
// doesn't parse is an assumed reader of the collected metrics it names, is
// marked Unparsed, and shows up as a gap; it never reads nothing silently.
func TestUnknownDatasources(t *testing.T) {
	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "shop", Title: "Shop", Panels: []collect.Panel{
		{ID: 1, Title: "CPU flame graph", Queries: []collect.PanelQuery{{DatasourceType: "grafana-pyroscope-datasource",
			Expr: `process_cpu:cpu:nanoseconds:cpu:nanoseconds{service_name="shop"}`}}},
		{ID: 2, Title: "Load (AMP)", Queries: []collect.PanelQuery{{DatasourceType: "grafana-amazonprometheus-datasource",
			Expr: `avg(rate(node_load1[5m])) + shop_not_collected_total`}}},
		{ID: 3, Title: "Cart", Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: `sum by (op) (shop_cart_operations_total)`}}},
		{ID: 4, Title: "Empty", Queries: []collect.PanelQuery{{DatasourceType: "elasticsearch"}}},
		{ID: 5, Title: "AMP by name", Queries: []collect.PanelQuery{{DatasourceType: "grafana-amazonprometheus-datasource",
			Expr: `{__name__=~"node_cpu_.+", mode="idle"}`}}},
	}}}
	in.Rules = append(in.Rules,
		collect.Rule{Source: "grafana", Namespace: "f", Group: "g", Name: "ES errors", UID: "es",
			Kind: collect.RuleAlert, EvaluatedBy: collect.EvaluatedByGrafana, Expr: "A: status:500",
			Queries: []collect.PanelQuery{{DatasourceUID: "es", DatasourceType: "elasticsearch", Expr: "status:500"}}},
		collect.Rule{Source: "grafana", Namespace: "f", Group: "g", Name: "Mixed", UID: "mixed",
			Kind: collect.RuleAlert, EvaluatedBy: collect.EvaluatedByGrafana, Expr: "A: status:500\nB: up",
			Queries: []collect.PanelQuery{{DatasourceUID: "es", DatasourceType: "elasticsearch", Expr: "status:500"},
				{DatasourceUID: "pyro", DatasourceType: "grafana-pyroscope-datasource"},
				{DatasourceUID: "es", DatasourceType: "elasticsearch"}}})
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	cons := consumers(out)
	for id, want := range map[string]string{
		"panel:1:shop:1:0":    "datasource type grafana-pyroscope-datasource",
		"panel:1:shop:2:0":    "datasource type grafana-amazonprometheus-datasource",
		"panel:1:shop:3:0":    "",
		"panel:1:shop:4:0":    "datasource type elasticsearch", // no query text: still a consumer
		"rule:grafana:f/g/es": "datasource type elasticsearch",
		// several types on one rule: all of them, not the last
		"rule:grafana:f/g/mixed": "datasource types elasticsearch, grafana-pyroscope-datasource",
	} {
		c, ok := cons[id]
		if !ok || c.Unparsed != want {
			t.Errorf("%s: %+v (found %v), want Unparsed %q", id, c, ok, want)
		}
	}
	var amp, amp5 []string
	for _, r := range out.References {
		switch r.ConsumerID {
		case "panel:1:shop:2:0":
			if !r.Assumed || !r.AllLabels || !strings.Contains(strings.Join(r.Reasons, " "), "doesn't parse") {
				t.Errorf("AMP panel ref %+v: want assumed, every label, with the reason", r)
			}
			amp = append(amp, r.SignalID)
		case "panel:1:shop:5:0":
			// a __name__ regex selector spells its metrics out
			amp5 = append(amp5, r.SignalID)
		case "panel:1:shop:1:0", "panel:1:shop:4:0", "rule:grafana:f/g/es", "rule:grafana:f/g/mixed":
			t.Errorf("%s names no collected metric, but references %s", r.ConsumerID, r.SignalID)
		}
	}
	if len(amp) != 1 || amp[0] != "prom:metric:node_load1" {
		t.Errorf("AMP panel references %v, want only the collected node_load1", amp)
	}
	if len(amp5) == 0 || !slices.Contains(amp5, "prom:metric:node_cpu_seconds_total") {
		t.Errorf("AMP {__name__=~...} panel references %v, want node_cpu_seconds_total", amp5)
	}
	if _, ok := signalIDs(out)["prom:metric:shop_not_collected_total"]; ok {
		t.Error("an unparsed query created a signal for a metric nobody collected")
	}
	var src string
	for _, s := range out.Sources {
		if s.Source == SourceDatasources {
			src = s.Gap
			// 9 queries (5 panels, 4 rule queries), 8 on datasources tvt doesn't parse.
			if !s.Configured || s.Coverage < 0.11 || s.Coverage > 0.12 {
				t.Errorf("datasources source %+v", s)
			}
		}
	}
	for _, want := range []string{
		"elasticsearch: 4 queries tvt doesn't parse (ES errors, Mixed, Shop / Empty)",
		"grafana-pyroscope-datasource: 2 queries tvt doesn't parse (Mixed, Shop / CPU flame graph)",
		"grafana-amazonprometheus-datasource: 2 queries tvt doesn't parse (Shop / AMP by name, Shop / Load (AMP))",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("datasources gap %q missing %q", src, want)
		}
	}
	if out.LogCoverage != 0 {
		t.Errorf("LogCoverage %v: the datasources row mustn't count", out.LogCoverage)
	}
}
