package report

import (
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
)

func TestIntervalRows(t *testing.T) {
	m := func(name string) string { return "prom:metric:" + name }
	var signals []graph.Signal
	for _, n := range []string{"a_total", "b", "c_total", "d", "e_total", "f", "g_total", "h_total", "k"} {
		signals = append(signals, graph.Signal{ID: m(n), Kind: graph.KindMetric, Name: n, Source: "prom"})
	}
	in := Input{
		Signals: signals,
		PipelineTargets: []graph.PipelineTarget{
			{Subject: m("a_total"), Jobs: []string{"fast"}},
			{Subject: m("b"), Jobs: []string{"fast"}},
			{Subject: m("c_total"), Jobs: []string{"narrow"}},
			{Subject: m("d"), Jobs: []string{"panel"}},
			{Subject: m("e_total"), Jobs: []string{"paging"}},
			{Subject: m("f"), Jobs: []string{"unread"}},
			{Subject: m("g_total"), Jobs: []string{"pushed"}},
			{Subject: m("h_total"), Jobs: []string{"recorded"}},
			{Subject: m("k"), Jobs: []string{"gauge"}},
		},
		Consumers: []graph.Consumer{
			{ID: "r1", Kind: graph.ConsumerAlertRule, Name: "AHigh", Expr: `rate(a_total[10m]) > 1`, Interval: time.Minute},
			{ID: "p1", Kind: graph.ConsumerDashboardPanel, Name: "B gauge", Expr: `max(b)`},
			{ID: "p2", Kind: graph.ConsumerDashboardPanel, Name: "C rate", Expr: `rate(c_total[1m])`},
			{ID: "p3", Kind: graph.ConsumerDashboardPanel, Name: "D panel", Expr: `rate(d[$__rate_interval])`},
			{ID: "r2", Kind: graph.ConsumerAlertRule, Name: "EPage", Expr: `increase(e_total[30m]) > 0`, Interval: time.Minute, Severity: "critical", For: 5 * time.Minute},
			{ID: "p4", Kind: graph.ConsumerDashboardPanel, Name: "G", Expr: `rate(g_total[5m])`},
			{ID: "rec", Kind: graph.ConsumerRecordingRule, Name: "job:h:rate20m", Expr: `rate(h_total[20m])`, Interval: time.Minute},
			{ID: "p5", Kind: graph.ConsumerDashboardPanel, Name: "H recorded", Expr: `job:h:rate20m`},
			{ID: "p6", Kind: graph.ConsumerDashboardPanel, Name: "K now", Expr: `max(k)`},
		},
		References: []graph.Reference{
			{SignalID: m("a_total"), ConsumerID: "r1"}, {SignalID: m("b"), ConsumerID: "p1"},
			{SignalID: m("c_total"), ConsumerID: "p2"}, {SignalID: m("d"), ConsumerID: "p3"},
			{SignalID: m("e_total"), ConsumerID: "r2"}, {SignalID: m("g_total"), ConsumerID: "p4"},
			{SignalID: m("h_total"), ConsumerID: "p5", Via: "rec"}, {SignalID: m("k"), ConsumerID: "p6"},
		},
		Costs: []graph.Cost{
			{SignalID: m("a_total"), Interval: 15 * time.Second, MonthlyCost: 8},
			{SignalID: m("b"), Interval: 15 * time.Second, MonthlyCost: 4},
			{SignalID: m("c_total"), Interval: 15 * time.Second},
			{SignalID: m("d"), Interval: 15 * time.Second},
			{SignalID: m("e_total"), Interval: 15 * time.Second},
			{SignalID: m("f"), Interval: 15 * time.Second},
			{SignalID: m("g_total"), Interval: 15 * time.Second, IntervalAssumed: "no scrape metadata"},
			{SignalID: m("h_total"), Interval: 15 * time.Second},
			{SignalID: m("k"), Interval: 15 * time.Second},
		},
		Objectives: map[string]time.Duration{"critical": 40 * time.Minute},
		Retention:  15 * 24 * time.Hour,
	}
	refs := map[string][]graph.Reference{}
	for _, r := range in.References {
		refs[r.SignalID] = append(refs[r.SignalID], r)
	}
	consumers := map[string]graph.Consumer{}
	for _, c := range in.Consumers {
		consumers[c.ID] = c
	}
	costs := map[string]graph.Cost{}
	for _, c := range in.Costs {
		costs[c.SignalID] = c
	}
	rows := map[string]IntervalChange{}
	for _, r := range intervalRows(in, refs, consumers, costs) {
		rows[r.Job] = r
	}
	if _, ok := rows["unread"]; ok {
		t.Error("a job nothing reads got a row: its drops cover it")
	}
	// fast: the 10m alert's range allows 2m30s (4 samples), so 2m.
	if r := rows["fast"]; r.Held != "" || r.ProposedSec != 120 || r.Readers != 2 {
		t.Errorf("fast: %+v", r)
	}
	// Saving: samples fall from every 15s to every 2m on a self-hosted card.
	if r := rows["fast"]; r.Monthly < 10.49 || r.Monthly > 10.51 {
		t.Errorf("fast saving %v, want 12 × (1 − 15/120) = 10.5", r.Monthly)
	}
	// narrow: rate over 1m needs four samples, so 15s is already the most.
	if r := rows["narrow"]; !strings.Contains(r.Held, "1m range needs 4 samples") {
		t.Errorf("narrow: %+v", r)
	}
	if r := rows["panel"]; !strings.Contains(r.Held, "D panel") || !strings.Contains(r.Held, "variable") {
		t.Errorf("panel: %+v", r)
	}
	if r := rows["pushed"]; !strings.Contains(r.Held, "wasn't measured") {
		t.Errorf("pushed: %+v", r)
	}
	// Through a recording rule, the rule's 20m window decides (5m).
	if r := rows["recorded"]; r.ProposedSec != 300 || r.ShortestBy != "job:h:rate20m" {
		t.Errorf("recorded: %+v", r)
	}
	// paging: 30m allows 5m, but the critical alert's delay then passes
	// its 40m objective (5m scrape + 30m range + 1m + 5m for + group wait).
	if r := rows["paging"]; r.ProposedSec != 120 || !strings.Contains(r.Bound, "EPage") {
		t.Errorf("paging: %+v", r)
	}
	// gauge: read only without a range, capped at 2m by the lookback.
	if r := rows["gauge"]; r.ProposedSec != 120 || !strings.Contains(r.Bound, "lookback") {
		t.Errorf("gauge: %+v", r)
	}
	// A vendor card prices series here: no saving.
	in.Retention = 0
	for _, r := range intervalRows(in, refs, consumers, costs) {
		if r.Monthly != 0 {
			t.Errorf("vendor %s saving %v", r.Job, r.Monthly)
		}
	}
}
