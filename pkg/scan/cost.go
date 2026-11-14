package scan

import (
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
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
func priceCosts(in Inputs, costs []graph.Cost) {
	metricPrefix := in.MetricsSource + ":metric:"
	labelPrefix := in.MetricsSource + ":label:"

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

	type priced struct {
		series  int64
		monthly float64
	}
	metrics := map[string]priced{}
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
		if in.PriceCard != nil {
			c.MonthlyCost = in.PriceCard.MetricMonthly(c.ActiveSeries, c.Interval)
		}
		metrics[name] = priced{c.ActiveSeries, c.MonthlyCost}
	}
	if in.PriceCard == nil {
		return
	}
	for i := range costs {
		c := &costs[i]
		rest, ok := strings.CutPrefix(c.SignalID, labelPrefix)
		if !ok {
			continue
		}
		// Label IDs are <source>:label:<metric>:<label>; metric names may
		// hold colons (recording rules), label names never do.
		j := strings.LastIndexByte(rest, ':')
		if j < 0 {
			continue
		}
		m := metrics[rest[:j]]
		c.MonthlyCost = cost.LabelMonthly(m.monthly, m.series, c.ActiveSeries, c.DistinctValues)
	}
}

func familyOf(s *collect.Scrape, series string) (string, bool) {
	if s == nil {
		return "", false
	}
	return s.Family(series)
}
