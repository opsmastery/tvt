package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTruncate(t *testing.T) {
	if got := Truncate("short"); got != "short" {
		t.Errorf("Truncate(short) = %q", got)
	}
	exact := strings.Repeat("a", MaxAttr)
	if got := Truncate(exact); got != exact {
		t.Error("a value of exactly MaxAttr characters was cut")
	}
	for _, long := range []string{strings.Repeat("a", 2000), strings.Repeat("é", 600)} {
		got := Truncate(long)
		if n := utf8.RuneCountInString(got); n != MaxAttr {
			t.Errorf("cut to %d characters, want %d", n, MaxAttr)
		}
		if !strings.HasSuffix(got, TruncatedMarker) || !utf8.ValidString(got) {
			t.Errorf("cut value %q... lacks the marker or isn't valid UTF-8", got[:20])
		}
	}
	// 300 two-byte runes are 600 bytes but under the cap.
	if s := strings.Repeat("é", 300); Truncate(s) != s {
		t.Error("cut by bytes, not characters")
	}
}

func TestRoute(t *testing.T) {
	for path, want := range map[string]string{
		"/api/v1/label/job/values":                                  "/api/v1/label/{}/values",
		"/api/v1/status/tsdb":                                       "/api/v1/status/tsdb",
		"/prometheus/api/v1/cardinality/label_values":               "/prometheus/api/v1/cardinality/label_values",
		"/api/dashboards/uid/shop-overview":                         "/api/dashboards/uid/{}",
		"/api/datasources/proxy/uid/abc123/loki/api/v1/query_range": "/api/datasources/proxy/uid/{}/loki/api/v1/query_range",
		"/loki/api/v1/rules/shop_alerts":                            "/loki/api/v1/rules/{}",
		"/":                                                         "/{}",
	} {
		if got := Route(path); got != want {
			t.Errorf("Route(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSetup(t *testing.T) {
	ctx := context.Background()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	before := otel.GetTracerProvider()
	if _, err := Setup(ctx, env(nil)); err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != before || Names() {
		t.Error("with nothing set, Setup changed the providers or the detail level")
	}
	if _, err := Setup(ctx, env(map[string]string{"TVT_TELEMETRY_DETAIL": "everything"})); err == nil {
		t.Error("an unknown detail level was accepted")
	}
	if _, err := Setup(ctx, env(map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"})); err == nil {
		t.Error("an unsupported exporter was accepted")
	}
	if _, err := Setup(ctx, env(map[string]string{"TVT_TELEMETRY_DETAIL": "names", "OTEL_SDK_DISABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318"})); err != nil || !Names() {
		t.Errorf("names detail: err %v, Names() %v", err, Names())
	}
	if otel.GetTracerProvider() != before {
		t.Error("OTEL_SDK_DISABLED=true still installed a provider")
	}
	_, _ = Setup(ctx, env(nil))
	for k, want := range map[string]string{"": "none", "otlp": "otlp", "console": "console", "none": "none"} {
		got, err := exporterKind(env(map[string]string{"OTEL_TRACES_EXPORTER": k}), "OTEL_TRACES_EXPORTER", "X", "otlp", "console", "none")
		if err != nil || got != want {
			t.Errorf("exporterKind(%q) = %q, %v; want %q", k, got, err, want)
		}
	}
	if got, _ := exporterKind(env(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://x:4318"}), "OTEL_TRACES_EXPORTER", "X", "otlp"); got != "otlp" {
		t.Errorf("an OTLP endpoint alone gave %q, want otlp", got)
	}
}

func TestRecordErrorBasic(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	err := errors.New(`Get "http://prom:9090/api/v1/label/shop_secret_total/values": refused`)
	for _, detail := range []string{"", DetailNames} {
		if _, e := Setup(context.Background(), func(k string) string {
			return map[string]string{"TVT_TELEMETRY_DETAIL": detail}[k]
		}); e != nil {
			t.Fatal(e)
		}
		_, span := tp.Tracer("t").Start(context.Background(), "s")
		RecordError(span, err)
		span.End()
	}
	_, _ = Setup(context.Background(), func(string) string { return "" })
	got := spans.GetSpans()
	basic, names := got[0].Events[0].Attributes, got[1].Events[0].Attributes
	for _, kv := range basic {
		if strings.Contains(kv.Value.Emit(), "shop_secret_total") {
			t.Errorf("basic detail recorded %s = %q", kv.Key, kv.Value.Emit())
		}
	}
	if len(names) != 2 || !strings.Contains(names[1].Value.Emit(), "shop_secret_total") {
		t.Errorf("names detail: %v", names)
	}
}
