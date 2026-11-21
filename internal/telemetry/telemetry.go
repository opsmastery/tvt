// Package telemetry is tvt's own observability (ADR 0008, Sprint 2 item 9,
// S12 item 6): OpenTelemetry traces and metrics, configured only from the
// standard OTEL_* environment variables and off unless they ask for an
// exporter.
//
// The conventions, which keep tvt's telemetry lean enough that a tvt scan
// of it finds nothing to cut:
//
//   - Span names are fixed: tvt.scan, tvt.collect.<backend>. Metric names
//     are fixed too, and no metric carries a per-scan or per-signal
//     attribute: only bounded ones (backend, outcome, kind).
//   - Signal names, URL paths and query text stay out by default. Routes
//     are recorded with every non-API path segment replaced by {}.
//     TVT_TELEMETRY_DETAIL=names adds the raw path and query text to spans,
//     never to metrics.
//   - Every string attribute is capped at MaxAttr characters, cut with
//     TruncatedMarker.
//   - Headers are never recorded, so no token can reach an exporter.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/Opsmastery-Summit/tvt/internal/version"
)

// Scope is the instrumentation scope of every tracer and meter.
const Scope = "github.com/Opsmastery-Summit/tvt"

// MaxAttr caps a string attribute's length in characters.
const MaxAttr = 512

// TruncatedMarker ends a cut attribute, so a cut value can't pass for a
// whole one.
const TruncatedMarker = "…[truncated]"

// Detail levels (TVT_TELEMETRY_DETAIL).
const (
	DetailBasic = "basic" // counts, durations, outcomes, normalized routes
	DetailNames = "names" // also raw paths and query text, on spans only
)

var (
	mu     sync.RWMutex
	detail = DetailBasic
)

// Names reports whether signal names and query text may go on spans.
func Names() bool {
	mu.RLock()
	defer mu.RUnlock()
	return detail == DetailNames
}

func setDetail(d string) {
	mu.Lock()
	detail = d
	mu.Unlock()
}

// Tracer is tvt's tracer, from the global provider (a no-op until Setup
// installs one).
func Tracer() trace.Tracer { return otel.Tracer(Scope) }

// Meter is tvt's meter, from the global provider.
func Meter() metric.Meter { return otel.Meter(Scope) }

// String is a string attribute capped at MaxAttr characters.
func String(key, value string) attribute.KeyValue {
	return attribute.String(key, Truncate(value))
}

// Truncate caps s at MaxAttr characters (runes), the marker included.
func Truncate(s string) string {
	if len(s) <= MaxAttr { // bytes ≥ runes
		return s
	}
	r := []rune(s)
	if len(r) <= MaxAttr {
		return s
	}
	m := []rune(TruncatedMarker)
	return string(r[:MaxAttr-len(m)]) + TruncatedMarker
}

// Setup reads the OTEL_* environment and, if it asks for an exporter,
// installs global tracer and meter providers. With nothing set (or
// OTEL_SDK_DISABLED=true) it installs nothing and the globals stay no-op.
// The returned shutdown flushes and stops what was installed.
//
// Traces: OTEL_TRACES_EXPORTER otlp, console (stderr) or none; unset means
// otlp when OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
// is set. Metrics: OTEL_METRICS_EXPORTER otlp or none, likewise. OTLP is
// over HTTP, and the exporters read the rest of the OTEL_EXPORTER_OTLP_*
// settings themselves.
func Setup(ctx context.Context, getenv func(string) string) (shutdown func(context.Context) error, err error) {
	shutdown = func(context.Context) error { return nil }
	switch d := strings.ToLower(strings.TrimSpace(getenv("TVT_TELEMETRY_DETAIL"))); d {
	case "", DetailBasic:
		setDetail(DetailBasic)
	case DetailNames:
		setDetail(DetailNames)
	default:
		return shutdown, fmt.Errorf("TVT_TELEMETRY_DETAIL=%q: want basic or names", d)
	}
	if strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true") {
		return shutdown, nil
	}
	traces, err := exporterKind(getenv, "OTEL_TRACES_EXPORTER", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "otlp", "console", "none")
	if err != nil {
		return shutdown, err
	}
	metrics, err := exporterKind(getenv, "OTEL_METRICS_EXPORTER", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "otlp", "none")
	if err != nil {
		return shutdown, err
	}
	if traces == "none" && metrics == "none" {
		return shutdown, nil
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "tvt"), attribute.String("service.version", version.Version)),
		resource.WithFromEnv())
	if err != nil {
		return shutdown, fmt.Errorf("telemetry resource: %w", err)
	}
	var stops []func(context.Context) error
	if traces != "none" {
		var exp sdktrace.SpanExporter
		if traces == "otlp" {
			exp, err = otlptracehttp.New(ctx)
		} else {
			exp, err = stdouttrace.New(stdouttrace.WithWriter(os.Stderr), stdouttrace.WithPrettyPrint())
		}
		if err != nil {
			return shutdown, fmt.Errorf("trace exporter: %w", err)
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.TraceContext{})
		stops = append(stops, tp.Shutdown)
	}
	if metrics == "otlp" {
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return shutdown, fmt.Errorf("metric exporter: %w", err)
		}
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)), sdkmetric.WithResource(res))
		otel.SetMeterProvider(mp)
		stops = append(stops, mp.Shutdown)
	}
	return func(ctx context.Context) error {
		var errs []error
		for _, stop := range stops {
			errs = append(errs, stop(ctx))
		}
		return errors.Join(errs...)
	}, nil
}

// exporterKind resolves one signal's exporter from its OTEL_*_EXPORTER
// variable, defaulting to otlp when an OTLP endpoint is configured.
func exporterKind(getenv func(string) string, key, endpointKey string, allowed ...string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(getenv(key)))
	if v == "" {
		if getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || getenv(endpointKey) != "" {
			return "otlp", nil
		}
		return "none", nil
	}
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s=%q: tvt supports %s", key, v, strings.Join(allowed, ", "))
}

// RecordError adds an exception event to span. An error's text often holds
// a URL, a signal name or a query, so at the basic detail level only its Go
// type is recorded; at the names level its message too, capped.
func RecordError(span trace.Span, err error) {
	if err == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("exception.type", fmt.Sprintf("%T", err))}
	if Names() {
		attrs = append(attrs, String("exception.message", err.Error()))
	}
	span.AddEvent("exception", trace.WithAttributes(attrs...))
}
