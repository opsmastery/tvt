package scan

import (
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// Sprint 2 item 17: a MetricsQL query the PromQL parser rejects, on a
// VictoriaMetrics datasource or in a rule, still references every
// collected metric it names (assumed), and invents none.
func TestMetricsQLFailsSafe(t *testing.T) {
	in := baseInputs()
	in.Rules = append(in.Rules, collect.Rule{Source: "vmalert", Namespace: "vm.yml", Group: "g", Name: "VMErrors",
		Kind: collect.RuleAlert, Language: "promql", Expr: `sum(rate(node_load1)) default 0 > 1`})
	in.Dashboards = []collect.Dashboard{{UID: "vm", Title: "VM", Panels: []collect.Panel{{ID: 1, Title: "cpu", Queries: []collect.PanelQuery{
		{DatasourceType: "victoriametrics-metrics-datasource",
			Expr: `WITH (f = {mode="idle"}) sum(rate(node_cpu_seconds_total{f}[5m])) by (mode) keep_metric_names`},
	}}}}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]bool{}
	for _, r := range out.References {
		if r.ConsumerID == "rule:vmalert:vm.yml/g/VMErrors" || r.ConsumerID == "panel:1:vm:1:0" {
			if !r.Assumed || !r.AllLabels {
				t.Errorf("%s -> %s: want assumed, every label", r.ConsumerID, r.SignalID)
			}
			refs[r.ConsumerID+" "+r.SignalID] = true
		}
	}
	for _, want := range []string{
		"rule:vmalert:vm.yml/g/VMErrors prom:metric:node_load1",
		"panel:1:vm:1:0 prom:metric:node_cpu_seconds_total",
	} {
		if !refs[want] {
			t.Errorf("missing reference %s; got %v", want, refs)
		}
	}
	sigs := signalIDs(out)
	if sigs["prom:metric:node_load1"].Class != "hard_keep" {
		t.Errorf("node_load1: class %q, want hard_keep through the MetricsQL alert", sigs["prom:metric:node_load1"].Class)
	}
	for id := range sigs {
		if id == "prom:metric:f" || id == "prom:metric:keep_metric_names" || id == "prom:metric:default" {
			t.Errorf("a MetricsQL keyword became a signal: %s", id)
		}
	}
}
