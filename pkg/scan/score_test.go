package scan

import (
	"math"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/score"
)

func TestScoreCandidates(t *testing.T) {
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	since := now.Add(-30 * day)
	sig := func(id, src, class string) graph.Signal {
		return graph.Signal{ID: id, Source: src, Class: class}
	}
	signals := []graph.Signal{
		sig("prom:metric:unread", "prom", "candidate"),
		sig("prom:metric:adhoc", "prom", "candidate"),
		sig("prom:metric:old_read", "prom", "candidate"),
		sig("prom:metric:stale", "prom", "candidate"),
		sig("prom:metric:rule", "prom", "candidate"),
		sig("prom:metric:new", "prom", "candidate"),
		sig("prom:metric:young", "prom", "candidate"),
		sig("prom:metric:alerted", "prom", "hard_keep"),
		sig("loki:stream:{app=\"a\"}", "loki", "candidate"),
	}
	consumers := []graph.Consumer{
		{ID: "q", Kind: graph.ConsumerAdhocQuery},
		{ID: "panel", Kind: graph.ConsumerDashboardPanel, LastViewed: now.Add(-100 * day)},
		{ID: "rec", Kind: graph.ConsumerRecordingRule},
		{ID: "alert", Kind: graph.ConsumerAlertRule},
	}
	refs := []graph.Reference{
		{SignalID: "prom:metric:adhoc", ConsumerID: "q", Evidence: graph.EvidenceObserved, LastSeen: now.Add(-15 * day)},
		{SignalID: "prom:metric:stale", ConsumerID: "panel", Evidence: graph.EvidenceObserved},
		{SignalID: "prom:metric:rule", ConsumerID: "rec", Evidence: graph.EvidenceDeclared},
		{SignalID: "prom:metric:alerted", ConsumerID: "alert", Evidence: graph.EvidenceDeclared},
	}
	costs := []graph.Cost{
		{SignalID: "prom:metric:unread", MonthlyCost: 100},
		{SignalID: "prom:metric:adhoc", MonthlyCost: 100},
		{SignalID: "prom:metric:rule", MonthlyCost: 100},
	}
	sources := []graph.SourceCoverage{
		{Source: SourcePromQueryLog, Configured: true, Coverage: 1},
		{Source: SourceGrafanaLog, Configured: true, Coverage: 0.5},
		{Source: SourceLokiQueryLog, Configured: false, Coverage: 0},
	}
	first := now.Add(-60 * day)
	in := Inputs{Start: now, Window: 30 * day, History: History{
		FirstSeen: map[string]time.Time{
			"prom:metric:unread": first, "prom:metric:adhoc": first, "prom:metric:old_read": first,
			"prom:metric:stale": first, "prom:metric:rule": first, "prom:metric:alerted": first,
			"prom:metric:young": now.Add(-7 * day),
		},
		LastReads: []PastRead{{SignalID: "prom:metric:old_read", At: now.Add(-45 * day)}},
	}}
	got := map[string]graph.Score{}
	out := scoreCandidates(in, signals, consumers, refs, costs, sources, since)
	for _, r := range out {
		got[r.SignalID] = r
	}
	if len(out) != 8 {
		t.Fatalf("scored %d signals, want the 8 candidates: %+v", len(out), out)
	}
	if _, ok := got["prom:metric:alerted"]; ok {
		t.Error("a hard keep was scored")
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	for _, tc := range []struct {
		id         string
		strongest  string
		weight     float64
		confidence float64
		held       string
	}{
		{"prom:metric:unread", "none", 0, 0.75, ""},
		{"prom:metric:adhoc", "adhoc_query", 1, 0.75, ""},            // one half-life
		{"prom:metric:old_read", "adhoc_query", 2 * 0.125, 0.75, ""}, // kept from history, three half-lives
		{"prom:metric:stale", "stale_dashboard", 0.5, 0.75, ""},      // no decay from a 100-day-old view
		{"prom:metric:rule", "recording_rule", 1, 0.75, ""},
		{"prom:metric:new", "none", 0, 0, score.HeldNew},
		{"prom:metric:young", "none", 0, 0.75 * 0.5, ""},
		{"loki:stream:{app=\"a\"}", "none", 0, 0.25, ""}, // tvt never watched loki before: age unknown
	} {
		r := got[tc.id]
		if r.Strongest != tc.strongest || !near(r.UsageWeight, tc.weight) || !near(r.Confidence, tc.confidence) || r.Held != tc.held {
			t.Errorf("%s = %+v, want %s weight %v confidence %v held %q", tc.id, r, tc.strongest, tc.weight, tc.confidence, tc.held)
		}
	}
	if out[0].SignalID != "prom:metric:unread" || !near(out[0].Priority, 75) {
		t.Errorf("top = %+v", out[0])
	}
	if r := got["prom:metric:adhoc"]; !near(r.Priority, 100/2.0*0.75) {
		t.Errorf("adhoc priority = %v", r.Priority)
	}

	// Loki first watched a week after Prometheus: its streams from that
	// scan still predate tvt.
	in.History.FirstSeen["loki:stream:{app=\"a\"}"] = first.Add(7 * day)
	for _, r := range scoreCandidates(in, signals, consumers, refs, costs, sources, since) {
		if r.SignalID == "loki:stream:{app=\"a\"}" && !near(r.Confidence, 0.25) {
			t.Errorf("stream from loki's first scan: %+v", r)
		}
	}

	// A first scan knows no ages; a window shorter than window_days holds
	// everything.
	in.History = History{}
	in.MinWindowDays = 45
	for _, r := range scoreCandidates(in, signals, consumers, refs, costs, sources, since) {
		if r.Confidence != 0 || r.Held != score.HeldWindow {
			t.Errorf("30-day window, window_days 45: %+v", r)
		}
	}
	in.MinWindowDays = 0
	for _, r := range scoreCandidates(in, signals, consumers, refs, costs, sources, since) {
		if r.Held == score.HeldNew {
			t.Errorf("first scan held %s as new", r.SignalID)
		}
	}
}
