package score

import (
	"math"
	"testing"
	"time"
)

var now = time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)

func ctx() Context {
	return Context{Now: now, WindowDays: 30, LogCoverage: 1, Weights: DefaultWeights}
}

func old() time.Time { return now.AddDate(0, -6, 0) }

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestHardKeepNeverScored(t *testing.T) {
	in := Input{SignalID: "m", MonthlyCost: 1e6, FirstSeen: old(), Evidence: []Evidence{
		{Usage: UsageStaleDashboard},
		{Usage: UsageHardKeep},
	}}
	if _, ok := Score(in, ctx()); ok {
		t.Fatal("hard-keep signal was scored")
	}
	if got := Rank([]Input{in}, ctx(), 0); len(got) != 0 {
		t.Fatalf("hard-keep signal ranked: %+v", got)
	}
}

func TestUnusedPriorityIsCost(t *testing.T) {
	r, ok := Score(Input{SignalID: "m", MonthlyCost: 120, FirstSeen: old()}, ctx())
	if !ok || !approx(r.Priority, 120) || r.UsageWeight != 0 || r.Confidence != 1 {
		t.Fatalf("got %+v", r)
	}
}

func TestViewedDashboardOutweighsStale(t *testing.T) {
	c := ctx()
	viewed, _ := Score(Input{MonthlyCost: 100, FirstSeen: old(), Evidence: []Evidence{{Usage: UsageViewedDashboard}}}, c)
	stale, _ := Score(Input{MonthlyCost: 100, FirstSeen: old(), Evidence: []Evidence{{Usage: UsageStaleDashboard}}}, c)
	if !approx(viewed.Priority, 100/6.0) || !approx(stale.Priority, 100/1.5) {
		t.Fatalf("viewed=%v stale=%v", viewed.Priority, stale.Priority)
	}
}

func TestStrongestEvidenceWins(t *testing.T) {
	r, _ := Score(Input{MonthlyCost: 10, FirstSeen: old(), Evidence: []Evidence{
		{Usage: UsageStaleDashboard}, {Usage: UsageViewedDashboard}, {Usage: UsageStaleDashboard},
	}}, ctx())
	if r.UsageWeight != DefaultWeights.ViewedDashboard {
		t.Fatalf("weight = %v, want %v", r.UsageWeight, DefaultWeights.ViewedDashboard)
	}
}

func TestAdhocDecay(t *testing.T) {
	c := ctx()
	fresh, _ := Score(Input{MonthlyCost: 10, FirstSeen: old(), Evidence: []Evidence{{Usage: UsageAdhocQuery, LastSeen: now}}}, c)
	halfLife, _ := Score(Input{MonthlyCost: 10, FirstSeen: old(), Evidence: []Evidence{{Usage: UsageAdhocQuery, LastSeen: now.Add(-c.Weights.AdhocHalfLife)}}}, c)
	if !approx(fresh.UsageWeight, 2) || !approx(halfLife.UsageWeight, 1) {
		t.Fatalf("fresh=%v halfLife=%v", fresh.UsageWeight, halfLife.UsageWeight)
	}
}

func TestConfidence(t *testing.T) {
	tests := []struct {
		name      string
		window    int
		coverage  float64
		firstSeen time.Time
		want      float64
		held      string
	}{
		{"full", 30, 1, old(), 1, ""},
		{"long window caps at 1", 90, 1, old(), 1, ""},
		{"short window recommends nothing", 29, 1, old(), 0, HeldWindow},
		{"half coverage", 30, 0.5, old(), 0.5, ""},
		{"no coverage", 30, 0, old(), 0, HeldCoverage},
		{"new signal", 30, 1, now.AddDate(0, 0, -7), 0.5, ""},
		{"first seen now", 30, 1, now, 0, HeldNew},
		{"unknown first seen", 30, 1, time.Time{}, 1, ""},
		{"compound", 30, 0.5, now.AddDate(0, 0, -7), 0.25, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ctx()
			c.WindowDays, c.LogCoverage = tt.window, tt.coverage
			r, _ := Score(Input{MonthlyCost: 1, FirstSeen: tt.firstSeen}, c)
			if !approx(r.Confidence, tt.want) || r.Held != tt.held {
				t.Fatalf("confidence = %v held %q, want %v %q", r.Confidence, r.Held, tt.want, tt.held)
			}
		})
	}
}

func TestMinWindowDays(t *testing.T) {
	c := ctx()
	c.WindowDays, c.MinWindowDays = 14, 7
	if r, _ := Score(Input{MonthlyCost: 1}, c); r.Confidence != 1 {
		t.Errorf("14d window with window_days 7: %+v", r)
	}
	c.MinWindowDays = 60
	c.WindowDays = 45
	if r, _ := Score(Input{MonthlyCost: 1}, c); r.Confidence != 0 || r.Held != HeldWindow {
		t.Errorf("45d window with window_days 60: %+v", r)
	}
}

func TestPerSignalCoverage(t *testing.T) {
	c := ctx()
	c.LogCoverage = 1
	half := 0.5
	r, _ := Score(Input{MonthlyCost: 10, Coverage: &half}, c)
	if !approx(r.Confidence, 0.5) || !approx(r.Priority, 5) {
		t.Fatalf("got %+v", r)
	}
}

func TestDeclaredNeverDecays(t *testing.T) {
	c := ctx()
	longAgo := now.AddDate(-1, 0, 0)
	for _, class := range []EvidenceClass{EvidenceDeclared, EvidenceIncident} {
		r, _ := Score(Input{MonthlyCost: 10, Evidence: []Evidence{{Usage: UsageAdhocQuery, Class: class, LastSeen: longAgo}}}, c)
		if !approx(r.UsageWeight, 2) {
			t.Errorf("%s ad hoc read a year ago weighs %v, want 2", class, r.UsageWeight)
		}
	}
	r, _ := Score(Input{MonthlyCost: 10, Evidence: []Evidence{{Usage: UsageRecordingRule, LastSeen: longAgo}}}, c)
	if !approx(r.UsageWeight, DefaultWeights.RecordingRule) || r.Strongest != UsageRecordingRule {
		t.Errorf("recording rule: %+v", r)
	}
}

func TestObservedDashboardViewDecays(t *testing.T) {
	c := ctx()
	r, _ := Score(Input{MonthlyCost: 10, Evidence: []Evidence{
		{Usage: UsageViewedDashboard, Class: EvidenceObserved, LastSeen: now.Add(-c.Weights.AdhocHalfLife)},
	}}, c)
	if !approx(r.UsageWeight, 2.5) {
		t.Fatalf("weight = %v, want 2.5", r.UsageWeight)
	}
}

// A last-seen date kept forever still weighs something, but less and less:
// "read nine months ago" never becomes "never read", and never protects.
func TestOldReadKeepsAFadingWeight(t *testing.T) {
	r, _ := Score(Input{MonthlyCost: 10, Evidence: []Evidence{
		{Usage: UsageAdhocQuery, Class: EvidenceObserved, LastSeen: now.AddDate(0, -9, 0)},
	}}, ctx())
	if r.UsageWeight <= 0 || r.UsageWeight > 0.01 || r.Strongest != UsageAdhocQuery {
		t.Fatalf("got %+v", r)
	}
}

func TestSweepIsWeak(t *testing.T) {
	c := ctx()
	sweep, _ := Score(Input{MonthlyCost: 10, Evidence: []Evidence{{Usage: UsageSweep, LastSeen: now}}}, c)
	stale, _ := Score(Input{MonthlyCost: 10, Evidence: []Evidence{{Usage: UsageStaleDashboard}}}, c)
	if !approx(sweep.UsageWeight, 0.25) || sweep.UsageWeight >= stale.UsageWeight || sweep.Strongest != UsageSweep {
		t.Fatalf("sweep %+v stale %+v", sweep, stale)
	}
	// A sweep next to a real read changes nothing.
	both, _ := Score(Input{MonthlyCost: 10, Evidence: []Evidence{{Usage: UsageSweep, LastSeen: now}, {Usage: UsageAdhocQuery, LastSeen: now}}}, c)
	if !approx(both.UsageWeight, 2) || both.Strongest != UsageAdhocQuery {
		t.Fatalf("sweep and read: %+v", both)
	}
}

func TestUsageString(t *testing.T) {
	for u := UsageNone; u <= UsageRecordingRule; u++ {
		if u.String() == "unknown" {
			t.Errorf("usage %d has no name", u)
		}
	}
}

func TestRankOrderAndLimit(t *testing.T) {
	inputs := []Input{
		{SignalID: "cheap-unused", MonthlyCost: 10, FirstSeen: old()},
		{SignalID: "pricey-viewed", MonthlyCost: 600, FirstSeen: old(), Evidence: []Evidence{{Usage: UsageViewedDashboard}}},
		{SignalID: "pricey-unused", MonthlyCost: 500, FirstSeen: old()},
		{SignalID: "alerting", MonthlyCost: 9999, FirstSeen: old(), Evidence: []Evidence{{Usage: UsageHardKeep}}},
	}
	got := Rank(inputs, ctx(), 2)
	if len(got) != 2 || got[0].SignalID != "pricey-unused" || got[1].SignalID != "pricey-viewed" {
		t.Fatalf("got %+v", got)
	}
}
