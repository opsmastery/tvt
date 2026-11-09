// Package cost turns observed telemetry volume into money using the customer's
// own price card. Vendor presets are a convenience and never billing-accurate.
package cost

import (
	"errors"
	"fmt"
	"time"
)

const (
	daysPerMonth    = 30
	secondsPerMonth = daysPerMonth * 24 * 60 * 60
	bytesPerGB      = 1e9
)

// PriceCard holds the customer's list or negotiated prices. Any price left at
// zero is simply not charged. See docs/pricecard.md for the file format.
type PriceCard struct {
	Name     string        `json:"name" yaml:"name"`
	Currency string        `json:"currency" yaml:"currency"`
	Metrics  MetricPricing `json:"metrics" yaml:"metrics"`
	Logs     LogPricing    `json:"logs" yaml:"logs"`
	Traces   TracePricing  `json:"traces" yaml:"traces"`
	Preset   bool          `json:"preset,omitempty" yaml:"preset,omitempty"` // true for vendor presets: approximate
}

// MetricPricing charges per active series, per sample, or both.
type MetricPricing struct {
	PerActiveSeriesMonth float64 `json:"per_active_series_month" yaml:"per_active_series_month"`
	PerMillionSamples    float64 `json:"per_million_samples" yaml:"per_million_samples"`
}

// LogPricing charges for ingest and for retained storage.
type LogPricing struct {
	PerGBIngested      float64 `json:"per_gb_ingested" yaml:"per_gb_ingested"`
	PerGBMonthRetained float64 `json:"per_gb_month_retained" yaml:"per_gb_month_retained"`
	RetentionDays      int     `json:"retention_days" yaml:"retention_days"`
}

// TracePricing charges per span or per GB ingested.
type TracePricing struct {
	PerMillionSpans float64 `json:"per_million_spans" yaml:"per_million_spans"`
	PerGBIngested   float64 `json:"per_gb_ingested" yaml:"per_gb_ingested"`
}

// Validate rejects negative prices and an empty card.
func (p PriceCard) Validate() error {
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
	}
	set := false
	for name, v := range prices {
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
