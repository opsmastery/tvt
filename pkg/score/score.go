// Package score ranks telemetry signals by how much they cost relative to how
// much they are used:
//
//	priority = monthly cost / (1 + usage weight) × confidence
//
// Signals with a hard-keep dependency (alert, SLO, declared external consumer,
// or a recording rule feeding one of those) are excluded before scoring.
//
// This package is shared with Legacy Bridge's Value Analyzer; keep it free of
// I/O and backend-specific types.
package score

import (
	"math"
	"sort"
	"time"
)

// Usage is one piece of evidence that a signal is used.
type Usage int

const (
	UsageNone            Usage = iota // no reference anywhere
	UsageStaleDashboard               // on a dashboard nobody viewed in 90 days
	UsageAdhocQuery                   // queried ad hoc (decays with age)
	UsageAgentQuery                   // queried by an AI agent or MCP tool
	UsageViewedDashboard              // on a dashboard viewed in the last 30 days
	UsageHardKeep                     // alert, SLO, external consumer: never recommended
)

// Weights are the usage weights for each kind of evidence. Tunable; the
// defaults map the spec's Low / Medium / High.
type Weights struct {
	StaleDashboard  float64
	AdhocQuery      float64
	AgentQuery      float64
	ViewedDashboard float64
	// AdhocHalfLife is how fast ad hoc and agent query evidence decays.
	AdhocHalfLife time.Duration
}

// DefaultWeights are the starting weights until partner data says otherwise.
var DefaultWeights = Weights{
	StaleDashboard:  0.5,
	AdhocQuery:      2,
	AgentQuery:      2,
	ViewedDashboard: 5,
	AdhocHalfLife:   15 * 24 * time.Hour,
}

// Evidence is one observed use of a signal.
type Evidence struct {
	Usage    Usage
	LastSeen time.Time
}

// Input is everything needed to score one signal.
type Input struct {
	SignalID    string
	MonthlyCost float64
	FirstSeen   time.Time
	Evidence    []Evidence
}

// Context describes the scan the inputs came from.
type Context struct {
	Now         time.Time
	WindowDays  int     // observation window
	LogCoverage float64 // 0..1, share of backends with usable query logs
	Weights     Weights
}

// Result is a scored candidate.
type Result struct {
	SignalID    string
	MonthlyCost float64
	UsageWeight float64
	Confidence  float64
	Priority    float64
}

const (
	minWindowDays = 30 // full confidence needs at least this much history
	newSignalDays = 14 // signals younger than this get reduced confidence
)

// Score returns the result for one signal, and false if the signal is a hard
// keep and must never be recommended.
func Score(in Input, c Context) (Result, bool) {
	w, hard := usageWeight(in.Evidence, c)
	if hard {
		return Result{}, false
	}
	conf := confidence(in.FirstSeen, c)
	return Result{
		SignalID:    in.SignalID,
		MonthlyCost: in.MonthlyCost,
		UsageWeight: w,
		Confidence:  conf,
		Priority:    in.MonthlyCost / (1 + w) * conf,
	}, true
}

// Rank scores all inputs and returns candidates in descending priority,
// truncated to limit (limit <= 0 means no limit).
func Rank(inputs []Input, c Context, limit int) []Result {
	out := make([]Result, 0, len(inputs))
	for _, in := range inputs {
		if r, ok := Score(in, c); ok {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].SignalID < out[j].SignalID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// usageWeight takes the strongest single piece of evidence rather than a sum,
// so many weak references can't outvote "nobody looks at this", and so the
// report can explain a weight with one reference.
func usageWeight(ev []Evidence, c Context) (weight float64, hardKeep bool) {
	for _, e := range ev {
		var w float64
		switch e.Usage {
		case UsageHardKeep:
			return 0, true
		case UsageViewedDashboard:
			w = c.Weights.ViewedDashboard
		case UsageAdhocQuery:
			w = c.Weights.AdhocQuery * decay(e.LastSeen, c)
		case UsageAgentQuery:
			w = c.Weights.AgentQuery * decay(e.LastSeen, c)
		case UsageStaleDashboard:
			w = c.Weights.StaleDashboard
		}
		weight = math.Max(weight, w)
	}
	return weight, false
}

func decay(lastSeen time.Time, c Context) float64 {
	if c.Weights.AdhocHalfLife <= 0 || lastSeen.IsZero() {
		return 1
	}
	age := c.Now.Sub(lastSeen)
	if age <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(age)/float64(c.Weights.AdhocHalfLife))
}

// confidence is lower when the window is short, query logs are incomplete, or
// the signal is new.
func confidence(firstSeen time.Time, c Context) float64 {
	window := clamp01(float64(c.WindowDays) / minWindowDays)
	coverage := clamp01(c.LogCoverage)
	age := 1.0
	if !firstSeen.IsZero() {
		age = clamp01(c.Now.Sub(firstSeen).Hours() / 24 / newSignalDays)
	}
	return window * coverage * age
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}
