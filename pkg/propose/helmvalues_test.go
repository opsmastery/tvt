package propose

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func kpsDrops() map[string]Drop {
	by := map[string]Drop{}
	for _, m := range []struct{ monitor, metric string }{
		{"kps-kube-prometheus-stack-kubelet", "kubelet_runtime_operations_duration_seconds_bucket"},
		{"kps-prometheus-node-exporter", "node_softnet_times_squeezed_total"},
		{"kps-kube-prometheus-stack-apiserver", "apiserver_storage_objects"},
	} {
		for _, k := range KPSValuesKeys(m.monitor) {
			by[k] = Drop{ID: "kps/" + m.monitor, Scan: 5, Metrics: []string{m.metric}}
		}
	}
	return by
}

func TestKPSValuesKeys(t *testing.T) {
	for m, want := range map[string]string{
		"kps-prometheus-node-exporter":         "prometheus-node-exporter.prometheus.monitor.metricRelabelings",
		"kps-kube-prometheus-stack-prometheus": "prometheus.serviceMonitor.metricRelabelings",
		"kps-kube-prometheus-stack-apiserver":  "kubeApiServer.serviceMonitor.metricRelabelings",
		"kps-kube-state-metrics":               "kube-state-metrics.prometheus.monitor.metricRelabelings",
	} {
		if got := KPSValuesKeys(m); len(got) != 1 || got[0] != want {
			t.Errorf("%s: %v, want %s", m, got, want)
		}
	}
	if len(KPSValuesKeys("kps-kube-prometheus-stack-kubelet")) != 3 || KPSValuesKeys("payments") != nil {
		t.Error("kubelet has three keys; a team's own monitor none")
	}
}

func TestHelmValues(t *testing.T) {
	src := read(t, "kps-values.yaml")
	defaults := kps(t, "values.yaml")
	out, err := HelmValues(src, defaults, "", kpsDrops())
	if err != nil {
		t.Fatal(err)
	}
	onlyAdded(t, src, out)
	s := string(out)
	for _, want := range []string{
		// cAdvisor: a new key, carrying the chart's 8 default rules first.
		"    # tvt:begin kps/kps-kube-prometheus-stack-kubelet scan=5\n    cAdvisorMetricRelabelings:\n      # the chart's default rules",
		// The apiserver: appended to the team's list, defaults not added.
		"        sourceLabels: [__name__]\n      # tvt:begin kps/kps-kube-prometheus-stack-apiserver scan=5\n      - action: drop\n",
		// node-exporter: a new top-level key.
		"# tvt:begin kps/kps-prometheus-node-exporter scan=5\nprometheus-node-exporter:\n  prometheus:\n    monitor:\n      metricRelabelings:\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("values lack %q:\n%s", want, s)
		}
	}
	if again, err := HelmValues(out, defaults, "", kpsDrops()); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)\n%s", err, again)
	}
	stripped, _, err := stripBlocks(out, "#")
	if err != nil || string(stripped) != string(src) {
		t.Errorf("stripping gives back the original? %v\n%s", err, stripped)
	}
	// Under an umbrella chart's key.
	umb := "kube-prometheus-stack:\n  grafana:\n    enabled: true\n"
	out, err = HelmValues([]byte(umb), defaults, "kube-prometheus-stack", map[string]Drop{"kubeEtcd.serviceMonitor.metricRelabelings": {ID: "e", Scan: 1, Metrics: []string{"etcd_x"}}})
	if err != nil || !strings.Contains(string(out), "  # tvt:begin e scan=1\n  kubeEtcd:\n    serviceMonitor:\n      metricRelabelings:\n") {
		t.Errorf("umbrella: %v\n%s", err, out)
	}
}

// TestHelmValuesRender renders the chart with the edited overlay (helm
// template at the pinned version; needs helm and the chart repo, so it
// runs with TVT_TEST_HELM=1): every drop lands on its monitor, the
// kubelet's cAdvisor endpoint keeps the chart's default drops, and the
// team's apiserver rule stays.
func TestHelmValuesRender(t *testing.T) {
	if os.Getenv("TVT_TEST_HELM") == "" {
		t.Skip("set TVT_TEST_HELM=1 to render with helm")
	}
	out, err := HelmValues(read(t, "kps-values.yaml"), kps(t, "values.yaml"), "", kpsDrops())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(f, out, 0o644); err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(kps(t, "VERSION")))
	r, err := exec.Command("helm", "template", "kps", "prometheus-community/kube-prometheus-stack", "--version", version, "--namespace", "monitoring", "-f", f).Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	docs := strings.Split(string(r), "\n---\n")
	monitor := func(name string) string {
		for _, d := range docs {
			if strings.Contains(d, "kind: ServiceMonitor") && strings.Contains(d, "\n  name: "+name+"\n") {
				return d
			}
		}
		t.Fatalf("no ServiceMonitor %s", name)
		return ""
	}
	kubelet := monitor("kps-kube-prometheus-stack-kubelet")
	if n := strings.Count(kubelet, "regex: kubelet_runtime_operations_duration_seconds_bucket"); n != 3 {
		t.Errorf("kubelet: the drop is on %d endpoints, want 3", n)
	}
	if !strings.Contains(kubelet, "container_cpu_(cfs_throttled_seconds_total|load_average_10s|system_seconds_total|user_seconds_total)") {
		t.Error("kubelet: the chart's default cAdvisor drops are gone")
	}
	if !strings.Contains(kubelet, "csi_operations|storage_operation_duration") {
		t.Error("kubelet: the chart's default metrics-endpoint drop is gone")
	}
	api := monitor("kps-kube-prometheus-stack-apiserver")
	if !strings.Contains(api, "regex: apiserver_storage_objects") || !strings.Contains(api, "regex: apiserver_watch_events_sizes_bucket") {
		t.Error("apiserver: want the team's rule and tvt's")
	}
	if !strings.Contains(monitor("kps-prometheus-node-exporter"), "regex: node_softnet_times_squeezed_total") {
		t.Error("node-exporter: no drop")
	}
}
