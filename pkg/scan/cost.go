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

// summable reports whether a metric's series can be summed when a label is
// dropped (S6 item 7): a counter or histogram by target metadata, else by
// the exposition suffixes counters and histograms carry (_total, _bucket,
// _sum, _count), for series no metadata names (an Alloy or OTLP pipeline
// gives Prometheus none). A gauge, summary quantile or unknown type isn't.
func summable(s *collect.Scrape, series string) bool {
	if fam, ok := familyOf(s, series); ok {
		switch s.Types[fam] {
		case "counter", "histogram":
			return true
		case "summary":
			return strings.HasSuffix(series, "_sum") || strings.HasSuffix(series, "_count")
		case "gauge":
			return false
		}
	}
	for _, suf := range []string{"_total", "_bucket", "_sum", "_count"} {
		if strings.HasSuffix(series, suf) {
			return true
		}
	}
	return false
}

// unreadStreams are signals for the streams the volume API listed that no
// queried selector selects: each stream's full label set, so an unread
// stream is classified, priced and reported like any other (route_cold
// when nothing marks it incident-critical, a confirm question when its
// labels do). When a query's selectors couldn't be read (AllStreams) or a
// selector can't be evaluated, nothing can be shown unread, so none is
// added: an unread stream is never a guess.
func unreadStreams(vols []collect.StreamVolume, signals []graph.Signal) []graph.Signal {
	var selectors [][]parse.Matcher
	ids := map[string]bool{}
	for _, s := range signals {
		ids[s.ID] = true
		if s.Kind != graph.KindStream {
			continue
		}
		if s.Name == AllStreams {
			return nil
		}
		refs, err := parse.LogQL(s.Name)
		if err != nil || len(refs) != 1 {
			return nil
		}
		selectors = append(selectors, refs[0].Matchers)
	}
	var out []graph.Signal
	for _, v := range vols {
		read := false
		for _, ms := range selectors {
			match, err := parse.MatchesLabels(ms, v.Labels)
			if err != nil {
				return nil
			}
			if match {
				read = true
				break
			}
		}
		if read || len(v.Labels) == 0 {
			continue
		}
		ms := make([]parse.Matcher, 0, len(v.Labels))
		for k, val := range v.Labels {
			ms = append(ms, parse.Matcher{Label: k, Op: parse.MatchEqual, Value: val})
		}
		sel := parse.CanonicalSelector(ms)
		id := "loki:stream:" + sel
		if ids[id] {
			continue
		}
		ids[id] = true
		out = append(out, graph.Signal{ID: id, Kind: graph.KindStream, Name: sel, Source: "loki"})
	}
	return out
}
