package otelcol

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A Collector 0.165's own metrics (trimmed): sent counters per exporter,
// one of them reported for two pipelines.
const metrics = `# HELP otelcol_exporter_sent_log_records_total Number of log record successfully sent to destination.
# TYPE otelcol_exporter_sent_log_records_total counter
otelcol_exporter_sent_log_records_total{exporter="otlp_http/loki",service_instance_id="x",service_name="otelcol-contrib",service_version="0.165.0"} 1200
otelcol_exporter_sent_log_records_total{exporter="file/cold",service_instance_id="x",service_name="otelcol-contrib",service_version="0.165.0"} 30
# HELP otelcol_exporter_sent_metric_points_total Number of metric points successfully sent to destination.
# TYPE otelcol_exporter_sent_metric_points_total counter
otelcol_exporter_sent_metric_points_total{exporter="prometheus_remote_write",pipeline="metrics"} 500
otelcol_exporter_sent_metric_points_total{exporter="prometheus_remote_write",pipeline="metrics/shop-errors"} 20
# HELP otelcol_process_uptime_seconds_total Uptime of the process
# TYPE otelcol_process_uptime_seconds_total counter
otelcol_process_uptime_seconds_total 99
`

func TestTelemetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/metrics" {
			http.Error(w, "no", 400)
			return
		}
		fmt.Fprint(w, metrics)
	}))
	defer srv.Close()
	got, err := Telemetry(context.Background(), srv.Client(), srv.URL+"/metrics")
	if err != nil {
		t.Fatal(err)
	}
	want := "[{file/cold logs 30} {otlp_http/loki logs 1200} {prometheus_remote_write metrics 520}]"
	if fmt.Sprint(got) != want {
		t.Errorf("got %v, want %s", got, want)
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "up 1\n") }))
	defer empty.Close()
	if _, err := Telemetry(context.Background(), empty.Client(), empty.URL); err == nil {
		t.Error("metrics with no sent counters: no error")
	}
}
