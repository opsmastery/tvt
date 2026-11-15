package scan

import (
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// TestPipelineTargets (S6 item 4): one match is the target; none or two
// is unmapped with the reason; labels follow their metric; a stream signal
// takes its streams' pipeline; a recording rule's output is the ruler's.
func TestPipelineTargets(t *testing.T) {
	in := baseInputs()
	in.Pipelines = []config.Pipeline{
		{Name: "prometheus-scrape", Kind: "prometheus", Handles: []string{`{job=~"prometheus|loki"}`}},
		{Name: "alloy-apps", Kind: "alloy", Handles: []string{`{job=~"shop|node"}`, `{app="shop"}`}},
		{Name: "legacy", Kind: "alloy", Handles: []string{`{job="node", instance="old:9100"}`}},
	}
	in.LabelMetrics = collect.LabelMetrics{
		"job": {
			"shop": {"shop_http_requests_total", "shop_cart_operations_total", "shop_http_request_duration_seconds_bucket"},
			"node": {"node_cpu_seconds_total", "node_load1"},
		},
		"instance": {
			"shop:8080":          {"shop_http_requests_total", "shop_cart_operations_total", "shop_http_request_duration_seconds_bucket"},
			"node-exporter:9100": {"node_cpu_seconds_total"},
			"old:9100":           {"node_load1"},
		},
	}
	in.StreamVolumes = []collect.StreamVolume{
		{Labels: map[string]string{"app": "shop", "level": "info"}, Bytes: 100, Window: 24 * 3600e9},
		{Labels: map[string]string{"app": "reconciler", "level": "error"}, Bytes: 10, Window: 24 * 3600e9},
	}
	in.Rules = append(in.Rules, collect.Rule{Source: "loki", Group: "g", Name: "shop_logs:rate5m", Kind: collect.RuleRecord, Language: "logql",
		Expr: `sum(rate({app=~"shop|reconciler"}[5m]))`})
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]graph.PipelineTarget{}
	for _, pt := range out.PipelineTargets {
		got[pt.Subject] = pt
	}
	for subject, want := range map[string]string{
		"prom:metric:shop_http_requests_total":                        "alloy-apps",
		"prom:label:shop_cart_operations_total:user_id":               "alloy-apps",
		"prom:metric:node_cpu_seconds_total":                          "alloy-apps",
		`stream:{app="shop", level="info"}`:                           "alloy-apps",
		"prom:metric:route:shop_http_request_duration_seconds:p99_5m": "",
		`stream:{app="reconciler", level="error"}`:                    "",
		"prom:metric:node_load1":                                      "", // alloy-apps and legacy both claim it
	} {
		pt, ok := got[subject]
		if !ok || pt.Pipeline != want {
			t.Errorf("%s: %+v, want pipeline %q", subject, pt, want)
		}
	}
	for subject, note := range map[string]string{
		"prom:metric:route:shop_http_request_duration_seconds:p99_5m": PipelineRuleOutput,
		`stream:{app="reconciler", level="error"}`:                    PipelineNoMatch,
		"prom:metric:node_load1":                                      "claimed by several pipelines: alloy-apps, legacy",
	} {
		if got[subject].Note != note {
			t.Errorf("%s: note %q, want %q", subject, got[subject].Note, note)
		}
	}
	// The recording rule's stream selector spans both streams: one claimed,
	// one not.
	for id, pt := range got {
		if strings.HasPrefix(id, "loki:stream:") && (pt.Pipeline != "" || !strings.Contains(pt.Note, "no single pipeline")) {
			t.Errorf("%s: %+v", id, pt)
		}
	}
	var src graph.SourceCoverage
	for _, s := range out.Sources {
		if s.Source == SourcePipelines {
			src = s
		}
	}
	if !src.Configured || src.Coverage <= 0 || src.Coverage >= 1 || !strings.Contains(src.Gap, "node_load1: unmapped") || !strings.Contains(src.Gap, `stream {app="reconciler", level="error"}: unmapped`) {
		t.Errorf("source %+v", src)
	}
	if out.LogCoverage != 0 {
		t.Errorf("LogCoverage %v: the pipelines source mustn't count", out.LogCoverage)
	}

	// A handled label that wasn't read leaves metrics unmapped, saying why.
	in.LabelMetrics, in.LabelMetricsErr = nil, "prometheus: 503"
	out, err = Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range out.PipelineTargets {
		if pt.Subject == "prom:metric:shop_http_requests_total" && (pt.Pipeline != "" || !strings.Contains(pt.Note, "labels app, instance, job weren't read (prometheus: 503)")) {
			t.Errorf("unread label: %+v", pt)
		}
	}
	// A truncated lookup (some values read) maps nothing either: a cut-off
	// value could hold the metric's series on another pipeline.
	in.LabelMetrics = collect.LabelMetrics{"job": {"shop": {"shop_http_requests_total"}}}
	in.LabelMetricsErr = "prometheus: truncated: label job has more than 500 values"
	out, _ = Build(in)
	for _, pt := range out.PipelineTargets {
		if strings.HasPrefix(pt.Subject, "prom:metric:") && pt.Pipeline != "" {
			t.Errorf("mapped from a truncated lookup: %+v", pt)
		}
	}
	// No pipelines: nothing mapped, no source.
	in.Pipelines = nil
	out, _ = Build(in)
	if len(out.PipelineTargets) != 0 {
		t.Errorf("targets without pipelines: %v", out.PipelineTargets)
	}
}
