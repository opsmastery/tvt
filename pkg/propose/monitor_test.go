package propose

import (
	"os"
	"strings"
	"testing"
)

func kps(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/kube-prometheus-stack/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMonitorJobs(t *testing.T) {
	jobs, unresolved, err := MonitorJobs(kps(t, "render.yaml"), kps(t, "extra-monitors.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for job, ms := range jobs {
		t.Logf("job %s -> %v", job, ms)
	}
	for m, why := range unresolved {
		t.Logf("unresolved %s: %s", m, why)
	}
	want := map[string]string{
		"node-exporter": "ServiceMonitor/monitoring/kps-prometheus-node-exporter",
		"batch/batch":   "PodMonitor/batch/batch",
	}
	for job, ref := range want {
		if len(jobs[job]) != 1 || jobs[job][0].String() != ref {
			t.Errorf("job %s: %v, want %s", job, jobs[job], ref)
		}
	}
}

func TestMonitors(t *testing.T) {
	src := kps(t, "extra-monitors.yaml")
	d := Drop{ID: "kps/payments", Scan: 3, Reason: "2 unread metrics", Metrics: []string{"payments_debug_total", "payments_cache_misses_total"}}
	shop := Drop{ID: "kps/shop", Scan: 3, Metrics: []string{"shop_legacy_queue_depth"}}
	by := map[MonitorRef]Drop{{"ServiceMonitor", "payments", "payments"}: d, {"ServiceMonitor", "shop", "shop"}: shop}
	out, n, err := Monitors(src, by)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("wrote %d monitors, want 2", n)
	}
	added := onlyAdded(t, src, out)
	want := []string{
		// payments: no metricRelabelings yet, so the key comes with it.
		"      # tvt:begin kps/payments scan=3",
		"      # 2 unread metrics",
		"      metricRelabelings:",
		"        - action: drop",
		"          sourceLabels: [__name__]",
		`          regex: "payments_cache_misses_total|payments_debug_total"`,
		"      # tvt:end",
	}
	if !strings.Contains(strings.Join(added, "\n"), strings.Join(want, "\n")) {
		t.Errorf("added:\n%s", strings.Join(added, "\n"))
	}
	// shop: appended to its existing list, after the hand-written rule.
	if !strings.Contains(string(out), "          regex: shop_debug_.*\n        # tvt:begin kps/shop scan=3\n        - action: drop\n") {
		t.Errorf("shop's rule not appended:\n%s", out)
	}
	if again, _, err := Monitors(out, by); err != nil || string(again) != string(out) {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	// The chart's render: the node-exporter monitor's endpoint list
	// (rules at the key's column) takes the rule too.
	r := kps(t, "render.yaml")
	out, n, err = Monitors(r, map[MonitorRef]Drop{{"ServiceMonitor", "monitoring", "kps-kube-prometheus-stack-apiserver"}: d})
	if err != nil || n != 1 {
		t.Fatalf("render: n %d err %v", n, err)
	}
	if !strings.Contains(string(out), "        - le\n      # tvt:begin kps/payments scan=3\n") {
		t.Errorf("apiserver rule misplaced:\n%s", strings.Join(onlyAdded(t, r, out), "\n"))
	}
}
