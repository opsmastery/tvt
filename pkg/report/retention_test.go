package report

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func TestRetentionTier(t *testing.T) {
	day := 24 * time.Hour
	s := graph.Signal{ID: "prom:metric:node_load15", Kind: graph.KindMetric, Name: "node_load15"}
	c := graph.Cost{SignalID: s.ID, MonthlyCost: 3, DiskBytes: 30_000}
	consumers := map[string]graph.Consumer{
		"adhoc":  {ID: "adhoc", Kind: graph.ConsumerAdhocQuery, Name: "explore", Expr: `max_over_time(node_load15[3d])`},
		"weekly": {ID: "weekly", Kind: graph.ConsumerAdhocQuery, Name: "weekly report", Expr: `avg_over_time(node_load15[1d] offset 9d)`},
		"vars":   {ID: "vars", Kind: graph.ConsumerDashboardPanel, Name: "Legacy / load", Expr: `avg_over_time(node_load15[$__range])`},
		"rule":   {ID: "rule", Kind: graph.ConsumerRecordingRule, Name: "node:load15:max1d", Expr: `max_over_time(node_load15[1d])`},
		"panel":  {ID: "panel", Kind: graph.ConsumerDashboardPanel, Name: "Load / weekly", Expr: `max_over_time(node:load15:max1d[30d])`},
	}

	// No reader: the floor, a week of 15 days.
	tier, why := retentionTier(s, c, nil, consumers, 15*day, 0)
	if tier == nil || tier.KeepDays != 7 || tier.CurrentDays != 15 || tier.Lookback != "" || tier.Writer != WriterNone {
		t.Fatalf("no reader: %+v %s", tier, why)
	}
	if math.Abs(tier.Monthly-3*8.0/15) > 1e-9 || tier.DiskBytes != 16_000 {
		t.Errorf("no reader saving: %+v", tier)
	}

	// A reader's window under the floor changes nothing; one over it sets
	// the tier, rounded up to a day.
	if tier, _ := retentionTier(s, c, []graph.Reference{{ConsumerID: "adhoc"}}, consumers, 15*day, 0); tier == nil || tier.KeepDays != 7 {
		t.Errorf("3d reader: %+v", tier)
	}
	tier, _ = retentionTier(s, c, []graph.Reference{{ConsumerID: "adhoc"}, {ConsumerID: "weekly"}}, consumers, 15*day, 0)
	if tier == nil || tier.KeepDays != 10 || tier.Lookback != "10d" || tier.Reader != "weekly report" {
		t.Errorf("10d reader: %+v", tier)
	}

	// Through a recording rule, the rule's window is the signal's.
	if tier, _ := retentionTier(s, c, []graph.Reference{{ConsumerID: "panel", Via: "rule"}}, consumers, 15*day, 0); tier == nil || tier.KeepDays != 7 || tier.Reader != "node:load15:max1d" {
		t.Errorf("via rule: %+v", tier)
	}

	// A window from a variable: no tier, with the reason.
	if tier, why := retentionTier(s, c, []graph.Reference{{ConsumerID: "vars"}}, consumers, 15*day, 0); tier != nil || !strings.Contains(why, "Legacy / load") {
		t.Errorf("variable window: %+v %q", tier, why)
	}
	// No shorter than now, a configured floor, a vendor card.
	if tier, why := retentionTier(s, c, nil, consumers, 5*day, 0); tier != nil || !strings.Contains(why, "no shorter") {
		t.Errorf("5-day retention: %+v %q", tier, why)
	}
	if tier, _ := retentionTier(s, c, nil, consumers, 90*day, 30*day); tier == nil || tier.KeepDays != 30 || tier.CurrentDays != 90 {
		t.Errorf("30d floor: %+v", tier)
	}
	if tier, _ := retentionTier(s, graph.Cost{MonthlyCost: 3}, nil, consumers, 15*day, 0); tier != nil {
		t.Errorf("vendor card (no disk): %+v", tier)
	}
	if tier, _ := retentionTier(graph.Signal{Kind: graph.KindStream}, c, nil, consumers, 15*day, 0); tier != nil {
		t.Errorf("stream: %+v", tier)
	}
}

// TestRetentionTierInReport: Build puts the tier on a dormant metric's
// confirm row when the scan is self-hosted, and nowhere otherwise.
func TestRetentionTierInReport(t *testing.T) {
	s := graph.Signal{ID: "prom:metric:node_context_switches_total", Kind: graph.KindMetric, Name: "node_context_switches_total",
		Source: "prom", Class: "dormant", Dormant: "named in postmortems/2026-10-24-checkout-slowdown.md"}
	in := Input{Scan: Scan{ID: 1, StartedAt: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), WindowDays: 30},
		Signals: []graph.Signal{s},
		Costs:   []graph.Cost{{SignalID: s.ID, ActiveSeries: 4, MonthlyCost: 1.5, DiskBytes: 9000}}}
	r := Build(in, 0)
	if len(r.Confirm) != 1 || r.Confirm[0].Retention != nil {
		t.Fatalf("vendor: %+v", r.Confirm)
	}
	in.Retention = 15 * 24 * time.Hour
	r = Build(in, 0)
	if len(r.Confirm) != 1 || r.Confirm[0].Retention == nil || r.Confirm[0].Retention.KeepDays != 7 {
		t.Fatalf("self-hosted: %+v", r.Confirm)
	}
}
