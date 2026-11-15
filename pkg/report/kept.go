package report

import (
	"sort"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
)

// Unowned names the service of signals no tvt.yaml owner claims.
const Unowned = "(unowned)"

// Service is one service's "evidence kept": what tvt would cut next to
// what stays protected, so an owner sees the cut in proportion.
type Service struct {
	Name string `json:"name"`
	// Cut is every recommendation for the service (not only the top N).
	Cut Tally `json:"cut"`
	// Kept is what stays: hard keeps, keep_high, dormant signals, signals
	// with incident evidence, and records (S7; zero until then).
	HardKeep Tally `json:"hard_keep"`
	KeepHigh Tally `json:"keep_high"`
	Dormant  Tally `json:"dormant"`
	Incident Tally `json:"incident"`
	Records  Tally `json:"records"`
}

// Tally counts signals and their monthly cost. Kept tallies count metrics
// and streams; a label's cost is part of its metric's.
type Tally struct {
	Signals int     `json:"signals"`
	Monthly float64 `json:"monthly"`
}

func (t *Tally) add(cost float64) {
	t.Signals++
	t.Monthly += cost
}

// services groups recommendations and protected signals by owner, largest
// cut first, unowned last among equals.
func services(signals []graph.Signal, recs []Recommendation, refs map[string][]graph.Reference, costs map[string]graph.Cost) []Service {
	by := map[string]*Service{}
	get := func(owner string) *Service {
		if owner == "" {
			owner = Unowned
		}
		s := by[owner]
		if s == nil {
			s = &Service{Name: owner}
			by[owner] = s
		}
		return s
	}
	for _, rec := range recs {
		get(rec.Owner).Cut.add(rec.Savings.Monthly)
	}
	for _, s := range signals {
		if s.Kind == graph.KindLabel {
			continue
		}
		cost := costs[s.ID].MonthlyCost
		svc := get(s.Owner)
		switch score.Class(s.Class) {
		case score.ClassHardKeep:
			svc.HardKeep.add(cost)
		case score.ClassKeepHigh:
			svc.KeepHigh.add(cost)
		case score.ClassDormant:
			svc.Dormant.add(cost)
		}
		for _, r := range refs[s.ID] {
			if r.Evidence == graph.EvidenceIncident {
				svc.Incident.add(cost)
				break
			}
		}
	}
	out := make([]Service, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cut.Monthly != out[j].Cut.Monthly {
			return out[i].Cut.Monthly > out[j].Cut.Monthly
		}
		if (out[i].Name == Unowned) != (out[j].Name == Unowned) {
			return out[j].Name == Unowned
		}
		return out[i].Name < out[j].Name
	})
	return out
}
