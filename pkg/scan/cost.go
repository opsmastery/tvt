package scan

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// priceCosts fills each metric and label cost row in place: the interval a
// metric's samples are counted at, a histogram's bucket count, and, with a
// price card, the monthly cost.
//
// A metric's interval is its recording rule's evaluation interval, else
// the shortest scrape interval of the jobs exposing it (target metadata),
// else an assumption: the shortest job interval for a scraped series no
// metadata names (up, scrape_*), or DefaultInterval without a targets API.
// A label costs the share of its metric's series it multiplies
// (cost.LabelMonthly).
//
// A self_hosted card prices the disk each metric holds over retention
// (cost.Price; inputs from cost.ResolveStorage), which it returns for the
// scan row.
func priceCosts(in Inputs, costs []graph.Cost) (*cost.Storage, error) {
	metricPrefix := in.MetricsSource + ":metric:"

	ruleInterval := map[string]time.Duration{}
	for _, r := range in.Rules {
		if r.Kind == collect.RuleRecord && r.Interval > 0 {
			ruleInterval[r.Name] = r.Interval
		}
	}
	buckets := map[string]int64{}
	for _, l := range in.Labels {
		if l.Label == "le" {
			buckets[l.Metric] = l.Values
		}
	}
	var shortestJob time.Duration
	if in.Scrape != nil {
		for _, d := range in.Scrape.JobIntervals {
			if shortestJob == 0 || d < shortestJob {
				shortestJob = d
			}
		}
	}

	for i := range costs {
		c := &costs[i]
		name, ok := strings.CutPrefix(c.SignalID, metricPrefix)
		if !ok {
			continue
		}
		switch fam, known := familyOf(in.Scrape, name); {
		case ruleInterval[name] > 0:
			c.Interval = ruleInterval[name]
		case known && in.Scrape.Intervals[fam] > 0:
			c.Interval = in.Scrape.Intervals[fam]
			if in.Scrape.Types[fam] == "histogram" && strings.HasSuffix(name, "_bucket") {
				c.Buckets = buckets[name]
				c.Exemplars = in.Scrape.Exemplars
			}
		case shortestJob > 0:
			c.Interval = shortestJob
			c.IntervalAssumed = "no target metadata names it; the shortest job interval"
		default:
			c.Interval = in.DefaultInterval
			c.IntervalAssumed = in.DefaultIntervalReason
		}
	}
	if in.PriceCard == nil {
		return nil, nil
	}
	return cost.Price(*in.PriceCard, costs, in.Storage)
}

const streamPrefix = "loki:stream:"

// streamCosts gives every stream signal the bytes per day of the Loki
// streams its selector matches (every stream for AllStreams), and returns
// each stream's own volume. Selectors overlap, so stream signal bytes
// aren't additive: the per-stream volumes are.
func streamCosts(vols []collect.StreamVolume, signals []graph.Signal, at time.Time) ([]graph.Cost, []graph.StreamVolume, error) {
	perDay := make([]int64, len(vols))
	out := make([]graph.StreamVolume, len(vols))
	for i, v := range vols {
		if v.Window <= 0 {
			return nil, nil, fmt.Errorf("scan: stream volume %v: no window", v.Labels)
		}
		perDay[i] = int64(float64(v.Bytes) * float64(24*time.Hour) / float64(v.Window))
		ms := make([]parse.Matcher, 0, len(v.Labels))
		for k, val := range v.Labels {
			ms = append(ms, parse.Matcher{Label: k, Op: parse.MatchEqual, Value: val})
		}
		out[i] = graph.StreamVolume{Selector: parse.CanonicalSelector(ms), BytesPerDay: perDay[i]}
	}
	var costs []graph.Cost
	for _, s := range signals {
		if s.Kind != graph.KindStream {
			continue
		}
		// Volume is a cost input, not protection: a selector tvt can't
		// evaluate (a regex Go won't compile) gets no cost row rather than
		// failing the scan.
		var matchers []parse.Matcher
		if s.Name != AllStreams {
			refs, err := parse.LogQL(s.Name)
			if err != nil || len(refs) != 1 {
				continue
			}
			matchers = refs[0].Matchers
		}
		var total int64
		ok := true
		for i, v := range vols {
			match, err := parse.MatchesLabels(matchers, v.Labels)
			if err != nil {
				ok = false
				break
			}
			if match {
				total += perDay[i]
			}
		}
		if ok {
			costs = append(costs, graph.Cost{SignalID: s.ID, BytesPerDay: total, ObservedAt: at})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Selector < out[j].Selector })
	return costs, out, nil
}

func familyOf(s *collect.Scrape, series string) (string, bool) {
	if s == nil {
		return "", false
	}
	return s.Family(series)
}
