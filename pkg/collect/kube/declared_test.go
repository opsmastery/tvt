package kube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
)

func loadYAML(t *testing.T, body string) []Object {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	objs, err := Load([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func TestRules(t *testing.T) {
	objs := loadYAML(t, `
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata: {name: r, namespace: mon}
spec:
  groups:
  - name: g
    interval: 1m
    rules:
    - record: job:up:sum
      expr: sum by (job) (up)
    - alert: Down
      expr: job:up:sum == 0
      labels: {severity: critical}
      annotations: {runbook_url: https://example.com/down}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: not-a-rule}
`)
	rules, err := Rules(objs)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("%d rules: %+v", len(rules), rules)
	}
	rec, alert := rules[0], rules[1]
	if rec.Kind != collect.RuleRecord || rec.Name != "job:up:sum" || rec.Namespace != "mon/r" || rec.Group != "g" || rec.Interval != time.Minute || rec.Language != "promql" {
		t.Errorf("record: %+v", rec)
	}
	if alert.Kind != collect.RuleAlert || alert.Labels["severity"] != "critical" || alert.Annotations["runbook_url"] == "" {
		t.Errorf("alert: %+v", alert)
	}

	for name, rule := range map[string]string{
		"both":     "- {alert: A, record: b, expr: up}",
		"neither":  "- {expr: up}",
		"no expr":  "- {alert: A}",
		"interval": "- {alert: A, expr: up}\n    interval: soon",
	} {
		body := "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata: {name: r}\nspec:\n  groups:\n  - name: g\n    rules:\n    " + rule + "\n"
		if name == "interval" {
			body = "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata: {name: r}\nspec:\n  groups:\n  - name: g\n    interval: soon\n    rules:\n    - {alert: A, expr: up}\n"
		}
		if _, err := Rules(loadYAML(t, body)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDashboards(t *testing.T) {
	objs := loadYAML(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: ds, labels: {grafana_datasource: "1"}}
data:
  datasource.yaml: |
    apiVersion: 1
    datasources:
    - {name: Prometheus, type: prometheus, uid: prom, isDefault: true}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: d, labels: {grafana_dashboard: "1"}}
data:
  d.json: '{"uid":"d1","title":"D","panels":[{"id":1,"title":"P","targets":[{"expr":"up"}]}]}'
  notes.txt: ignored
`)
	ds, err := Dashboards(objs)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].UID != "d1" || len(ds[0].Panels) != 1 || ds[0].Panels[0].Queries[0].DatasourceUID != "prom" || ds[0].Panels[0].Queries[0].DatasourceType != "prometheus" {
		t.Fatalf("%+v", ds)
	}

	// No datasource list: the default can't be resolved. A library panel
	// isn't in the file. Either is an error, never a dashboard reading
	// nothing.
	for name, body := range map[string]string{
		"no list":       `{"uid":"d1","panels":[{"id":1,"targets":[{"expr":"up"}]}]}`,
		"library panel": `{"uid":"d1","panels":[{"id":1,"libraryPanel":{"uid":"lib"}}]}`,
		"no uid":        `{"panels":[]}`,
		"bad json":      `{`,
	} {
		o := []Object{{Kind: "ConfigMap", Name: "d", Labels: map[string]string{DashboardLabel: "1"}, Data: map[string]string{"d.json": body}}}
		if name != "no list" {
			o = append(o, objs[0])
		}
		if _, err := Dashboards(o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDrops(t *testing.T) {
	objs := loadYAML(t, `
apiVersion: monitoring.coreos.com/v1
kind: Prometheus
metadata: {name: p, namespace: mon}
spec:
  serviceMonitorSelector: {matchLabels: {release: x}}
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata: {name: sel, namespace: mon, labels: {release: x}}
spec:
  endpoints:
  - port: http
    metricRelabelings:
    - {action: drop, sourceLabels: [__name__], regex: foo_total}
    - {action: Drop, sourceLabels: [__name__, le], regex: "bar_bucket;0.5"}
    - {action: keep, sourceLabels: [__name__], regex: kept}
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata: {name: unselected, namespace: mon}
spec:
  endpoints:
  - metricRelabelings:
    - {action: drop, sourceLabels: [__name__], regex: .*}
`)
	drops, err := Drops(objs, Resolve(objs, "mon"))
	if err != nil {
		t.Fatal(err)
	}
	if len(drops) != 2 {
		t.Fatalf("%+v", drops)
	}
	whole, part := drops[0], drops[1]
	if !whole.Whole() || !whole.DropsMetric("foo_total") || whole.Monitor != "ServiceMonitor/mon/sel" || whole.Endpoint != "http" {
		t.Errorf("whole: %+v", whole)
	}
	// Prometheus anchors relabel regexes.
	if whole.DropsMetric("foo_total_extra") || whole.DropsMetric("xfoo_total") {
		t.Error("unanchored match")
	}
	if part.Whole() || part.DropsMetric("bar_bucket") || strings.Join(part.Labels, ",") != "__name__,le" {
		t.Errorf("partial: %+v", part)
	}

	bad := loadYAML(t, `
apiVersion: monitoring.coreos.com/v1
kind: Prometheus
metadata: {name: p, namespace: mon}
spec: {serviceMonitorSelector: {}}
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata: {name: m, namespace: mon}
spec:
  endpoints:
  - metricRelabelings: [{action: drop, sourceLabels: [__name__], regex: "("}]
`)
	if _, err := Drops(bad, Resolve(bad, "mon")); err == nil {
		t.Error("invalid regex: no error")
	}
}
