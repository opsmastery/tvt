package scan

import (
	"fmt"
	"math"
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
// (cost.PriceCard.SelfHostedMetric) using resolveStorage's inputs, which
// it returns for the scan row.
func priceCosts(in Inputs, costs []graph.Cost) (*cost.Storage, error) {
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
		disk    int64
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
		metrics[name] = priced{series: c.ActiveSeries}
	}
	if in.PriceCard == nil {
		return nil, nil
	}
	var st *cost.Storage
	if in.PriceCard.Kind == cost.KindSelfHosted {
		s, err := resolveStorage(*in.PriceCard, in.Storage, func(bps float64) float64 {
			var perDay float64
			for i := range costs {
				if c := &costs[i]; strings.HasPrefix(c.SignalID, metricPrefix) && c.Interval > 0 {
					perDay += float64(c.ActiveSeries) * 86400 / c.Interval.Seconds() * bps
				}
			}
			return perDay
		})
		if err != nil {
			return nil, err
		}
		st = &s
	}
	for i := range costs {
		c := &costs[i]
		name, ok := strings.CutPrefix(c.SignalID, metricPrefix)
		if !ok {
			continue
		}
		if st != nil {
			disk, monthly := in.PriceCard.SelfHostedMetric(c.ActiveSeries, c.Interval, *st)
			c.DiskBytes, c.MonthlyCost = int64(disk), monthly
		} else {
			c.MonthlyCost = in.PriceCard.MetricMonthly(c.ActiveSeries, c.Interval)
		}
		metrics[name] = priced{c.ActiveSeries, c.MonthlyCost, c.DiskBytes}
	}
	for i := range costs {
		c := &costs[i]
		if strings.HasPrefix(c.SignalID, streamPrefix) {
			c.MonthlyCost = in.PriceCard.LogMonthly(c.BytesPerDay)
			continue
		}
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
		c.DiskBytes = int64(cost.LabelMonthly(float64(m.disk), m.series, c.ActiveSeries, c.DistinctValues))
	}
	return st, nil
}

// Where a self-hosted model's inputs came from, recorded on the scan.
const (
	FromCard          = "price card"
	FromCompaction    = "measured: compacted chunk bytes over samples (/metrics)"
	FromRetentionTime = "measured: --storage.tsdb.retention.time (/metrics)"
	FromRetentionSize = "measured: --storage.tsdb.retention.size fills first (/metrics)"
	AssumedNoCompact  = "assumed: the server hasn't compacted a block yet"
	AssumedNoStorage  = "assumed: the backend doesn't report storage"
)

// resolveStorage picks each self-hosted input: the card's override, else
// what the backend reports (m; nil for Mimir), else for bytes per sample
// cost.DefaultBytesPerSample. Retention has no default: a wrong one
// scales every cost, so a backend that doesn't report it needs the card
// to say. A size limit that fills before the time limit shortens the
// retention; bytesPerDay is the blocks written a day at a bytes per
// sample.
func resolveStorage(card cost.PriceCard, m *collect.Storage, bytesPerDay func(bps float64) float64) (cost.Storage, error) {
	h := card.SelfHosted
	var st cost.Storage
	if h == nil {
		return st, fmt.Errorf("price card %q is self_hosted but has no self_hosted section", card.Name)
	}
	switch {
	case h.BytesPerSample > 0:
		st.BytesPerSample, st.BytesPerSampleFrom = h.BytesPerSample, FromCard
	case m != nil && m.BytesPerSample > 0:
		st.BytesPerSample, st.BytesPerSampleFrom = m.BytesPerSample, FromCompaction
	case m != nil:
		st.BytesPerSample, st.BytesPerSampleFrom = cost.DefaultBytesPerSample, AssumedNoCompact
	default:
		st.BytesPerSample, st.BytesPerSampleFrom = cost.DefaultBytesPerSample, AssumedNoStorage
	}
	if m != nil && m.HeadSeries > 0 {
		st.HeadBytesPerSeries = float64(m.HeadBytes) / float64(m.HeadSeries)
	}
	switch {
	case h.RetentionDays > 0:
		st.Retention, st.RetentionFrom = time.Duration(h.RetentionDays*24*float64(time.Hour)), FromCard
	case m != nil && m.RetentionTime > 0:
		st.Retention, st.RetentionFrom = m.RetentionTime, FromRetentionTime
	case m != nil && m.RetentionBytes > 0:
		// Size-only retention: no time limit, so the cap below decides.
		st.Retention, st.RetentionFrom = time.Duration(math.MaxInt64), FromRetentionSize
	default:
		return st, fmt.Errorf("price card %q is self_hosted: set self_hosted.retention_days (the backend doesn't report its retention)", card.Name)
	}
	if st.RetentionFrom != FromCard && m.RetentionBytes > 0 {
		overhead := h.Overhead
		if overhead == 0 {
			overhead = cost.DefaultOverhead
		}
		if perDay := bytesPerDay(st.BytesPerSample * overhead); perDay > 0 {
			days := float64(m.RetentionBytes-m.HeadBytes) / perDay
			if hours := days * 24; hours > 0 && hours < st.Retention.Hours() {
				st.Retention, st.RetentionFrom = time.Duration(hours*float64(time.Hour)), FromRetentionSize
			}
		}
	}
	if st.Retention == time.Duration(math.MaxInt64) {
		return st, fmt.Errorf("price card %q is self_hosted: the server's size-only retention can't be modeled yet (no samples priced); set self_hosted.retention_days", card.Name)
	}
	return st, nil
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
