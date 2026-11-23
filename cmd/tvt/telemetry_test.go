package main

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/opsmastery/tvt/internal/telemetry"
)

// telemetryScan runs the lab scan with in-memory trace and metric
// providers installed, at the given detail level.
func telemetryScan(t *testing.T, detail string) (tracetest.SpanStubs, metricdata.ResourceMetrics) {
	t.Helper()
	setNow(t)
	if _, err := telemetry.Setup(context.Background(), func(k string) string {
		return map[string]string{"TVT_TELEMETRY_DETAIL": detail, "OTEL_SDK_DISABLED": "true"}[k]
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = telemetry.Setup(context.Background(), func(string) string { return "" }) })
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	oldTP, oldMP := otel.GetTracerProvider(), otel.GetMeterProvider()
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetTracerProvider(oldTP); otel.SetMeterProvider(oldMP) })

	l := newLab(t, nil)
	o := labOptions(l, filepath.Join(t.TempDir(), "tvt.db"))
	var out bytes.Buffer
	if err := tracedScan(context.Background(), o, func(ctx context.Context) error { return runScan(ctx, o, env, &out) }); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return spans.GetSpans(), rm
}

// TestScanTelemetryShape is the golden trace shape (ADR 0008): one
// tvt.scan root, every other span a tvt.collect.<backend> child of it,
// evidence gaps as events, and run metrics with bounded attributes only.
func TestScanTelemetryShape(t *testing.T) {
	spans, rm := telemetryScan(t, telemetry.DetailBasic)
	var root *tracetest.SpanStub
	backends := map[string]int{}
	for i, s := range spans {
		switch {
		case s.Name == "tvt.scan":
			root = &spans[i]
		case strings.HasPrefix(s.Name, "tvt.collect."):
			backends[strings.TrimPrefix(s.Name, "tvt.collect.")]++
		default:
			t.Errorf("unexpected span %q", s.Name)
		}
	}
	if root == nil {
		t.Fatal("no tvt.scan span")
	}
	for _, b := range []string{"grafana", "prometheus", "loki"} {
		if backends[b] == 0 {
			t.Errorf("no tvt.collect.%s spans (got %v)", b, backends)
		}
	}
	if backends["http"] != 0 {
		t.Errorf("%d requests to an unregistered backend", backends["http"])
	}
	for _, s := range spans {
		if s.Name != "tvt.scan" && s.Parent.SpanID() != root.SpanContext.SpanID() {
			t.Errorf("%s isn't a child of tvt.scan", s.Name)
		}
	}
	gaps := 0
	for _, e := range root.Events {
		if e.Name == "evidence_gap" {
			gaps++
		}
	}
	if gaps == 0 {
		t.Error("no evidence_gap events: the lab scan has unconfigured sources")
	}

	// Metrics: no per-scan or per-signal attribute anywhere.
	allowed := map[string][]string{
		"tvt.scan.runs":        {"outcome"},
		"tvt.scan.duration":    {"outcome"},
		"tvt.scan.signals":     {"tvt.class"},
		"tvt.collect.requests": {"outcome", "tvt.backend"},
		"tvt.collect.duration": {"outcome", "tvt.backend"},
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			keys, ok := allowed[m.Name]
			if !ok {
				t.Errorf("unexpected metric %q", m.Name)
				continue
			}
			seen[m.Name] = true
			for _, set := range attrSets(m.Data) {
				var got []string
				for _, kv := range set.ToSlice() {
					got = append(got, string(kv.Key))
				}
				slices.Sort(got)
				if !slices.Equal(got, keys) {
					t.Errorf("%s has attributes %v, want %v", m.Name, got, keys)
				}
			}
		}
	}
	for name := range allowed {
		if !seen[name] {
			t.Errorf("metric %s not recorded", name)
		}
	}
}

// TestScanTelemetryNoSecrets: no token or auth header reaches a span, and
// at the basic detail level no signal name, path or query text does.
// At the names level, paths and queries appear, capped at 512 characters.
func TestScanTelemetryNoSecrets(t *testing.T) {
	check := func(spans tracetest.SpanStubs, forbidden []string) (paths, queries, gaps int) {
		for _, s := range spans {
			kvs := slices.Clone(s.Attributes)
			for _, e := range s.Events {
				kvs = append(kvs, e.Attributes...)
			}
			for _, kv := range kvs {
				v := kv.Value.Emit()
				if len([]rune(v)) > telemetry.MaxAttr {
					t.Errorf("%s %s is %d characters", s.Name, kv.Key, len([]rune(v)))
				}
				for _, f := range forbidden {
					if strings.Contains(v, f) {
						t.Errorf("%s %s = %q contains %q", s.Name, kv.Key, v, f)
					}
				}
				switch kv.Key {
				case "url.path":
					paths++
				case "db.query.text":
					queries++
				case "tvt.gap":
					gaps++
				}
			}
		}
		return
	}
	secrets := []string{"test-grafana-token", "Bearer", "Basic ", "Authorization"}
	spans, _ := telemetryScan(t, telemetry.DetailBasic)
	if p, q, g := check(spans, append(secrets, "shop_", "tvt_query_log_probe")); p+q+g != 0 {
		t.Errorf("basic detail recorded %d paths, %d queries and %d gap texts", p, q, g)
	}
	spans, _ = telemetryScan(t, telemetry.DetailNames)
	if p, q, g := check(spans, secrets); p == 0 || q == 0 || g == 0 {
		t.Errorf("names detail recorded %d paths, %d queries and %d gap texts, want all three", p, q, g)
	}
}

func attrSets(d metricdata.Aggregation) []attribute.Set {
	var sets []attribute.Set
	switch d := d.(type) {
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	}
	return sets
}
