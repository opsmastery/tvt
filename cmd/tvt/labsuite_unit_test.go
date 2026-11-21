package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The applied-change suite's judgement, without a lab.

const federateBody = `# TYPE up untyped
up{instance="shop:8080",job="shop"} 1 1795110757000
up{instance="node-exporter:9100",job="node"} 1 1795110776000
# TYPE node_boot_time_seconds untyped
node_boot_time_seconds{instance="node-exporter:9100",job="node"} 1.7e+09 1795110596652
coding_agent_token_usage_total{job="coding-agent",model="lab-model-large",note="a \"quoted\" value"} 159388 1795110800000
`

func TestParseFederate(t *testing.T) {
	got, err := parseFederate(strings.NewReader(federateBody))
	if err != nil {
		t.Fatal(err)
	}
	if len(got["up"]) != 2 || got["node_boot_time_seconds"][0].Value != 1.7e9 || got["node_boot_time_seconds"][0].TsMs != 1795110596652 {
		t.Errorf("%+v", got)
	}
	if l := got["coding_agent_token_usage_total"][0].Labels; l["model"] != "lab-model-large" || l["note"] != `a "quoted" value` {
		t.Errorf("labels %v", l)
	}
	if _, err := parseFederate(strings.NewReader("not a line {")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestStoppedFlowing(t *testing.T) {
	got, _ := parseFederate(strings.NewReader(federateBody))
	restart := time.UnixMilli(1795110599000) // 17:49:59, as on Nov 19
	if !stopped(got["node_boot_time_seconds"], restart) || flowing(got["node_boot_time_seconds"], restart) {
		t.Error("node_boot_time_seconds should have stopped")
	}
	if stopped(got["up"], restart) || !flowing(got["up"], restart) {
		t.Error("up should be flowing")
	}
	if !stopped(nil, restart) || flowing(nil, restart) {
		t.Error("a metric with no series has stopped")
	}
}

const labPatch = `diff --git a/prometheus/scrape/lab.yml b/prometheus/scrape/lab.yml
+    metric_relabel_configs:
+    # tvt:begin prometheus-scrape/prometheus scan=1 evidence=aaa
+    - source_labels: [__name__]
+      regex: "prometheus_tsdb_head_series|prometheus_build_info"
+      action: drop
+    - source_labels: [__name__]
+      regex: "shop_cart_operations_total"
+      target_label: op_kind
+      replacement: ""
+      action: replace
+    # tvt:end
diff --git a/alloy/config.alloy b/alloy/config.alloy
+  rule {
+    source_labels = ["__name__"]
+    regex         = "node_boot_time_seconds|node_memory_MemTotal_bytes"
+    action        = "drop"
+  }
diff --git a/otelcol/config.yaml b/otelcol/config.yaml
+  interval/tvt:
diff --git a/.tvt/proposal.json b/.tvt/proposal.json
`

func TestPatchParsing(t *testing.T) {
	if got := strings.Join(servicesFor(labPatch), ","); got != "alloy,otelcol,prometheus" {
		t.Errorf("services %s", got)
	}
	// The label replace isn't a drop.
	if got := strings.Join(droppedMetrics(labPatch), ","); got != "node_boot_time_seconds,node_memory_MemTotal_bytes,prometheus_build_info,prometheus_tsdb_head_series" {
		t.Errorf("dropped %s", got)
	}
}

func TestDeltaCheck(t *testing.T) {
	restart := time.UnixMilli(1_000_000)
	poll := func(ts int64, v float64, labels map[string]string) map[string][]sample {
		return map[string][]sample{"c": {{Labels: labels, Value: v, TsMs: ts}}}
	}
	model := map[string]string{"model": "large"}
	withSession := map[string]string{"model": "large", "session_id": "x"}
	if err := deltaCheck([]map[string][]sample{poll(900_000, 5, withSession), poll(1_010_000, 8, model), poll(1_040_000, 16, model)}, "c", restart); err != nil {
		t.Errorf("good run: %v", err)
	}
	if err := deltaCheck([]map[string][]sample{poll(1_010_000, 8, withSession)}, "c", restart); err == nil || !strings.Contains(err.Error(), "session_id") {
		t.Errorf("session label kept: %v", err)
	}
	if err := deltaCheck([]map[string][]sample{poll(1_010_000, 8, model), poll(1_040_000, 5, model)}, "c", restart); err == nil || !strings.Contains(err.Error(), "went down") {
		t.Errorf("counter reset: %v", err)
	}
	if err := deltaCheck([]map[string][]sample{poll(900_000, 8, model)}, "c", restart); err == nil {
		t.Error("no samples after the restart passed")
	}
}

func TestServerTime(t *testing.T) {
	h := http.Header{}
	h.Set("Date", "Sat, 21 Nov 2026 00:05:17 GMT")
	at, ok := serverTime(h)
	if want := time.Date(2026, 11, 21, 0, 5, 18, 0, time.UTC); !ok || !at.Equal(want) {
		t.Errorf("serverTime = %v %v, want %v (the Date rounded up)", at, ok, want)
	}
	// The Nov 21 run: the last old-config sample at 00:05:17.9 on the lab's
	// clock is within the grace of a restart the lab saw end at 00:05:17.
	last := []sample{{TsMs: time.Date(2026, 11, 21, 0, 5, 17, 900e6, time.UTC).UnixMilli()}}
	if !stopped(last, at) {
		t.Error("a sample before the server's restart time counted as still flowing")
	}
	if _, ok := serverTime(http.Header{}); ok {
		t.Error("no Date header read as a time")
	}
}
