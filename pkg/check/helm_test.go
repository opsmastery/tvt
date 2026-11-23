package check

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/report"
)

func mapFiles(m map[string]string) Files {
	return func(p string) ([]byte, error) {
		if s, ok := m[p]; ok {
			return []byte(s), nil
		}
		return nil, fs.ErrNotExist
	}
}

func rulesOf(r Result) map[string]int {
	out := map[string]int{}
	for _, f := range r.Findings {
		out[f.Rule]++
	}
	return out
}

// TestHelmOverlay (S8 item 11): an overlay list where the chart has
// default rules must keep them, or the chart's own drops are lost.
func TestHelmOverlay(t *testing.T) {
	defaults, err := os.ReadFile("../../testdata/kube-prometheus-stack/values.yaml")
	if err != nil {
		t.Skip("no kube-prometheus-stack chart values in testdata")
	}
	m := report.Manifest{Version: 2, GeneratedAt: time.Now(), PipelineDefs: []report.PipelineDef{{Name: "kps", Kind: "prometheus-operator",
		HelmValues: &report.ManifestHelm{File: "values.yaml", Defaults: "chart-values.yaml"}}}}
	base := "kubelet:\n  enabled: true\n"
	lose := base + "  serviceMonitor:\n    cAdvisorMetricRelabelings:\n      - sourceLabels: [__name__]\n        regex: container_tasks_state\n        action: drop\n"
	r := Run(Input{Manifest: m, Now: time.Now(), SkipConfirmation: true,
		Base: mapFiles(map[string]string{"values.yaml": base, "chart-values.yaml": string(defaults)}),
		Head: mapFiles(map[string]string{"values.yaml": lose, "chart-values.yaml": string(defaults)})})
	if rulesOf(r)[RuleHelm] != 1 {
		t.Errorf("an overlay that loses the chart's cAdvisor drops passed: %v", r.Findings)
	}
	unchanged := Run(Input{Manifest: m, Now: time.Now(), SkipConfirmation: true,
		Base: mapFiles(map[string]string{"values.yaml": lose, "chart-values.yaml": string(defaults)}),
		Head: mapFiles(map[string]string{"values.yaml": lose, "chart-values.yaml": string(defaults)})})
	if !unchanged.Pass() {
		t.Errorf("an unchanged overlay was refused: %v", unchanged.Findings)
	}
}

// TestUnselectedMonitor (item 11): editing a monitor no Prometheus
// selects changes nothing, so check says so.
func TestUnselectedMonitor(t *testing.T) {
	mon := func(regex string) string {
		return "apiVersion: monitoring.coreos.com/v1\nkind: ServiceMonitor\nmetadata:\n  name: shop\n  namespace: shop\nspec:\n  endpoints:\n    - port: http\n      metricRelabelings:\n        - sourceLabels: [__name__]\n          regex: " + regex + "\n          action: drop\n"
	}
	m := report.Manifest{Version: 2, GeneratedAt: time.Now(), UnselectedMonitors: []string{"ServiceMonitor shop/shop"},
		PipelineDefs: []report.PipelineDef{{Name: "ops", Kind: "prometheus-operator", Files: []string{"shop.yaml"}}}}
	r := Run(Input{Manifest: m, Now: time.Now(), SkipConfirmation: true,
		Base: mapFiles(map[string]string{"shop.yaml": mon("a")}), Head: mapFiles(map[string]string{"shop.yaml": mon("a|b")})})
	if rulesOf(r)[RuleHelm] != 1 || !strings.Contains(r.Findings[0].Reason, "no Prometheus selects") {
		t.Errorf("an edit to an unselected monitor passed: %v", r.Findings)
	}
}

// TestDeltaReplicas (item 12): the delta-counter label removal behind
// several Collector replicas with no load balancing.
func TestDeltaReplicas(t *testing.T) {
	base := `processors:
  delta_to_cumulative: {}
service:
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [delta_to_cumulative]
      exporters: [prometheus_remote_write]
`
	head := strings.Replace(base, "  delta_to_cumulative: {}\n", `  delta_to_cumulative: {}
  transform/tvt_aggregate:
    metric_statements:
      - 'delete_key(datapoint.attributes, "session.id")'
`, 1)
	head = strings.Replace(head, "processors: [delta_to_cumulative]", "processors: [transform/tvt_aggregate, delta_to_cumulative]", 1)
	for _, c := range []struct {
		replicas int
		lb       bool
		want     int
	}{{1, false, 0}, {3, false, 1}, {3, true, 0}} {
		m := report.Manifest{Version: 2, GeneratedAt: time.Now(), PipelineDefs: []report.PipelineDef{{Name: "agent", Kind: "otelcol",
			Files: []string{"config.yaml"}, Replicas: c.replicas, LoadBalanced: c.lb}}}
		r := Run(Input{Manifest: m, Now: time.Now(), SkipConfirmation: true,
			Base: mapFiles(map[string]string{"config.yaml": base}), Head: mapFiles(map[string]string{"config.yaml": head})})
		if got := rulesOf(r)[RuleDeltaReplicas]; got != c.want {
			t.Errorf("replicas %d, load balanced %v: %d delta_replicas findings, want %d: %v", c.replicas, c.lb, got, c.want, r.Findings)
		}
	}
}
