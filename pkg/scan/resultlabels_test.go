package scan

import (
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
)

// An alert or a viewed panel whose result keeps a metric's labels (no
// aggregation) depends on every one of them: removing one merges series
// and changes what fires or shows. Each label is protected like the
// metric, though none is counted as read. An ad hoc query's raw result
// protects nothing.
func TestResultLabelsProtectLabels(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Inputs)
		want  string
	}{
		{"alert", func(in *Inputs) {
			in.Rules = append(in.Rules, collect.Rule{Source: "prometheus", Namespace: "shop.yml", Group: "shop-alerts", Name: "CartSpike",
				Kind: collect.RuleAlert, Language: "promql", Expr: "rate(shop_cart_operations_total[5m]) > 100"})
		}, "hard_keep"},
		{"viewed raw panel", func(in *Inputs) {
			in.Dashboards = []collect.Dashboard{{UID: "v", Title: "V", Panels: []collect.Panel{{ID: 1, Title: "raw",
				Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: "rate(shop_cart_operations_total[5m])"}}}}}}
			in.Views = []collect.DashboardView{{DashboardUID: "v", OrgID: 1, At: start.Add(-time.Hour), Browser: true}}
		}, "keep_high"},
		{"aggregating alert", func(in *Inputs) {
			in.Rules = append(in.Rules, collect.Rule{Source: "prometheus", Namespace: "shop.yml", Group: "shop-alerts", Name: "CartSpike",
				Kind: collect.RuleAlert, Language: "promql", Expr: "sum by (op) (rate(shop_cart_operations_total[5m])) > 100"})
		}, "candidate"}, // user_id is summed away; op is read (checked below)
	}
	for _, tc := range cases {
		in := baseInputs()
		tc.setup(&in)
		out, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		sigs := signalIDs(out)
		if c := sigs["prom:label:shop_cart_operations_total:user_id"].Class; c != tc.want {
			t.Errorf("%s: user_id class %s, want %s", tc.name, c, tc.want)
		}
		if tc.name == "aggregating alert" {
			if c := sigs["prom:label:shop_cart_operations_total:op"].Class; c != "hard_keep" {
				t.Errorf("%s: op class %s, want hard_keep", tc.name, c)
			}
		}
		for _, r := range out.References {
			if r.SignalID == "prom:label:shop_cart_operations_total:user_id" && !r.Sweep {
				t.Errorf("%s: user_id has a reference %+v; result labels aren't reads", tc.name, r)
			}
		}
	}
}
