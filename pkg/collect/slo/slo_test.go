package slo

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/collect"
)

func TestFiles(t *testing.T) {
	got, err := Files{Paths: []string{"testdata"}}.SLOs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		format, name, origin, unreadable string
		exprs                            []string
	}
	var rows []row
	for _, s := range got {
		r := row{format: s.Format, name: s.Name, origin: filepath.Base(s.Origin), unreadable: s.Unreadable}
		for _, q := range s.Queries {
			r.exprs = append(r.exprs, q.Language+": "+q.Expr)
		}
		if r.unreadable != "" {
			r.unreadable = "yes"
		}
		rows = append(rows, r)
	}
	want := []row{
		{"openslo", "search/search-availability", "openslo.yaml", "", []string{
			`promql: sum(rate(search_requests_total{status="ok"}[5m]))`, `promql: sum(rate(search_requests_total[5m]))`}},
		{"openslo", "ingest/ingest-logs", "openslo.yaml", "", []string{`logql: sum(rate({app="ingest"} |= "error" [5m]))`}},
		{"openslo", "legacy/datadog-one", "openslo.yaml", "yes", nil},
		{"openslo", "legacy/dangling", "openslo.yaml", "yes", nil},
		{"pyrra", "monitoring/api-errors", "pyrra.yaml", "", []string{
			`promql: sum by (route) (rate(api_requests_total{code=~"5.."}[5m]))`, `promql: sum by (route) (rate(api_requests_total[5m]))`}},
		{"pyrra", "monitoring/api-latency", "pyrra.yaml", "", []string{
			`promql: sum (rate(api_duration_seconds_bucket{le="0.5"}[5m]))`, `promql: sum (rate(api_duration_seconds_count[5m]))`}},
		{"sloth", "payments/availability", "sloth-k8s.yaml", "", []string{
			`promql: sum(rate(payments_total{result="error"}[5m]))`, `promql: sum(rate(payments_total[5m]))`}},
		{"sloth", "checkout/requests-availability", "sloth.yml", "", []string{
			`promql: sum(rate(http_requests_total{job="checkout",code=~"5.."}[5m]))`, `promql: sum(rate(http_requests_total{job="checkout"}[5m]))`}},
		{"sloth", "checkout/latency", "sloth.yml", "", []string{
			`promql: 1 - (sum(rate(http_duration_bucket{le="0.5"}[5m])) / sum(rate(http_duration_count[5m])))`}},
		{"sloth", "checkout/plugin", "sloth.yml", "yes", nil},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("SLOs:\n got %#v\nwant %#v", rows, want)
	}
}

func TestFilesFailClosed(t *testing.T) {
	ctx := context.Background()
	if _, err := (Files{Paths: []string{"testdata/nope"}}).SLOs(ctx); err == nil {
		t.Error("missing path: want an error")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("slos: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Files{Paths: []string{dir}}).SLOs(ctx); err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("invalid YAML: err = %v, want one naming the file", err)
	}
}

func TestParseIgnoresOtherDocuments(t *testing.T) {
	got, err := Parse([]byte("apiVersion: v1\nkind: ConfigMap\n---\n- a list\n---\nplain\n"), "x")
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want nothing", got, err)
	}
}

func TestOpenSLOOtherVersion(t *testing.T) {
	got, err := Parse([]byte("apiVersion: openslo/v1alpha\nkind: SLO\nmetadata: {name: old}\n"), "x")
	if err != nil || len(got) != 1 || got[0].Unreadable == "" {
		t.Errorf("got %+v, %v; want one unreadable SLO", got, err)
	}
}

var _ = collect.SLO{}

// TestAlertsAndServices (S6 item 3): each format's service, short name and
// the alert it generates, which SLO coverage looks for in the rulers.
func TestAlertsAndServices(t *testing.T) {
	for _, c := range []struct {
		doc                   string
		service, short, alert string
	}{
		{`version: prometheus/v1
service: shop
slos:
  - name: a
    sli: {events: {error_query: e, total_query: t}}
    alerting: {name: ShopABurn, page_alert: {labels: {severity: critical}}}`, "shop", "a", "ShopABurn"},
		{`version: prometheus/v1
service: shop
slos:
  - name: a
    sli: {events: {error_query: e, total_query: t}}
    alerting: {name: ShopABurn, page_alert: {disable: true}, ticket_alert: {disable: true}}`, "shop", "a", ""},
		{`apiVersion: pyrra.dev/v1alpha1
kind: ServiceLevelObjective
metadata: {name: up, namespace: monitoring}
spec:
  indicator: {ratio: {errors: {metric: e}, total: {metric: t}}}`, "", "up", "ErrorBudgetBurn"},
		{`apiVersion: pyrra.dev/v1alpha1
kind: ServiceLevelObjective
metadata: {name: up, namespace: monitoring}
spec:
  alerting: {disabled: true}
  indicator: {ratio: {errors: {metric: e}, total: {metric: t}}}`, "", "up", ""},
		{`apiVersion: openslo/v1
kind: SLO
metadata: {name: lat}
spec:
  service: web
  indicator:
    spec:
      thresholdMetric: {metricSource: {type: Prometheus, spec: {query: x}}}`, "web", "lat", ""},
	} {
		slos, err := Parse([]byte(c.doc), "x.yml")
		if err != nil || len(slos) != 1 {
			t.Fatalf("Parse: %v, %+v", err, slos)
		}
		if s := slos[0]; s.Service != c.service || s.Short != c.short || s.Alert != c.alert {
			t.Errorf("%s: service %q short %q alert %q, want %q %q %q", s.Name, s.Service, s.Short, s.Alert, c.service, c.short, c.alert)
		}
	}
}
