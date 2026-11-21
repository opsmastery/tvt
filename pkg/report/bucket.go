package report

import (
	"fmt"
	"sort"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
)

// ActionBucket: keep a label but bucket its values to their class (500
// and 503 to 500), when every reader of it tells only classes apart
// (S10 item 1, ADR 0010: bucketing comes before label removal).
const ActionBucket = "bucket"

// bucketClasses is how many value classes a bucketed label keeps at most:
// status codes have five (1xx to 5xx).
const bucketClasses = 5

// bucketRows recommends bucketing for every read label whose readers all
// use it coarsely (graph.LabelCoarse, as in code=~"5.."): a label with
// any exact or grouping read, a reader of every label, an assumed
// reference, or an alert or viewed panel that keeps the label in its
// result would see the bucketed values, so it gets none. Bucketing merges
// series, so a row says how many and is a candidate for writers that sum
// them (a Collector's ordered pipeline, stream aggregation), not a
// relabel rule.
func bucketRows(signals []graph.Signal, refs map[string][]graph.Reference, consumers map[string]graph.Consumer,
	costs map[string]graph.Cost, gs *gaps, listed map[string]bool, meta Scan) []Recommendation {
	var out []Recommendation
	for _, s := range signals {
		if s.Kind != graph.KindLabel || s.Record != "" || s.Class == string(score.ClassCandidate) || s.Class == "" {
			continue
		}
		c := costs[s.ID]
		// The values left once bucketed: measured when the scan read the
		// values, else at most bucketClasses.
		after := int64(bucketClasses)
		if c.Classes > 0 {
			after = c.Classes
		}
		if c.DistinctValues <= after || c.MonthlyCost <= 0 {
			continue
		}
		coarse, blocked := 0, ""
		var readers []Reader
		for _, ref := range refs[s.ID] {
			if ref.Sweep {
				continue
			}
			con := consumers[ref.ConsumerID]
			switch {
			case ref.AllLabels:
				blocked = con.Name + " may read every label"
			case ref.Assumed:
				blocked = con.Name + " is an assumed reader"
			}
			for _, l := range ref.Labels {
				if l.Use == graph.LabelCoarse {
					coarse++
				} else {
					blocked = fmt.Sprintf("%s reads it as %s", con.Name, l.Use)
				}
			}
			readers = append(readers, Reader{Consumer: con.ID, Kind: string(con.Kind), Name: con.Name, Evidence: string(ref.Evidence)})
			if blocked != "" {
				break
			}
		}
		if blocked != "" || coarse == 0 {
			continue
		}
		share := 1 - float64(after)/float64(c.DistinctValues)
		rec := Recommendation{
			ID:     ActionBucket + ":" + s.ID,
			Action: ActionBucket,
			Signal: SignalRef{ID: s.ID, Kind: string(s.Kind), Name: s.Name, Parent: s.Parent, Source: s.Source},
			Claim: fmt.Sprintf("Every reader of %s on %s tells only its classes apart (like %s=~\"5..\"): bucketing its %d values to at most %d classes (503 to 500) keeps their results and merges series.",
				s.Name, s.Parent, s.Name, c.DistinctValues, after),
			Evidence:    Evidence{References: []string{}, QueryLog: []LoggedQuery{}},
			Downstream:  readers,
			Assumptions: []string{"the values are status-code-like: a leading digit names the class; tvt buckets to that digit and 00, which every coarse reader's pattern still matches"},
			Owner:       s.Owner, OwnerSource: s.OwnerSource, OwnerInferred: s.OwnerInferred,
			Status: StatusOpen,
			Savings: Savings{Monthly: c.MonthlyCost * share, Currency: meta.Currency, DiskBytes: int64(float64(c.DiskBytes) * share),
				Approximate: true},
			RiskIfWrong: Risk{Evidence: "coarse", Tier: s.Tier},
			Removal:     "bucket",
			// Series that collide once bucketed, estimated as even
			// across the values.
			Merges: c.ActiveSeries - int64(float64(c.ActiveSeries)*float64(after)/float64(c.DistinctValues)),
		}
		for _, ref := range refs[s.ID] {
			if !ref.Sweep {
				rec.Evidence.References = append(rec.Evidence.References, ref.ConsumerID)
			}
		}
		sort.Strings(rec.Evidence.References)
		rec.Evidence.Gaps, rec.Evidence.Acknowledged = split(gs.forSignal(s, listed))
		if rec.Evidence.Gaps == nil {
			rec.Evidence.Gaps = []string{}
		}
		rec.CheapestCheck = "Check that no reader needs the exact value (an exact match, a grouping or a legend would): tvt why " + s.Parent + "{" + s.Name + "}"
		rec.CheckMinutes = 5
		rec.EvidenceHash = EvidenceHash(rec.Evidence)
		if rec.CheckMinutes > 0 {
			rec.SavingsPerMinute = rec.Savings.Monthly / float64(rec.CheckMinutes)
		}
		out = append(out, rec)
	}
	return out
}
