// Package cost turns observed telemetry volume into money using the customer's
// own price card. Vendor presets are a convenience and never billing-accurate.
package cost

import (
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	daysPerMonth    = 30
	secondsPerMonth = daysPerMonth * 24 * 60 * 60
	bytesPerGB      = 1e9
)

// PriceCard holds the customer's list or negotiated prices (format v1,
// docs/pricecard.md). Any price left at zero is free. Prices are marginal:
// the rate of the next unit, so a saving comes off the top tier.
type PriceCard struct {
	Version int `json:"version" yaml:"version"`
	// Kind is "" or "vendor" (prices per series, sample, GB) or
	// "self_hosted" (series become disk; SelfHosted holds the rates).
	Kind     string        `json:"kind,omitempty" yaml:"kind,omitempty"`
	Name     string        `json:"name" yaml:"name"`
	Currency string        `json:"currency" yaml:"currency"`
	Metrics  MetricPricing `json:"metrics" yaml:"metrics"`
	Logs     LogPricing    `json:"logs" yaml:"logs"`
	Traces   TracePricing  `json:"traces" yaml:"traces"`
	// SelfHosted prices metrics on infrastructure you run (kind
	// self_hosted).
	SelfHosted *SelfHostedPricing `json:"self_hosted,omitempty" yaml:"self_hosted,omitempty"`
	// Preset marks a vendor preset: approximate, never billing-accurate.
	// A preset says where its numbers came from and when.
	Preset bool   `json:"preset,omitempty" yaml:"preset,omitempty"`
	Source string `json:"source,omitempty" yaml:"source,omitempty"`
	AsOf   string `json:"as_of,omitempty" yaml:"as_of,omitempty"`
}

// MetricPricing charges per active series, per sample, or both.
type MetricPricing struct {
	PerActiveSeriesMonth float64 `json:"per_active_series_month" yaml:"per_active_series_month"`
	PerMillionSamples    float64 `json:"per_million_samples" yaml:"per_million_samples"`
	// IncludedActiveSeries is a free or committed allotment, applied to the
	// total (MetricsBillable), never per signal.
	IncludedActiveSeries int64 `json:"included_active_series" yaml:"included_active_series"`
}

// LogPricing charges for ingest, retained storage and indexed events.
type LogPricing struct {
	PerGBIngested           float64 `json:"per_gb_ingested" yaml:"per_gb_ingested"`
	PerGBMonthRetained      float64 `json:"per_gb_month_retained" yaml:"per_gb_month_retained"`
	RetentionDays           int     `json:"retention_days" yaml:"retention_days"`
	PerMillionEventsIndexed float64 `json:"per_million_events_indexed" yaml:"per_million_events_indexed"`
	// IncludedGBMonth is ingested GB per month that cost nothing, applied to
	// the total (LogsBillable).
	IncludedGBMonth float64 `json:"included_gb_month" yaml:"included_gb_month"`
}

// Price card kinds.
const (
	KindVendor     = "vendor"
	KindSelfHosted = "self_hosted"
)

// SelfHostedPricing turns series into disk (docs/pricecard.md):
// series × samples kept over the retention × bytes per sample × overhead
// × replicas, plus each series' share of the WAL and head chunks.
type SelfHostedPricing struct {
	StorageGBMonth float64 `json:"storage_gb_month" yaml:"storage_gb_month"`
	Replicas       int     `json:"replicas" yaml:"replicas"`
	// Overhead is the index and compaction overhead; 0 means 1.2.
	Overhead float64 `json:"overhead" yaml:"overhead"`
	// BytesPerSample overrides the measured value; 0 means measure it
	// (Prometheus), else 1.5, marked assumed.
	BytesPerSample float64 `json:"bytes_per_sample" yaml:"bytes_per_sample"`
	// RetentionDays overrides the measured retention; 0 means measure it.
	// Required where tvt can't measure it (Mimir).
	RetentionDays float64 `json:"retention_days" yaml:"retention_days"`
	// FixedMonthly is the floor that doesn't shrink with series (compute,
	// replicas); never counted as a saving.
	FixedMonthly float64 `json:"fixed_monthly" yaml:"fixed_monthly"`
	// NodeSizes are the storage nodes in use. A cut saves compute only when
	// it crosses a node step (NodeStep). One size for now.
	NodeSizes []NodeSize `json:"node_sizes" yaml:"node_sizes"`
}

// NodeSize is a storage node: its usable disk and its monthly cost.
type NodeSize struct {
	Name    string  `json:"name" yaml:"name"`
	DiskGB  float64 `json:"disk_gb" yaml:"disk_gb"`
	Monthly float64 `json:"monthly" yaml:"monthly"`
}

// DefaultOverhead and DefaultBytesPerSample are the self-hosted model's
// fallbacks.
const (
	DefaultOverhead       = 1.2
	DefaultBytesPerSample = 1.5
)

// TracePricing charges per span or per GB ingested.
type TracePricing struct {
	PerMillionSpans float64 `json:"per_million_spans" yaml:"per_million_spans"`
	PerGBIngested   float64 `json:"per_gb_ingested" yaml:"per_gb_ingested"`
}

// Validate rejects a missing or unknown version, negative or non-finite
// prices (YAML .nan, .inf), negative allotments, a preset without its
// source, and an empty card.
func (p PriceCard) Validate() error {
	if p.Version != 1 {
		if p.Version == 0 {
			return errors.New("price card: version is required (version: 1)")
		}
		return fmt.Errorf("price card: version %d is not supported (this tvt reads version 1)", p.Version)
	}
	if p.Currency == "" {
		return errors.New("price card: currency is required")
	}
	switch p.Kind {
	case "", KindVendor:
		if p.SelfHosted != nil {
			return errors.New("price card: self_hosted is only read with kind: self_hosted")
		}
	case KindSelfHosted:
		if err := p.SelfHosted.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("price card: kind %q is not vendor or self_hosted", p.Kind)
	}
	prices := map[string]float64{
		"metrics.per_active_series_month": p.Metrics.PerActiveSeriesMonth,
		"metrics.per_million_samples":     p.Metrics.PerMillionSamples,
		"logs.per_gb_ingested":            p.Logs.PerGBIngested,
		"logs.per_gb_month_retained":      p.Logs.PerGBMonthRetained,
		"traces.per_million_spans":        p.Traces.PerMillionSpans,
		"traces.per_gb_ingested":          p.Traces.PerGBIngested,
		"logs.per_million_events_indexed": p.Logs.PerMillionEventsIndexed,
	}
	set := false
	for name, v := range prices {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("price card: %s is not a finite number", name)
		}
		if v < 0 {
			return fmt.Errorf("price card: %s is negative", name)
		}
		set = set || v > 0
	}
	if !set && p.Kind != KindSelfHosted {
		return errors.New("price card: no prices set")
	}
	if p.Logs.RetentionDays < 0 {
		return errors.New("price card: logs.retention_days is negative")
	}
	if p.Metrics.IncludedActiveSeries < 0 {
		return errors.New("price card: metrics.included_active_series is negative")
	}
	if v := p.Logs.IncludedGBMonth; v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return errors.New("price card: logs.included_gb_month must be a finite, non-negative number")
	}
	if p.Preset && (p.Source == "" || p.AsOf == "") {
		return errors.New("price card: a preset needs source and as_of (where and when its numbers were copied)")
	}
	return nil
}

// MetricMonthly is the monthly cost of activeSeries scraped every interval.
func (p PriceCard) MetricMonthly(activeSeries int64, interval time.Duration) float64 {
	c := float64(activeSeries) * p.Metrics.PerActiveSeriesMonth
	if interval > 0 && p.Metrics.PerMillionSamples > 0 {
		samples := float64(activeSeries) * secondsPerMonth / interval.Seconds()
		c += samples / 1e6 * p.Metrics.PerMillionSamples
	}
	return c
}

// LogMonthly is the monthly cost of a stream ingesting bytesPerDay: ingest
// plus steady-state retained storage.
func (p PriceCard) LogMonthly(bytesPerDay int64) float64 {
	gbPerDay := float64(bytesPerDay) / bytesPerGB
	ingest := gbPerDay * daysPerMonth * p.Logs.PerGBIngested
	retained := gbPerDay * float64(p.Logs.RetentionDays) * p.Logs.PerGBMonthRetained
	return ingest + retained
}

// TraceMonthly is the monthly cost of spansPerDay totalling bytesPerDay.
func (p PriceCard) TraceMonthly(spansPerDay, bytesPerDay int64) float64 {
	spans := float64(spansPerDay) * daysPerMonth / 1e6 * p.Traces.PerMillionSpans
	gb := float64(bytesPerDay) * daysPerMonth / bytesPerGB * p.Traces.PerGBIngested
	return spans + gb
}

// LabelMonthly is the share of a metric's monthly cost one of its labels
// multiplies: the metric's cost per series, times the series carrying the
// label, times the fraction that dropping the label would collapse
// (1 - 1/distinct values; independence assumed). A label with one value
// multiplies nothing.
func LabelMonthly(metricMonthly float64, metricSeries, labelSeries, distinctValues int64) float64 {
	if metricSeries <= 0 || labelSeries <= 0 || distinctValues <= 1 {
		return 0
	}
	perSeries := metricMonthly / float64(metricSeries)
	return perSeries * float64(labelSeries) * (1 - 1/float64(distinctValues))
}

// MetricsBillable applies the included series allotment to a total: gross
// is the sum of per-signal metric costs for totalSeries active series. The
// allotment is priced at the marginal per-series rate and never takes the
// total below zero.
func (p PriceCard) MetricsBillable(gross float64, totalSeries int64) float64 {
	included := min(totalSeries, p.Metrics.IncludedActiveSeries)
	return max(0, gross-float64(included)*p.Metrics.PerActiveSeriesMonth)
}

// LogsBillable applies the included ingest allotment to a total: gross is
// the sum of per-stream log costs for bytesPerDay ingested in all.
func (p PriceCard) LogsBillable(gross float64, bytesPerDay int64) float64 {
	gbMonth := float64(bytesPerDay) / bytesPerGB * daysPerMonth
	included := min(gbMonth, p.Logs.IncludedGBMonth)
	return max(0, gross-included*p.Logs.PerGBIngested)
}

func (h *SelfHostedPricing) validate() error {
	if h == nil {
		return errors.New("price card: kind self_hosted needs a self_hosted section")
	}
	finite := func(name string, v float64, positive bool) error {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || (positive && v == 0) {
			want := "a finite, non-negative number"
			if positive {
				want = "a finite, positive number"
			}
			return fmt.Errorf("price card: self_hosted.%s must be %s", name, want)
		}
		return nil
	}
	for _, f := range []struct {
		name     string
		v        float64
		positive bool
	}{
		{"storage_gb_month", h.StorageGBMonth, true},
		{"overhead", h.Overhead, false},
		{"bytes_per_sample", h.BytesPerSample, false},
		{"retention_days", h.RetentionDays, false},
		{"fixed_monthly", h.FixedMonthly, false},
	} {
		if err := finite(f.name, f.v, f.positive); err != nil {
			return err
		}
	}
	if h.Overhead != 0 && h.Overhead < 1 {
		return errors.New("price card: self_hosted.overhead is a multiplier, at least 1")
	}
	if h.Replicas < 1 {
		return errors.New("price card: self_hosted.replicas must be at least 1")
	}
	if len(h.NodeSizes) > 1 {
		return errors.New("price card: self_hosted.node_sizes: one node size is supported for now")
	}
	for _, n := range h.NodeSizes {
		if err := finite("node_sizes.disk_gb", n.DiskGB, true); err != nil {
			return err
		}
		if err := finite("node_sizes.monthly", n.Monthly, false); err != nil {
			return err
		}
	}
	return nil
}

// Storage is what the self-hosted model needs to know about the TSDB.
type Storage struct {
	// BytesPerSample on disk after compression; BytesPerSampleFrom says
	// where it came from (the card, a measurement, or an assumption).
	BytesPerSample     float64
	BytesPerSampleFrom string
	// Retention is how long samples are kept; RetentionFrom says why.
	Retention     time.Duration
	RetentionFrom string
	// HeadBytesPerSeries is each active series' share of the WAL and head
	// chunks; 0 when unknown.
	HeadBytesPerSeries float64
}

// SelfHostedMetric is the disk a metric's series occupy, in bytes, and
// its monthly storage cost: series × (retention ÷ interval) samples ×
// bytes per sample × overhead, plus the head and WAL share, × replicas.
// The fixed floor and node steps aren't per signal (NodeStep).
func (p PriceCard) SelfHostedMetric(series int64, interval time.Duration, st Storage) (diskBytes, monthly float64) {
	h := p.SelfHosted
	if h == nil || series <= 0 || interval <= 0 {
		return 0, 0
	}
	overhead := h.Overhead
	if overhead == 0 {
		overhead = DefaultOverhead
	}
	samples := st.Retention.Seconds() / interval.Seconds()
	perSeries := samples*st.BytesPerSample*overhead + st.HeadBytesPerSeries
	diskBytes = float64(series) * perSeries * float64(h.Replicas)
	return diskBytes, diskBytes / bytesPerGB * h.StorageGBMonth
}

// NodeStep says what freeing freedBytes of totalBytes does to the node
// count: nodes before and after (each holding node_sizes[0].disk_gb), and
// the monthly compute saved, which is zero unless the cut crosses a step.
// Without node sizes it returns zeros.
func (p PriceCard) NodeStep(totalBytes, freedBytes float64) (before, after int, monthly float64) {
	if p.SelfHosted == nil || len(p.SelfHosted.NodeSizes) == 0 {
		return 0, 0, 0
	}
	n := p.SelfHosted.NodeSizes[0]
	per := n.DiskGB * bytesPerGB
	nodes := func(b float64) int { return max(1, int(math.Ceil(b/per))) }
	before, after = nodes(totalBytes), nodes(max(0, totalBytes-freedBytes))
	return before, after, float64(before-after) * n.Monthly
}
