package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
)

var at = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func fixture() Input {
	tier := 2
	sig := func(id string, kind graph.SignalKind, name, parent, src, class string) graph.Signal {
		return graph.Signal{ID: id, Kind: kind, Name: name, Parent: parent, Source: src, Class: class}
	}
	owned := sig("prom:metric:owned", graph.KindMetric, "owned", "", "prom", "candidate")
	owned.Owner, owned.Tier = "team-shop", &tier
	return Input{
		Scan: Scan{ID: 7, StartedAt: at, WindowDays: 30, LogCoverage: 0.5, PriceCard: "grafana-cloud-pro", Currency: "USD", Approximate: true},
		Signals: []graph.Signal{
			sig("prom:metric:unread", graph.KindMetric, "unread", "", "prom", "candidate"),
			owned,
			sig("prom:metric:swept", graph.KindMetric, "swept", "", "prom", "candidate"),
			sig("prom:metric:queried", graph.KindMetric, "queried", "", "prom", "candidate"),
			sig("prom:metric:stale", graph.KindMetric, "stale", "", "prom", "candidate"),
			sig("prom:label:unread:pod", graph.KindLabel, "pod", "unread", "prom", "candidate"),
			sig("prom:label:kept:pod", graph.KindLabel, "pod", "kept", "prom", "candidate"),
			sig("prom:metric:alerted", graph.KindMetric, "alerted", "", "prom", "hard_keep"),
			sig("prom:metric:viewed", graph.KindMetric, "viewed", "", "prom", "keep_high"),
			sig("prom:metric:node_errors", graph.KindMetric, "node_errors", "", "prom", "dormant"),
			sig("prom:metric:fresh", graph.KindMetric, "fresh", "", "prom", "candidate"),
			sig(`loki:stream:{app="shop"}`, graph.KindStream, `{app="shop"}`, "", "loki", "candidate"),
			sig("loki:stream:*", graph.KindStream, "*", "", "loki", "candidate"),
		},
		Consumers: []graph.Consumer{
			{ID: "qlog:prometheus:agent:sweep:1", Kind: graph.ConsumerAgentQuery, Name: "sweep", Expr: `count by (__name__) ({__name__=~".+"})`, Client: "agent"},
			{ID: "qlog:prometheus:abc", Kind: graph.ConsumerAdhocQuery, Name: "explore", Expr: "rate(queried[5m])", Client: "explore"},
			{ID: "panel:1:d:3:0", Kind: graph.ConsumerDashboardPanel, Name: "Old / panel", Team: "team-ops"},
			{ID: "rule:prom:a/g/HighErrors", Kind: graph.ConsumerAlertRule, Name: "HighErrors"},
		},
		References: []graph.Reference{
			{SignalID: "prom:metric:swept", ConsumerID: "qlog:prometheus:agent:sweep:1", Evidence: graph.EvidenceObserved, LastSeen: at.Add(-time.Hour), Sweep: true},
			{SignalID: "prom:metric:queried", ConsumerID: "qlog:prometheus:abc", Evidence: graph.EvidenceObserved, LastSeen: at.Add(-20 * 24 * time.Hour), Actor: "alice"},
			{SignalID: "prom:metric:stale", ConsumerID: "panel:1:d:3:0", Evidence: graph.EvidenceObserved, Assumed: true, Reasons: []string{"variable $metric"}},
			{SignalID: "prom:metric:alerted", ConsumerID: "rule:prom:a/g/HighErrors", Evidence: graph.EvidenceDeclared},
		},
		Costs: []graph.Cost{
			{SignalID: "prom:metric:unread", MonthlyCost: 100},
			{SignalID: "prom:metric:owned", MonthlyCost: 100},
			{SignalID: "prom:metric:swept", MonthlyCost: 40},
			{SignalID: "prom:metric:queried", MonthlyCost: 80},
			{SignalID: "prom:metric:stale", MonthlyCost: 30},
			{SignalID: "prom:label:unread:pod", MonthlyCost: 60},
			{SignalID: "prom:label:kept:pod", MonthlyCost: 20},
			{SignalID: "prom:metric:alerted", MonthlyCost: 999},
			{SignalID: `loki:stream:{app="shop"}`, MonthlyCost: 12},
		},
		Sources: []graph.SourceCoverage{
			{Source: scan.SourcePromQueryLog, Configured: true, Coverage: 1},
			{Source: scan.SourceGrafanaLog, Configured: true, Coverage: 0.4},
			{Source: scan.SourceLokiQueryLog},
			{Source: scan.SourceDatasources, Configured: true, Coverage: 1},
		},
		Scores: []graph.Score{
			{SignalID: "prom:metric:unread", MonthlyCost: 100, Strongest: "none", Confidence: 0.7, Priority: 70},
			{SignalID: "prom:metric:owned", MonthlyCost: 100, Strongest: "none", Confidence: 0.7, Priority: 70},
			{SignalID: "prom:metric:swept", MonthlyCost: 40, UsageWeight: 0.25, Strongest: "sweep", Confidence: 0.7, Priority: 22.4},
			{SignalID: "prom:metric:queried", MonthlyCost: 80, UsageWeight: 0.8, Strongest: "adhoc_query", Confidence: 0.7, Priority: 31.1},
			{SignalID: "prom:metric:stale", MonthlyCost: 30, UsageWeight: 0.5, Strongest: "stale_dashboard", Confidence: 0.7, Priority: 14},
			{SignalID: "prom:label:unread:pod", MonthlyCost: 60, Strongest: "none", Confidence: 0.7, Priority: 42},
			{SignalID: "prom:label:kept:pod", MonthlyCost: 20, Strongest: "none", Confidence: 0.7, Priority: 14},
			{SignalID: "prom:metric:fresh", Strongest: "none", Held: "new"},
			{SignalID: `loki:stream:{app="shop"}`, MonthlyCost: 12, Strongest: "none", Confidence: 0.2, Priority: 2.4},
			{SignalID: "loki:stream:*", Strongest: "none", Confidence: 0.2},
			// A protected signal with a stale score row must not leak in.
			{SignalID: "prom:metric:alerted", MonthlyCost: 999, Strongest: "none", Confidence: 1, Priority: 999},
		},
		LastReads: []LastReadOf{{SignalID: "prom:metric:queried", LastRead: LastRead{Consumer: "qlog:prometheus:abc", Actor: "alice", At: at.Add(-20 * 24 * time.Hour)}}},
	}
}

func byID(r Report) map[string]Recommendation {
	m := map[string]Recommendation{}
	for _, rec := range r.Recommendations {
		m[rec.ID] = rec
	}
	return m
}

func TestBuildRecommendsOnlyTrustedCandidates(t *testing.T) {
	r := Build(fixture(), 0)
	got := byID(r)
	for _, id := range []string{"drop:prom:metric:alerted", "drop:prom:metric:viewed", "drop:prom:metric:node_errors", "drop:prom:metric:fresh", "reroute:loki:stream:*"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s was recommended", id)
		}
	}
	if _, ok := got["label_removal:prom:label:unread:pod"]; ok {
		t.Error("a label of a dropped metric was recommended on its own")
	}
	if _, ok := got["label_removal:prom:label:kept:pod"]; !ok {
		t.Error("a label of a kept metric wasn't recommended")
	}
	if r.Held["new"] != 1 || len(r.Held) != 1 {
		t.Errorf("held = %v", r.Held)
	}
	if len(r.Recommendations) != 7 || r.Totals.Recommendations != 7 || r.Totals.Candidates != 10 {
		t.Errorf("recommendations %d, totals %+v", len(r.Recommendations), r.Totals)
	}
	if r.Totals.StreamSavings != 12 || r.Totals.MetricSavings != 100+100+40+80+30+20 {
		t.Errorf("totals %+v", r.Totals)
	}
}

func TestBuildRanksBySavingsPerMinute(t *testing.T) {
	r := Build(fixture(), 0)
	for i := 1; i < len(r.Recommendations); i++ {
		if r.Recommendations[i].SavingsPerMinute > r.Recommendations[i-1].SavingsPerMinute {
			t.Fatalf("out of order at %d: %+v", i, r.Recommendations)
		}
	}
	// The queried metric has a 2-minute check (ask alice): it outranks
	// the owned one (5 minutes) despite a lower priority.
	if r.Recommendations[0].ID != "drop:prom:metric:queried" {
		t.Errorf("first = %s", r.Recommendations[0].ID)
	}
	if top := Build(fixture(), 2); len(top.Recommendations) != 2 || top.Totals.Recommendations != 2 {
		t.Errorf("limit 2: %d", len(top.Recommendations))
	}
}

func TestBuildCheapestCheck(t *testing.T) {
	got := byID(Build(fixture(), 0))
	for id, want := range map[string]struct {
		has     string
		minutes int
	}{
		"drop:prom:metric:queried": {"Ask alice", 2},
		"drop:prom:metric:owned":   {"Ask team-shop", 5},
		"drop:prom:metric:stale":   {`Ask team-ops whether "Old / panel"`, 5},
		"drop:prom:metric:unread":  {"Search code", 15},
		"drop:prom:metric:swept":   {"Search code", 15},
	} {
		rec := got[id]
		if !strings.Contains(rec.CheapestCheck, want.has) || rec.CheckMinutes != want.minutes {
			t.Errorf("%s: %q (%d min), want %q (%d)", id, rec.CheapestCheck, rec.CheckMinutes, want.has, want.minutes)
		}
	}
}

func TestBuildEvidenceAndRisk(t *testing.T) {
	got := byID(Build(fixture(), 0))

	swept := got["drop:prom:metric:swept"]
	if !strings.Contains(swept.Claim, "only a sweep over every metric name touched metric swept") || !strings.Contains(swept.Claim, "40.00 USD") {
		t.Errorf("claim = %q", swept.Claim)
	}
	if swept.RiskIfWrong.Evidence != "none" || len(swept.Evidence.QueryLog) != 1 || !swept.Evidence.QueryLog[0].Sweep {
		t.Errorf("swept = %+v", swept)
	}

	q := got["drop:prom:metric:queried"]
	if q.Evidence.LastRead == nil || q.Evidence.LastRead.Actor != "alice" || q.RiskIfWrong.Evidence != "observed" {
		t.Errorf("queried = %+v", q)
	}
	if len(q.Downstream) != 1 || q.Downstream[0].Source != SourceQueryLog || !q.Downstream[0].Exact {
		t.Errorf("downstream = %+v", q.Downstream)
	}
	if !strings.Contains(q.Claim, "was only read ad hoc, last on 2026-10-25") {
		t.Errorf("claim = %q", q.Claim)
	}

	stale := got["drop:prom:metric:stale"]
	if stale.Downstream[0].Source != SourceAssumed || stale.Downstream[0].Exact {
		t.Errorf("stale downstream = %+v", stale.Downstream)
	}
	if !contains(stale.Assumptions, "Old / panel is assumed to read it (variable $metric)") {
		t.Errorf("assumptions = %v", stale.Assumptions)
	}

	owned := got["drop:prom:metric:owned"]
	if owned.Owner != "team-shop" || owned.RiskIfWrong.Tier == nil || *owned.RiskIfWrong.Tier != 2 || owned.Status != StatusOpen {
		t.Errorf("owned = %+v", owned)
	}
	// Gaps: the Grafana log covers 40%; the Prometheus log is full.
	if !contains(owned.Evidence.Gaps, "grafana router log covers 40% of the window") || len(owned.Evidence.Gaps) != 1 {
		t.Errorf("gaps = %v", owned.Evidence.Gaps)
	}

	stream := got[`reroute:loki:stream:{app="shop"}`]
	if !contains(stream.Evidence.Gaps, "loki query log not configured") || !stream.Savings.Approximate {
		t.Errorf("stream = %+v", stream)
	}
}

func TestReportGapsAndJSON(t *testing.T) {
	r := Build(fixture(), 0)
	if !contains(r.Gaps, "loki query log not configured") || !contains(r.Gaps, "grafana router log covers 40% of the window") {
		t.Errorf("gaps = %v", r.Gaps)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"version":1`, `"claim"`, `"evidence"`, `"cheapest_check"`, `"check_minutes"`, `"downstream"`,
		`"savings"`, `"risk_if_wrong"`, `"assumptions"`, `"status":"open"`, `"savings_per_minute"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON has no %s", key)
		}
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil || len(back.Recommendations) != len(r.Recommendations) {
		t.Fatalf("round trip: %v", err)
	}
	// An empty report still has arrays, not nulls, for every list.
	empty, _ := json.Marshal(Build(Input{Scan: Scan{StartedAt: at, WindowDays: 30}}, 0))
	if strings.Contains(string(empty), "null") {
		t.Errorf("empty report has a null: %s", empty)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestPct(t *testing.T) {
	for v, want := range map[float64]string{0: "0%", 0.0004: "<0.1%", 0.0023: "0.2%", 0.4: "40%", 1: "100%"} {
		if got := pct(v); got != want {
			t.Errorf("pct(%v) = %q, want %q", v, got, want)
		}
	}
}
