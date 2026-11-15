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
	// UsageSweep: a query over every metric name touched it (an agent's
	// inventory sweep). Weak: it says the signal exists, not that anyone
	// needs it.
	UsageSweep
	// UsageRecordingRule: a recording rule reads it, but nothing that
	// protects the signal reads the rule's output. Declared, so it never
	// decays.
	UsageRecordingRule
)

// String names the usage for reports and the store.
func (u Usage) String() string {
	switch u {
	case UsageNone:
		return "none"
	case UsageStaleDashboard:
		return "stale_dashboard"
	case UsageAdhocQuery:
		return "adhoc_query"
	case UsageAgentQuery:
		return "agent_query"
	case UsageViewedDashboard:
		return "viewed_dashboard"
	case UsageHardKeep:
		return "hard_keep"
	case UsageSweep:
		return "sweep"
	case UsageRecordingRule:
		return "recording_rule"
	}
	return "unknown"
}

// EvidenceClass is how a piece of evidence was established. It decides
// whether the evidence decays: something that evaluates the signal by
// definition (declared) or read it during an incident keeps its weight;
// someone seen using it (observed) fades with the half-life.
type EvidenceClass string

const (
	EvidenceDeclared EvidenceClass = "declared"
	EvidenceIncident EvidenceClass = "incident"
	EvidenceObserved EvidenceClass = "observed"
)

// Weights are the usage weights for each kind of evidence. Tunable; the
// defaults map the spec's Low / Medium / High.
type Weights struct {
	StaleDashboard  float64
	AdhocQuery      float64
	AgentQuery      float64
	ViewedDashboard float64
	Sweep           float64
	RecordingRule   float64
	// AdhocHalfLife is how fast observed evidence decays (ad hoc and agent
	// queries, dashboard views): its weight halves every AdhocHalfLife
	// since it was last seen.
	AdhocHalfLife time.Duration
}

// DefaultWeights are the starting weights until partner data says otherwise.
var DefaultWeights = Weights{
	StaleDashboard:  0.5,
	AdhocQuery:      2,
	AgentQuery:      2,
	ViewedDashboard: 5,
	Sweep:           0.25,
	RecordingRule:   1,
	AdhocHalfLife:   15 * 24 * time.Hour,
}

// Evidence is one use of a signal.
type Evidence struct {
	Usage Usage
	// Class decides decay: only observed evidence decays, from LastSeen.
	// Empty means observed for ad hoc and agent queries and sweeps, and
	// declared for everything else.
	Class EvidenceClass
	// LastSeen is when the use was last seen; zero when unknown (no decay).
	LastSeen time.Time
}

// Input is everything needed to score one signal.
type Input struct {
	SignalID    string
	MonthlyCost float64
	// FirstSeen is when the signal first appeared; zero when unknown (it
	// predates every observation), which costs no confidence.
	FirstSeen time.Time
	Evidence  []Evidence
	// Coverage is how complete the usage evidence that could show this
	// signal being read was, 0..1 (the query logs of its backend and the
	// logs that show dashboard views). Nil means Context.LogCoverage.
	Coverage *float64
}

// Context describes the scan the inputs came from.
type Context struct {
	Now        time.Time
	WindowDays int // observation window of the scan, in days
	// MinWindowDays is the shortest window anything is recommended from
	// (tvt.yaml window_days). A shorter window gives every signal
	// confidence 0. Zero means DefaultMinWindowDays.
	MinWindowDays int
	LogCoverage   float64 // 0..1, used when an Input has no Coverage
	Weights       Weights
}

// Result is a scored candidate.
type Result struct {
	SignalID    string
	MonthlyCost float64
	UsageWeight float64
	// Strongest is the evidence the weight came from (UsageNone when
	// there is none), so a report can explain the weight with one use.
	Strongest  Usage
	Confidence float64
	Priority   float64
	// Held says why Confidence is 0 when it is: HeldWindow or HeldCoverage.
	Held string
}

const (
	// DefaultMinWindowDays is window_days' default.
	DefaultMinWindowDays = 30
	newSignalDays        = 14 // signals younger than this get reduced confidence

	// HeldWindow: the scan's window is shorter than MinWindowDays.
	HeldWindow = "window"
	// HeldCoverage: no usage evidence could have shown the signal read.
	HeldCoverage = "coverage"
	// HeldNew: the signal first appeared at the scan (no history at all).
	HeldNew = "new"
)

// Score returns the result for one signal, and false if the signal is a hard
// keep and must never be recommended.
func Score(in Input, c Context) (Result, bool) {
	w, strongest, hard := usageWeight(in.Evidence, c)
	if hard {
		return Result{}, false
	}
	conf, held := confidence(in, c)
	return Result{
		SignalID:    in.SignalID,
		MonthlyCost: in.MonthlyCost,
		UsageWeight: w,
		Strongest:   strongest,
		Confidence:  conf,
		Priority:    Priority(in.MonthlyCost, w, conf),
		Held:        held,
	}, true
}

// Priority is a scored signal's rank: monthly cost discounted by its usage
// weight and scaled by confidence. Repricing a stored score at another
// card recomputes it from the new cost with the same weight and confidence.
func Priority(monthlyCost, usageWeight, confidence float64) float64 {
	return monthlyCost / (1 + usageWeight) * confidence
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
func usageWeight(ev []Evidence, c Context) (weight float64, strongest Usage, hardKeep bool) {
	for _, e := range ev {
		var w float64
		switch e.Usage {
		case UsageHardKeep:
			return 0, UsageHardKeep, true
		case UsageViewedDashboard:
			w = c.Weights.ViewedDashboard
		case UsageAdhocQuery:
			w = c.Weights.AdhocQuery
		case UsageAgentQuery:
			w = c.Weights.AgentQuery
		case UsageStaleDashboard:
			w = c.Weights.StaleDashboard
		case UsageSweep:
			w = c.Weights.Sweep
		case UsageRecordingRule:
			w = c.Weights.RecordingRule
		}
		if decays(e) {
			w *= decay(e.LastSeen, c)
		}
		if w > weight {
			weight, strongest = w, e.Usage
		}
	}
	return weight, strongest, false
}

// decays reports whether e's weight fades with age: observed evidence
// does, declared and incident evidence never does.
func decays(e Evidence) bool {
	switch e.Class {
	case EvidenceObserved:
		return true
	case EvidenceDeclared, EvidenceIncident:
		return false
	}
	return e.Usage == UsageAdhocQuery || e.Usage == UsageAgentQuery || e.Usage == UsageSweep
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

// confidence is how far the evidence can be trusted to show every reader:
// 0 when the window is shorter than MinWindowDays (nothing is recommended
// from less), otherwise the coverage of the sources that could have shown
// the signal read, reduced for a signal younger than newSignalDays.
func confidence(in Input, c Context) (float64, string) {
	minDays := c.MinWindowDays
	if minDays <= 0 {
		minDays = DefaultMinWindowDays
	}
	if c.WindowDays < minDays {
		return 0, HeldWindow
	}
	coverage := c.LogCoverage
	if in.Coverage != nil {
		coverage = *in.Coverage
	}
	coverage = clamp01(coverage)
	if coverage == 0 {
		return 0, HeldCoverage
	}
	age := 1.0
	if !in.FirstSeen.IsZero() {
		age = clamp01(c.Now.Sub(in.FirstSeen).Hours() / 24 / newSignalDays)
		if age == 0 {
			return 0, HeldNew
		}
	}
	return coverage * age, ""
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}
