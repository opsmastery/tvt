package report

import (
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
)

// Reprice turns in into a what-if: its costs priced at card (cost.Reprice,
// with the scan's stored TSDB measurements for a self-hosted card), each
// score's cost and priority recomputed with the same usage weight and
// confidence, and the scan marked RepricedFrom the card it was priced at.
// Evidence, classes and confidence don't depend on price, so nothing else
// changes. in is not modified.
func Reprice(in Input, card cost.PriceCard, tsdb *collect.Storage) (Input, error) {
	if err := card.Validate(); err != nil {
		return in, err
	}
	costs, _, err := cost.Reprice(card, in.Costs, tsdb)
	if err != nil {
		return in, err
	}
	monthly := map[string]float64{}
	for _, c := range costs {
		monthly[c.SignalID] = c.MonthlyCost
	}
	scores := make([]graph.Score, len(in.Scores))
	for i, sc := range in.Scores {
		sc.MonthlyCost = monthly[sc.SignalID]
		sc.Priority = score.Priority(sc.MonthlyCost, sc.UsageWeight, sc.Confidence)
		scores[i] = sc
	}
	out := in
	out.Costs, out.Scores = costs, scores
	out.LineFindings = make([]graph.LineFinding, len(in.LineFindings))
	for i, f := range in.LineFindings {
		f.MonthlyCost = card.LogMonthly(f.BytesPerDay)
		out.LineFindings[i] = f
	}
	from := in.Scan.PriceCard
	if from == "" {
		from = "(none)"
	}
	out.Scan.RepricedFrom = from
	out.Scan.PriceCard, out.Scan.Currency, out.Scan.Approximate = card.Name, card.Currency, card.Preset
	return out, nil
}
