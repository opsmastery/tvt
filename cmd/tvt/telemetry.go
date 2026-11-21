package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Opsmastery-Summit/tvt/internal/telemetry"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
)

// tracedScan runs one scan inside the tvt.scan span and records the run's
// metrics (docs/telemetry.md): outcome and duration, nothing per signal.
func tracedScan(ctx context.Context, o scanOptions, run func(context.Context) error) error {
	telemetry.RegisterBackend(o.Grafana, "grafana")
	if o.Mimir != "" {
		telemetry.RegisterBackend(mimirAPI(o.Mimir), "mimir")
	} else {
		telemetry.RegisterBackend(o.Prom, "prometheus")
	}
	telemetry.RegisterBackend(o.Loki, "loki")
	ctx, span := telemetry.Tracer().Start(ctx, "tvt.scan", trace.WithAttributes(
		attribute.Int("tvt.window_days", o.Window),
		attribute.String("tvt.grafana_edition", o.GrafanaEdition),
	))
	defer span.End()
	start := time.Now()
	err := run(ctx)
	outcome := "ok"
	if err != nil {
		outcome = "error"
		if ctx.Err() != nil {
			outcome = "interrupted"
		}
		telemetry.RecordError(span, err)
		span.SetStatus(codes.Error, outcome)
	}
	m := telemetry.Meter()
	set := metric.WithAttributes(attribute.String("outcome", outcome))
	if c, cerr := m.Int64Counter("tvt.scan.runs", metric.WithDescription("tvt scan runs")); cerr == nil {
		c.Add(ctx, 1, set)
	}
	if h, herr := m.Float64Histogram("tvt.scan.duration", metric.WithUnit("s"), metric.WithDescription("tvt scan duration")); herr == nil {
		h.Record(ctx, time.Since(start).Seconds(), set)
	}
	return err
}

// recordScanResult puts the built graph's shape on the scan span: counts,
// and one evidence_gap event per source that can't vouch for the window.
// The signal counts by class also go on tvt.scan.signals, a gauge whose
// only attribute is the class.
func recordScanResult(ctx context.Context, g scan.Output, classes map[string]int) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.Int("tvt.signals", len(g.Signals)),
		attribute.Int("tvt.consumers", len(g.Consumers)),
		attribute.Int("tvt.references", len(g.References)),
		attribute.Float64("tvt.log_coverage", g.LogCoverage),
	)
	for _, s := range g.Sources {
		if s.Configured && s.Coverage >= 1 && s.Gap == "" {
			continue
		}
		attrs := []attribute.KeyValue{
			telemetry.String("tvt.source", s.Source),
			attribute.Bool("tvt.configured", s.Configured),
			attribute.Float64("tvt.coverage", s.Coverage),
		}
		if s.Gap != "" {
			attrs = append(attrs, telemetry.String("tvt.gap", s.Gap))
		}
		span.AddEvent("evidence_gap", trace.WithAttributes(attrs...))
	}
	if gauge, err := telemetry.Meter().Int64Gauge("tvt.scan.signals", metric.WithDescription("Signals in the last scan, by class")); err == nil {
		for class, n := range classes {
			if class == "" {
				class = "unclassified"
			}
			gauge.Record(ctx, int64(n), metric.WithAttributes(attribute.String("tvt.class", class)))
		}
	}
}
