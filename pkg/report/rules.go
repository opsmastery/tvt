package report

import (
	"sort"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// Rule is one rule's cost and findings (S6 item 1, version 6). Report
// only: tvt never edits a rule. Cost is the measured work of its
// evaluations, samples (PromQL) or bytes (LogQL) per day; no price card
// prices query work yet, so rules carry no money figure.
type Rule struct {
	ID          string  `json:"id"` // the rule's consumer ID
	Name        string  `json:"name"`
	Source      string  `json:"source"` // prometheus, mimir, loki, grafana
	Kind        string  `json:"kind"`   // alert or record
	EvaluatedBy string  `json:"evaluated_by,omitempty"`
	IntervalSec float64 `json:"interval_seconds"`
	Expr        string  `json:"expr"`
	// EvalsPerDay is 0 when the interval is unknown; the per-evaluation
	// figures are means over Evaluations logged evaluations.
	EvalsPerDay    float64 `json:"evals_per_day"`
	SamplesPerEval float64 `json:"samples_per_eval"`
	BytesPerEval   float64 `json:"bytes_per_eval"`
	SamplesPerDay  float64 `json:"samples_per_day"`
	BytesPerDay    float64 `json:"bytes_per_day"`
	Evaluations    int64   `json:"evaluations"`
	// CostUnknown says why the daily cost is unknown (never zero).
	CostUnknown string `json:"cost_unknown,omitempty"`
	// Alerts (S8 item 5): severity, for, and the worst-case detection
	// delay with its terms, at the scan's scrape intervals.
	Severity          string        `json:"severity,omitempty"`
	ForSec            float64       `json:"for_seconds,omitempty"`
	DetectionDelaySec float64       `json:"detection_delay_seconds,omitempty"`
	DetectionTerms    string        `json:"detection_terms,omitempty"`
	Findings          []RuleFinding `json:"findings"`
}

// RuleFinding is one thing wrong with a rule, with its fix.
type RuleFinding struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

// rules joins each rule's cost and findings, rules with findings first,
// then by daily samples and bytes, unknown costs last.
func rules(consumers map[string]graph.Consumer, costs []graph.RuleCost, findings []graph.RuleFinding) ([]Rule, int) {
	byRule := map[string][]RuleFinding{}
	for _, f := range findings {
		byRule[f.ConsumerID] = append(byRule[f.ConsumerID], RuleFinding{Kind: f.Kind, Detail: f.Detail, Fix: f.Fix})
	}
	out := make([]Rule, 0, len(costs))
	for _, c := range costs {
		con := consumers[c.ConsumerID]
		kind := "alert"
		if con.Kind == graph.ConsumerRecordingRule {
			kind = "record"
		}
		src := strings.SplitN(strings.TrimPrefix(c.ConsumerID, "rule:"), ":", 2)[0]
		r := Rule{
			ID: c.ConsumerID, Name: con.Name, Source: src, Kind: kind, EvaluatedBy: con.EvaluatedBy,
			IntervalSec: con.Interval.Seconds(), Expr: con.Expr,
			EvalsPerDay: c.EvalsPerDay, SamplesPerEval: c.SamplesPerEval, BytesPerEval: c.BytesPerEval,
			SamplesPerDay: c.SamplesPerDay(), BytesPerDay: c.BytesPerDay(), Evaluations: c.Evaluations,
			CostUnknown: c.Unknown, Findings: byRule[c.ConsumerID],
		}
		if r.Findings == nil {
			r.Findings = []RuleFinding{}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (len(a.Findings) > 0) != (len(b.Findings) > 0) {
			return len(a.Findings) > 0
		}
		if (a.CostUnknown == "") != (b.CostUnknown == "") {
			return a.CostUnknown == ""
		}
		if a.SamplesPerDay != b.SamplesPerDay {
			return a.SamplesPerDay > b.SamplesPerDay
		}
		if a.BytesPerDay != b.BytesPerDay {
			return a.BytesPerDay > b.BytesPerDay
		}
		return a.ID < b.ID
	})
	return out, len(findings)
}
