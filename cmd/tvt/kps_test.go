package main

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/kube"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
)

// TestKubePrometheusStack (S6 item 8): the chart's rules, dashboards and
// monitors, rendered at the pinned version, read as declared evidence.
// Every expected value below was checked by hand against
// testdata/kube-prometheus-stack/render.yaml (grep and the dashboards'
// JSON), not taken from tvt's output.
func TestKubePrometheusStack(t *testing.T) {
	objs, err := kube.Load([]string{filepath.Join("..", "..", "testdata", "kube-prometheus-stack", "render.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	rules, err := kube.Rules(objs)
	if err != nil {
		t.Fatal(err)
	}
	dashboards, err := kube.Dashboards(objs)
	if err != nil {
		t.Fatal(err)
	}
	drops, err := kube.Drops(objs, kube.Resolve(objs, "monitoring"))
	if err != nil {
		t.Fatal(err)
	}

	// 155 `alert:` and 95 `record:` lines in 35 PrometheusRules; 29
	// ConfigMaps labelled grafana_dashboard, one dashboard each.
	var alerts, records int
	for _, r := range rules {
		if r.Kind == collect.RuleAlert {
			alerts++
		} else {
			records++
		}
	}
	if alerts != 155 || records != 95 {
		t.Errorf("rules: %d alerts, %d records, want 155 and 95", alerts, records)
	}
	if len(dashboards) != 29 {
		t.Errorf("%d dashboards, want 29", len(dashboards))
	}
	for _, d := range dashboards {
		for _, p := range d.Panels {
			for _, q := range p.Queries {
				if q.DatasourceType != "prometheus" && q.DatasourceType != "alertmanager" {
					t.Errorf("%s panel %d: datasource %q (%s)", d.UID, p.ID, q.DatasourceType, q.DatasourceUID)
				}
			}
		}
	}

	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	out, err := scan.Build(scan.Inputs{Start: start, Window: 30 * 24 * time.Hour, MetricsSource: "prom", OrgID: 1, Rules: rules, Dashboards: dashboards})
	if err != nil {
		t.Fatal(err)
	}
	direct, via := map[string][]string{}, map[string][]string{}
	for _, r := range out.References {
		if r.Sweep {
			continue
		}
		if r.Via != "" {
			via[r.SignalID] = append(via[r.SignalID], r.ConsumerID)
		} else {
			direct[r.SignalID] = append(direct[r.SignalID], r.ConsumerID)
		}
	}
	const ns = "rule:prometheus:monitoring/kps-kube-prometheus-stack-"
	for _, tc := range []struct {
		metric      string
		direct, via []string
	}{
		{
			// Kept by the kubelet monitor although its neighbours go: the
			// recording rule and two Memory Quota panels read it, and the
			// Node (Pods) panel through the rule's output.
			metric: "container_memory_swap",
			direct: []string{
				ns + "k8s.rules.container-memory-swap/k8s.rules.container_memory_swap/node_namespace_pod_container:container_memory_swap",
				"panel:1:6581e46e4e5c7ba40a07646395ef7b23:5:7",
				"panel:1:85a562078cdf77779eaa1add43ccec1e:8:7",
			},
			via: []string{"panel:1:200ac8fdbfbb74b39aff88118e4d1c2c:5:7"},
		},
		{
			metric: "kube_pod_container_status_waiting_reason",
			direct: []string{
				ns + "kubernetes-apps/kubernetes-apps/KubeContainerWaiting",
				ns + "kubernetes-apps/kubernetes-apps/KubePodCrashLooping",
			},
		},
	} {
		id := "prom:metric:" + tc.metric
		got := dedupe(direct[id])
		if !slices.Equal(got, dedupe(tc.direct)) {
			t.Errorf("%s direct readers:\n got  %v\n want %v", tc.metric, got, tc.direct)
		}
		if got := dedupe(via[id]); !slices.Equal(got, dedupe(tc.via)) {
			t.Errorf("%s readers via a recording rule:\n got  %v\n want %v", tc.metric, got, tc.via)
		}
	}

	// The kubelet's cAdvisor drops (whole metrics, on __name__ alone):
	// these are gone at scrape, and nothing in the chart reads them.
	dropped := []string{
		"container_cpu_cfs_throttled_seconds_total", "container_cpu_load_average_10s",
		"container_cpu_system_seconds_total", "container_cpu_user_seconds_total",
		"container_fs_io_current", "container_fs_writes_merged_total",
		"container_memory_mapped_file", "container_file_descriptors",
		"container_tasks_state", "container_threads_max", "container_spec_cpu_quota",
	}
	drops1 := func(name string) bool {
		return slices.ContainsFunc(drops, func(d kube.Drop) bool { return d.DropsMetric(name) })
	}
	for _, m := range dropped {
		if !drops1(m) {
			t.Errorf("%s: no whole-metric drop matches it", m)
		}
	}
	if drops1("container_memory_swap") || drops1("container_memory_working_set_bytes") {
		t.Error("container_memory_swap or _working_set_bytes matched a drop")
	}
	// tvt agrees with the chart: no metric it reads is dropped whole.
	for _, s := range out.Signals {
		if s.Kind != graph.KindMetric || len(direct[s.ID])+len(via[s.ID]) == 0 {
			continue
		}
		if drops1(s.Name) {
			t.Errorf("%s is dropped at scrape but read by %v", s.Name, dedupe(append(direct[s.ID], via[s.ID]...)))
		}
	}
	// Five whole-metric drops (cAdvisor cpu, fs, mapped_file, fd/tasks/
	// threads, spec) and five series-level ones (the API server's and
	// kubelet's le buckets, memory_failures scope, CNI interfaces, id
	// without pod).
	var whole, partial int
	for _, d := range drops {
		if d.Whole() {
			whole++
		} else {
			partial++
			if !strings.Contains(strings.Join(d.Labels, ","), "__name__") && d.Regex != ".+;" {
				t.Errorf("partial drop %+v", d)
			}
		}
	}
	if whole != 5 || partial != 5 {
		t.Errorf("drops: %d whole, %d partial, want 5 and 5", whole, partial)
	}
}

func dedupe(s []string) []string {
	s = slices.Clone(s)
	sort.Strings(s)
	return slices.Compact(s)
}
