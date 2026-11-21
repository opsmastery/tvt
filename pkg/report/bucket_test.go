package report

import (
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// TestBucketRows is S10 item 1: a read label whose readers all match it by
// class gets a bucketing row with its saving; an exact match, a grouping,
// a reader of every label or too few values gets none.
func TestBucketRows(t *testing.T) {
	label := func(name string) graph.Signal {
		return graph.Signal{ID: "prom:label:http_requests_total:" + name, Kind: graph.KindLabel, Name: name, Parent: "http_requests_total", Source: "prom", Class: "hard_keep"}
	}
	signals := []graph.Signal{label("code"), label("code_exact"), label("code_grouped"), label("code_all"), label("code_few"),
		{ID: "prom:label:http_requests_total:path", Kind: graph.KindLabel, Name: "path", Parent: "http_requests_total", Source: "prom", Class: "candidate"}}
	consumers := map[string]graph.Consumer{
		"rule:a":  {ID: "rule:a", Kind: graph.ConsumerAlertRule, Name: "HighErrors"},
		"panel:b": {ID: "panel:b", Kind: graph.ConsumerDashboardPanel, Name: "Shop / errors"},
	}
	coarse := func(sig, con, l string) graph.Reference {
		return graph.Reference{SignalID: sig, ConsumerID: con, Evidence: "declared", Labels: []graph.LabelRef{{Name: l, Use: graph.LabelCoarse}}}
	}
	refs := map[string][]graph.Reference{
		"prom:label:http_requests_total:code": {coarse("prom:label:http_requests_total:code", "rule:a", "code"), coarse("prom:label:http_requests_total:code", "panel:b", "code")},
		"prom:label:http_requests_total:code_exact": {coarse("x", "rule:a", "code_exact"),
			{ConsumerID: "panel:b", Labels: []graph.LabelRef{{Name: "code_exact", Use: graph.LabelMatch}}}},
		"prom:label:http_requests_total:code_grouped": {{ConsumerID: "panel:b", Labels: []graph.LabelRef{{Name: "code_grouped", Use: graph.LabelCoarse}, {Name: "code_grouped", Use: graph.LabelGroup}}}},
		"prom:label:http_requests_total:code_all":     {coarse("x", "rule:a", "code_all"), {ConsumerID: "panel:b", AllLabels: true}},
		"prom:label:http_requests_total:code_few":     {coarse("x", "rule:a", "code_few")},
	}
	costs := map[string]graph.Cost{}
	for _, s := range signals {
		costs[s.ID] = graph.Cost{SignalID: s.ID, DistinctValues: 40, ActiveSeries: 400, MonthlyCost: 10}
	}
	costs["prom:label:http_requests_total:code_few"] = graph.Cost{DistinctValues: 4, ActiveSeries: 40, MonthlyCost: 1}
	rows := bucketRows(signals, refs, consumers, costs, newGaps(nil, nil), map[string]bool{}, Scan{Currency: "USD"})
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1 (code): %+v", len(rows), rows)
	}
	r := rows[0]
	if r.ID != "bucket:prom:label:http_requests_total:code" || r.Action != ActionBucket || r.Savings.Monthly != 10*(1-5.0/40) || r.Merges != 350 ||
		len(r.Downstream) != 2 || r.EvidenceHash == "" || !strings.Contains(r.Claim, "40 values to at most 5 classes") {
		t.Errorf("%+v", r)
	}
	if e := expected(r, costs[r.Signal.ID], 0); e.SeriesAfter != 50 || !strings.Contains(e.Statements[0], "bucketed to its class") {
		t.Errorf("expected %+v", e)
	}
	// Measured classes (the lab's seed): 5 values, but only 2xx, 4xx and
	// 5xx, so bucketing still merges series where the estimate of at most
	// 5 classes would have seen nothing to save.
	costs["prom:label:http_requests_total:code_few"] = graph.Cost{DistinctValues: 5, Classes: 3, ActiveSeries: 40, MonthlyCost: 1}
	rows = bucketRows(signals, refs, consumers, costs, newGaps(nil, nil), map[string]bool{}, Scan{Currency: "USD"})
	if len(rows) != 2 {
		t.Fatalf("%d rows, want code and code_few: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Signal.Name == "code_few" && (r.Savings.Monthly != 1*(1-3.0/5) || !strings.Contains(r.Claim, "5 values to at most 3 classes")) {
			t.Errorf("code_few %+v", r)
		}
	}
}
