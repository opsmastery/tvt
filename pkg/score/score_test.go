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
	}{
		{"full", 30, 1, old(), 1},
		{"long window caps at 1", 90, 1, old(), 1},
		{"short window", 15, 1, old(), 0.5},
		{"half coverage", 30, 0.5, old(), 0.5},
		{"new signal", 30, 1, now.AddDate(0, 0, -7), 0.5},
		{"unknown first seen", 30, 1, time.Time{}, 1},
		{"compound", 15, 0.5, now.AddDate(0, 0, -7), 0.125},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ctx()
			c.WindowDays, c.LogCoverage = tt.window, tt.coverage
			r, _ := Score(Input{MonthlyCost: 1, FirstSeen: tt.firstSeen}, c)
			if !approx(r.Confidence, tt.want) {
				t.Fatalf("confidence = %v, want %v", r.Confidence, tt.want)
			}
		})
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
