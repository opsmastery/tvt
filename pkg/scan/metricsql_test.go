package scan

import (
	"slices"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// Sprint 2 item 17: a MetricsQL query the PromQL parser rejects, on a
// VictoriaMetrics datasource or in a rule, still references every
// collected metric it names (assumed), and invents none: no keyword, no
// WITH binding, and no match-every-metric wildcard from a WITH filter.
func TestMetricsQLFailsSafe(t *testing.T) {
	const vm = "victoriametrics-metrics-datasource"
	in := baseInputs()
	in.Rules = append(in.Rules,
		collect.Rule{Source: "vmalert", Namespace: "vm.yml", Group: "g", Name: "VMErrors",
			Kind: collect.RuleAlert, Language: "promql", Expr: `sum(rate(node_load1)) default 0 > 1`},
		collect.Rule{Source: "vmalert", Namespace: "vm.yml", Group: "g", Name: "VMWith",
			Kind: collect.RuleAlert, Language: "promql",
			Expr: `WITH (cf = {job="shop"}) sum(rate(shop_http_requests_total{cf}[5m])) > 0`},
	)
	in.Dashboards = []collect.Dashboard{{UID: "vm", Title: "VM",
		Variables: []collect.Variable{{Name: "m", Type: "query", DatasourceType: vm, Query: "metrics(^node_load)"}},
		Panels: []collect.Panel{{ID: 1, Title: "cpu", Queries: []collect.PanelQuery{
			{DatasourceType: vm, Expr: `WITH (f = {mode="idle"}) sum(rate(node_cpu_seconds_total{f}[5m])) by (mode) keep_metric_names`},
			{DatasourceType: vm, Expr: `sum(rate($m[5m]))`},
		}}}}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"rule:vmalert:vm.yml/g/VMErrors": {"prom:metric:node_load1"},
		"rule:vmalert:vm.yml/g/VMWith":   {"prom:metric:shop_http_requests_total"},
		"panel:1:vm:1:0":                 {"prom:metric:node_cpu_seconds_total"},
		"panel:1:vm:1:1":                 {"prom:metric:node_load1"},
	}
	got := map[string][]string{}
	for _, r := range out.References {
		if _, ok := want[r.ConsumerID]; !ok {
			continue
		}
		got[r.ConsumerID] = append(got[r.ConsumerID], r.SignalID)
		if r.ConsumerID != "panel:1:vm:1:1" && (!r.Assumed || !r.AllLabels) {
			t.Errorf("%s -> %s: want assumed, every label", r.ConsumerID, r.SignalID)
		}
	}
	for c, w := range want {
		g := got[c]
		slices.Sort(g)
		if !slices.Equal(g, w) {
			t.Errorf("%s references %v, want exactly %v", c, g, w)
		}
	}
	sigs := signalIDs(out)
	if sigs["prom:metric:node_load1"].Class != "hard_keep" {
		t.Errorf("node_load1: class %q, want hard_keep through the MetricsQL alert", sigs["prom:metric:node_load1"].Class)
	}
	if c := sigs["prom:metric:shop_cart_operations_total"].Class; c == "hard_keep" {
		t.Error("shop_cart_operations_total is hard_keep: a MetricsQL WITH alert protected a metric it doesn't name")
	}
	for id := range sigs {
		if id == "prom:metric:f" || id == "prom:metric:cf" || id == "prom:metric:keep_metric_names" || id == "prom:metric:default" {
			t.Errorf("a MetricsQL keyword or WITH binding became a signal: %s", id)
		}
	}
}
