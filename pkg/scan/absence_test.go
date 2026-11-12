package scan

import (
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// Sprint 2 item 18: an absent() alert is a hard keep for its metric and
// for every label it matches on (dropping one would fire it).
func TestAbsenceAlertIsHardKeep(t *testing.T) {
	in := baseInputs()
	in.Rules = append(in.Rules, collect.Rule{Source: "prometheus", Namespace: "shop.yml", Group: "shop-alerts", Name: "CartAddsMissing",
		Kind: collect.RuleAlert, Language: "promql", Expr: `absent(shop_cart_operations_total{op="add"})`})
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	sigs := signalIDs(out)
	for id, class := range map[string]string{
		"prom:metric:shop_cart_operations_total":        "hard_keep",
		"prom:label:shop_cart_operations_total:op":      "hard_keep", // matched on
		"prom:label:shop_cart_operations_total:user_id": "candidate", // not read
	} {
		if sigs[id].Class != class {
			t.Errorf("%s: class %q, want %q", id, sigs[id].Class, class)
		}
	}
	for _, r := range out.References {
		if r.ConsumerID == "rule:prometheus:shop.yml/shop-alerts/CartAddsMissing" && r.SignalID == "prom:metric:shop_cart_operations_total" {
			if r.ResultLabels || r.Assumed || r.Evidence != graph.EvidenceDeclared ||
				len(r.Labels) != 1 || r.Labels[0] != (graph.LabelRef{Name: "op", Use: graph.LabelMatch}) {
				t.Errorf("absence ref %+v", r)
			}
			return
		}
	}
	t.Error("no reference from the absence alert")
}
