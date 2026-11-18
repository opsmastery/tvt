package prometheus

import (
	"context"
	"fmt"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

type rulesData struct {
	Groups []struct {
		Name string `json:"name"`
		File string `json:"file"`
		// Interval is the group's effective evaluation interval in seconds
		// (its own, else the global evaluation_interval).
		Interval float64 `json:"interval"`
		// EvaluationTime is how long the group's last evaluation took, in
		// seconds.
		EvaluationTime float64 `json:"evaluationTime"`
		Rules          []struct {
			Name  string `json:"name"`
			Query string `json:"query"`
			Type  string `json:"type"` // "alerting" or "recording"
			// Duration is an alerting rule's for:, in seconds.
			Duration float64 `json:"duration"`
			// Annotations: alerting rules only.
			Annotations map[string]string `json:"annotations"`
			Labels      map[string]string `json:"labels"`
		} `json:"rules"`
	} `json:"groups"`
}

// Rules returns every alerting and recording rule from /api/v1/rules. The
// rule file is reported as the namespace (Mimir reports its namespace there).
//
// An unknown rule type is an error rather than being skipped: a rule tvt
// can't classify might be an alert, and silently dropping it could leave a
// hard keep unprotected.
func (c *Client) Rules(ctx context.Context) ([]collect.Rule, error) {
	var d rulesData
	if _, err := getJSON(ctx, c, "/api/v1/rules", nil, &d); err != nil {
		return nil, err
	}
	var out []collect.Rule
	for _, g := range d.Groups {
		for _, r := range g.Rules {
			var kind collect.RuleKind
			switch r.Type {
			case "alerting":
				kind = collect.RuleAlert
			case "recording":
				kind = collect.RuleRecord
			default:
				return nil, fmt.Errorf("prometheus: /api/v1/rules: group %q rule %q: unknown type %q", g.Name, r.Name, r.Type)
			}
			out = append(out, collect.Rule{
				Source:    c.source(),
				Namespace: g.File,
				Group:     g.Name,
				Name:      r.Name,
				Kind:      kind,
				Expr:      r.Query,
				Language:  "promql",
				Interval:  time.Duration(g.Interval * float64(time.Second)),
				For:       time.Duration(min(max(r.Duration, 0), 86400*30) * float64(time.Second)),
				// Clamped: a hostile or broken API can't overflow it.
				GroupEvaluation: time.Duration(min(max(g.EvaluationTime, 0), 86400) * float64(time.Second)),

				Annotations: nonEmpty(r.Annotations),
				Labels:      nonEmpty(r.Labels),
			})
		}
	}
	return out, nil
}

// nonEmpty returns m, or nil when it has no entries.
func nonEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}
