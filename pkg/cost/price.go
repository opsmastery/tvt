package cost

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/graph"
)

// Price sets the money on cost rows in place from what they measured
// (series, interval, bytes): metric rows (<src>:metric:<name>), label rows
// (<src>:label:<metric>:<label>, their share of the metric) and Loki
// stream rows. Rows of other kinds are left alone. A self_hosted card
// prices metrics by disk, resolving its inputs from measured (the
// Prometheus server's /metrics; nil when there is none), and returns them.
//
// It only reads what a scan stores, so a stored scan can be priced at any
// card (Reprice) and gets the numbers a scan with that card would have.
func Price(card PriceCard, costs []graph.Cost, measured *collect.Storage) (*Storage, error) {
	var st *Storage
	if card.Kind == KindSelfHosted {
		s, err := ResolveStorage(card, measured, func(bps float64) float64 {
			var perDay float64
			for i := range costs {
				if c := &costs[i]; isMetric(c.SignalID) && c.Interval > 0 {
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
	type priced struct {
		series  int64
		monthly float64
		disk    int64
	}
	metrics := map[string]priced{}
	for i := range costs {
		c := &costs[i]
		if !isMetric(c.SignalID) {
			continue
		}
		c.DiskBytes = 0
		if st != nil {
			disk, monthly := card.SelfHostedMetric(c.ActiveSeries, c.Interval, *st)
			c.DiskBytes, c.MonthlyCost = int64(disk), monthly
		} else {
			c.MonthlyCost = card.MetricMonthly(c.ActiveSeries, c.Interval)
		}
		metrics[c.SignalID] = priced{c.ActiveSeries, c.MonthlyCost, c.DiskBytes}
	}
	for i := range costs {
		c := &costs[i]
		if strings.HasPrefix(c.SignalID, streamPrefix) {
			c.MonthlyCost = card.LogMonthly(c.BytesPerDay)
			continue
		}
		metric, ok := labelMetric(c.SignalID)
		if !ok {
			continue
		}
		m := metrics[metric]
		c.MonthlyCost = LabelMonthly(m.monthly, m.series, c.ActiveSeries, c.DistinctValues)
		c.DiskBytes = int64(LabelMonthly(float64(m.disk), m.series, c.ActiveSeries, c.DistinctValues))
	}
	return st, nil
}

// Reprice is Price on a copy of costs: what a stored scan would cost at
// another card, without rescanning.
func Reprice(card PriceCard, costs []graph.Cost, measured *collect.Storage) ([]graph.Cost, *Storage, error) {
	out := append([]graph.Cost(nil), costs...)
	st, err := Price(card, out, measured)
	if err != nil {
		return nil, nil, err
	}
	return out, st, nil
}

const streamPrefix = "loki:stream:"

func isMetric(id string) bool {
	_, rest, ok := strings.Cut(id, ":")
	return ok && strings.HasPrefix(rest, "metric:")
}

// labelMetric is a label row's metric row ID. Label IDs are
// <src>:label:<metric>:<label>; metric names may hold colons (recording
// rules), label names never do.
func labelMetric(id string) (string, bool) {
	src, rest, ok := strings.Cut(id, ":")
	if !ok {
		return "", false
	}
	rest, ok = strings.CutPrefix(rest, "label:")
	if !ok {
		return "", false
	}
	j := strings.LastIndexByte(rest, ':')
	if j < 0 {
		return "", false
	}
	return src + ":metric:" + rest[:j], true
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

// ResolveStorage picks each self-hosted input: the card's override, else
// what the backend reports (m; nil for Mimir), else for bytes per sample
// DefaultBytesPerSample. Retention has no default: a wrong one
// scales every cost, so a backend that doesn't report it needs the card
// to say. A size limit that fills before the time limit shortens the
// retention; bytesPerDay is the blocks written a day at a bytes per
// sample.
func ResolveStorage(card PriceCard, m *collect.Storage, bytesPerDay func(bps float64) float64) (Storage, error) {
	h := card.SelfHosted
	var st Storage
	if h == nil {
		return st, fmt.Errorf("price card %q is self_hosted but has no self_hosted section", card.Name)
	}
	switch {
	case h.BytesPerSample > 0:
		st.BytesPerSample, st.BytesPerSampleFrom = h.BytesPerSample, FromCard
	case m != nil && m.BytesPerSample > 0:
		st.BytesPerSample, st.BytesPerSampleFrom = m.BytesPerSample, FromCompaction
	case m != nil:
		st.BytesPerSample, st.BytesPerSampleFrom = DefaultBytesPerSample, AssumedNoCompact
	default:
		st.BytesPerSample, st.BytesPerSampleFrom = DefaultBytesPerSample, AssumedNoStorage
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
			overhead = DefaultOverhead
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
