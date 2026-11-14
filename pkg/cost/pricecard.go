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
	Version  int           `json:"version" yaml:"version"`
	Name     string        `json:"name" yaml:"name"`
	Currency string        `json:"currency" yaml:"currency"`
	Metrics  MetricPricing `json:"metrics" yaml:"metrics"`
	Logs     LogPricing    `json:"logs" yaml:"logs"`
	Traces   TracePricing  `json:"traces" yaml:"traces"`
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
	if !set {
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
