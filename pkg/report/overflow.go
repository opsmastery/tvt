package report

import (
	"fmt"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
)

// OverflowRow is one metric with OpenTelemetry overflow series (S6 item 5,
// version 10): an evidence gap, never a recommendation or a total.
type OverflowRow struct {
	Signal  string           `json:"signal"`
	Metric  string           `json:"metric"`
	Series  int              `json:"series"`
	Capped  []string         `json:"capped"`
	Readers []OverflowReader `json:"readers"`
	Detail  string           `json:"detail"`
	Fix     string           `json:"fix"`
}

// OverflowReader is a consumer that filters or groups on a capped label.
type OverflowReader struct {
	Consumer string   `json:"consumer"`
	Labels   []string `json:"labels"`
}

// overflow builds the overflow rows and marks every recommendation and
// confirm row on an overflowing metric (or one of its labels): its cost is
// a floor, and the row carries the overflow gap.
func overflow(findings []graph.OverflowFinding, recs []Recommendation, confirm []Confirm, gs *gaps) []OverflowRow {
	out := []OverflowRow{}
	gapOf := map[string]Gap{}
	for _, f := range findings {
		metric := f.Signal
		if i := strings.Index(f.Signal, ":metric:"); i >= 0 {
			metric = f.Signal[i+len(":metric:"):]
		}
		row := OverflowRow{Signal: f.Signal, Metric: metric, Series: f.Series, Capped: f.Capped, Readers: []OverflowReader{}}
		if row.Capped == nil {
			row.Capped = []string{}
		}
		for _, r := range f.Readers {
			c, ls, _ := strings.Cut(r, "\t")
			row.Readers = append(row.Readers, OverflowReader{Consumer: c, Labels: strings.Split(ls, ",")})
		}
		row.Detail = fmt.Sprintf("%d overflow series (otel.metric.overflow=\"true\"): the SDK or Collector hit its cardinality limit, so the metric has more series than the backend shows and those points lost %s",
			f.Series, labelList(row.Capped))
		if len(row.Readers) > 0 {
			row.Detail += fmt.Sprintf("; %d reader(s) filter or group on a capped label and miss or misplace them", len(row.Readers))
		}
		row.Fix = "raise the cardinality limit or drop the high-cardinality attribute before aggregation; until then treat this metric's cost as a floor and its per-label breakdowns as partial"
		out = append(out, row)
		gapOf[f.Signal] = gs.ack(Gap{Source: scan.SourceOverflow, Kind: GapOverflow,
			Text: fmt.Sprintf("%s: %s: %d overflow series (otel.metric.overflow): real cardinality is higher than shown, cost is a floor; capped labels %s",
				scan.SourceOverflow, metric, f.Series, strings.Join(row.Capped, ", "))})
	}
	// metricOf maps a row's signal to the overflowing metric it belongs to.
	metricOf := func(id string) (Gap, bool) {
		if g, ok := gapOf[id]; ok {
			return g, true
		}
		for sig, g := range gapOf {
			src, name, _ := strings.Cut(sig, ":metric:")
			if strings.HasPrefix(id, src+":label:"+name+":") {
				return g, true
			}
		}
		return Gap{}, false
	}
	const floor = "cost is a floor: the metric has overflow series, so its real cardinality and cost are higher than the backend shows"
	for i := range recs {
		r := &recs[i]
		if g, ok := metricOf(r.Signal.ID); ok {
			r.CostFloor = true
			r.Assumptions = append(r.Assumptions, floor)
			o, a := split([]Gap{g})
			r.Evidence.Gaps, r.Evidence.Acknowledged = append(r.Evidence.Gaps, o...), append(r.Evidence.Acknowledged, a...)
		}
	}
	for i := range confirm {
		c := &confirm[i]
		if g, ok := metricOf(c.Signal.ID); ok {
			c.CostFloor = true
			o, a := split([]Gap{g})
			c.Gaps, c.Acknowledged = append(c.Gaps, o...), append(c.Acknowledged, a...)
		}
	}
	return out
}

func labelList(ls []string) string {
	if len(ls) == 0 {
		return "their labels"
	}
	return "labels " + strings.Join(ls, ", ")
}
