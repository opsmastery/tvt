package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func TestServicesEvidenceKept(t *testing.T) {
	in := fixture()
	for i := range in.Signals {
		switch in.Signals[i].ID {
		case "prom:metric:alerted", "prom:metric:viewed":
			in.Signals[i].Owner = "team-shop"
		}
	}
	in.Signals = append(in.Signals,
		graph.Signal{ID: "prom:label:alerted:pod", Kind: graph.KindLabel, Name: "pod", Parent: "alerted", Source: "prom", Class: "hard_keep", Owner: "team-shop"},
		graph.Signal{ID: "prom:metric:postmortem", Kind: graph.KindMetric, Name: "postmortem", Source: "prom", Class: "candidate", Owner: "team-shop"})
	in.Consumers = append(in.Consumers, graph.Consumer{ID: "pm:1", Kind: graph.ConsumerAdhocQuery, Name: "incident"})
	in.References = append(in.References, graph.Reference{SignalID: "prom:metric:postmortem", ConsumerID: "pm:1", Evidence: graph.EvidenceIncident},
		graph.Reference{SignalID: "prom:metric:postmortem", ConsumerID: "pm:1", Evidence: graph.EvidenceIncident})
	in.Costs = append(in.Costs, graph.Cost{SignalID: "prom:metric:viewed", MonthlyCost: 5}, graph.Cost{SignalID: "prom:label:alerted:pod", MonthlyCost: 500},
		graph.Cost{SignalID: "prom:metric:postmortem", MonthlyCost: 7})
	// Top 1 still tallies every recommendation per service.
	r := Build(in, 1)
	all := Build(in, 0)
	cut := map[string]Tally{}
	for _, rec := range all.Recommendations {
		owner := rec.Owner
		if owner == "" {
			owner = Unowned
		}
		tl := cut[owner]
		tl.add(rec.Savings.Monthly)
		cut[owner] = tl
	}
	got := map[string]Service{}
	for _, s := range r.Services {
		got[s.Name] = s
	}
	for name, want := range cut {
		if got[name].Cut != want {
			t.Errorf("%s cut = %+v, want %+v", name, got[name].Cut, want)
		}
	}
	shop := got["team-shop"]
	if shop.HardKeep != (Tally{1, 999}) || shop.KeepHigh != (Tally{1, 5}) || shop.Incident != (Tally{1, 7}) || shop.Records != (Tally{}) {
		t.Errorf("team-shop = %+v", shop)
	}
	if un := got[Unowned]; un.Dormant != (Tally{1, 0}) {
		t.Errorf("unowned = %+v", un)
	}
	for i := 1; i < len(r.Services); i++ {
		if r.Services[i-1].Cut.Monthly < r.Services[i].Cut.Monthly {
			t.Errorf("order: %s before %s", r.Services[i-1].Name, r.Services[i].Name)
		}
	}
	var b bytes.Buffer
	if err := RenderHTML(&b, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "Evidence kept per service") || !strings.Contains(b.String(), "<td>team-shop</td>") {
		t.Error("HTML lacks the evidence-kept table")
	}
}
