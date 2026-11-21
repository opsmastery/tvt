// Package otelcol reads an OpenTelemetry Collector's own telemetry (S11
// item 4): what each exporter sent, from the Collector's Prometheus
// endpoint (service.telemetry.metrics, :8888/metrics by default). A GET of
// the Collector's own metrics, never a query: every backend stays
// read-only and unqueried.
package otelcol

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/Opsmastery-Summit/tvt/internal/httpx"
)

// maxTelemetryBytes caps the /metrics page read.
const maxTelemetryBytes = 16 << 20

// Sent is one exporter's sent counter for one signal.
type Sent struct {
	Exporter string
	// Signal is logs (log records), traces (spans) or metrics (metric
	// points).
	Signal string
	Sent   float64
}

// families maps the Collector's sent counters to signals. Newer
// Collectors add _total; older ones don't.
var families = map[string]string{
	"otelcol_exporter_sent_log_records":   "logs",
	"otelcol_exporter_sent_spans":         "traces",
	"otelcol_exporter_sent_metric_points": "metrics",
}

// Telemetry reads url (the Collector's /metrics) and returns each
// exporter's sent counters, summed over the other labels (a Collector
// with several pipelines exporting through one exporter reports it once
// per pipeline in some versions).
func Telemetry(ctx context.Context, client *http.Client, url string) ([]Sent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c := httpx.DefaultClient()
	if client != nil {
		c = client
	}
	if c.CheckRedirect == nil {
		cp := *c
		cp.CheckRedirect = httpx.CheckRedirect
		c = &cp
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("otelcol: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("otelcol: %s: %s", url, resp.Status)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	// A Collector's own metrics are a few kilobytes; a URL pointing at
	// something else mustn't stream unbounded into the parser.
	fams, err := parser.TextToMetricFamilies(httpx.Limit(resp.Body, maxTelemetryBytes))
	if err != nil {
		return nil, fmt.Errorf("otelcol: %s: %w", url, err)
	}
	return parse(fams)
}

func parse(fams map[string]*dto.MetricFamily) ([]Sent, error) {
	sums := map[[2]string]float64{}
	found := false
	for name, f := range fams {
		signal, ok := families[strings.TrimSuffix(name, "_total")]
		if !ok {
			continue
		}
		found = true
		for _, m := range f.Metric {
			exp := ""
			for _, l := range m.Label {
				if l.GetName() == "exporter" {
					exp = l.GetValue()
				}
			}
			if exp == "" {
				continue
			}
			v := m.GetCounter().GetValue()
			if f.GetType() == dto.MetricType_UNTYPED {
				v = m.GetUntyped().GetValue()
			}
			sums[[2]string{exp, signal}] += v
		}
	}
	if !found {
		return nil, fmt.Errorf("otelcol: no otelcol_exporter_sent_* counters: not a Collector's own metrics, or no exporter has sent yet")
	}
	out := make([]Sent, 0, len(sums))
	for k, v := range sums {
		out = append(out, Sent{Exporter: k[0], Signal: k[1], Sent: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Exporter != out[j].Exporter {
			return out[i].Exporter < out[j].Exporter
		}
		return out[i].Signal < out[j].Signal
	})
	return out, nil
}
