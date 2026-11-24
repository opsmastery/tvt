package scan

import (
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/propose"
)

// TestHoldUnfollowableAll: a collector rule tvt can't follow (All) holds
// every metric mapped to that pipeline, and its labels, and nothing on
// other pipelines.
func TestHoldUnfollowableAll(t *testing.T) {
	out := []graph.PipelineTarget{
		{Subject: "prom:metric:coding_agent_token_usage_total", Pipeline: "otelcol-agent"},
		{Subject: "prom:label:coding_agent_token_usage_total:session_id", Pipeline: "otelcol-agent"},
		{Subject: "prom:metric:shop_http_requests_total", Pipeline: "alloy-apps"},
	}
	in := Inputs{Rewrites: map[string][]propose.Rewrite{"otelcol-agent": {
		{File: "otelcol/config.yaml", Component: "processor transform/names", Unfollowable: "replace_pattern on the metric name", All: true},
	}}}
	gaps := holdUnfollowable(in, out, "prom:metric:", "prom:label:")
	if out[0].Pipeline != "" || !strings.HasPrefix(out[0].Note, PipelineUnfollowable) || out[1].Pipeline != "" {
		t.Errorf("collector metrics not held: %+v", out[:2])
	}
	if out[2].Pipeline != "alloy-apps" {
		t.Errorf("another pipeline's metric was held: %+v", out[2])
	}
	if len(gaps) != 1 || !strings.Contains(gaps[0], "1 metrics held") {
		t.Errorf("gaps %v", gaps)
	}
}

// TestHoldUnfollowableStreams: a loki.process rule tvt can't follow holds
// the pipeline's streams and stream signals, not its metrics.
func TestHoldUnfollowableStreams(t *testing.T) {
	out := []graph.PipelineTarget{
		{Subject: StreamSubject(`{app="shop", level="info"}`), Pipeline: "alloy-apps"},
		{Subject: `loki:stream:{app="shop", level=~"info|warn"}`, Pipeline: "alloy-apps"},
		{Subject: "prom:metric:shop_http_requests_total", Pipeline: "alloy-apps"},
		{Subject: StreamSubject(`{app="reconciler"}`), Pipeline: "other"},
	}
	in := Inputs{Rewrites: map[string][]propose.Rewrite{"alloy-apps": {
		{File: "alloy/config.alloy", Component: `loki.process "apps"`, Unfollowable: "stage.labels sets stream labels from log content", All: true, Streams: true},
	}}}
	gaps := holdUnfollowable(in, out, "prom:metric:", "prom:label:")
	if out[0].Pipeline != "" || out[1].Pipeline != "" || !strings.HasPrefix(out[1].Note, PipelineUnfollowable) {
		t.Errorf("streams not held: %+v", out[:2])
	}
	if out[2].Pipeline != "alloy-apps" || out[3].Pipeline != "other" {
		t.Errorf("held too much: %+v", out[2:])
	}
	if len(gaps) != 1 || !strings.Contains(gaps[0], "1 streams held") {
		t.Errorf("gaps %v", gaps)
	}
}

// TestHoldLabelRewrites: a rule that sets a label holds that label on the
// metrics that pass it, and only that label; one that can't say which
// labels it sets holds every label of those metrics. Metrics stay mapped.
func TestHoldLabelRewrites(t *testing.T) {
	out := []graph.PipelineTarget{
		{Subject: "prom:metric:shop_http_requests_total", Pipeline: "alloy-apps"},
		{Subject: "prom:label:shop_http_requests_total:instance_pod", Pipeline: "alloy-apps"},
		{Subject: "prom:label:shop_http_requests_total:route", Pipeline: "alloy-apps"},
		{Subject: "prom:label:node_cpu_seconds_total:instance_pod", Pipeline: "alloy-apps"},
		{Subject: "prom:label:agent_tokens_total:session_id", Pipeline: "otelcol-agent"},
	}
	in := Inputs{
		MetricJobs: map[string][]string{"shop_http_requests_total": {"shop"}, "node_cpu_seconds_total": {"node"}},
		Rewrites: map[string][]propose.Rewrite{
			"alloy-apps":    {{File: "config.alloy", Component: `prometheus.relabel "apps"`, Label: true, Labels: []string{"instance_pod"}, Jobs: []string{"shop"}, Unfollowable: "replace sets label instance_pod"}},
			"otelcol-agent": {{File: "c.yaml", Component: "processor attributes/x", Label: true, All: true, Unfollowable: "extract sets labels from a pattern on http.url"}},
		},
	}
	gaps := holdUnfollowable(in, out, "prom:metric:", "prom:label:")
	if out[0].Pipeline != "alloy-apps" {
		t.Errorf("the metric was held: %+v", out[0])
	}
	if out[1].Pipeline != "" || !strings.Contains(out[1].Note, "replace sets label instance_pod") {
		t.Errorf("the set label wasn't held: %+v", out[1])
	}
	if out[2].Pipeline != "alloy-apps" || out[3].Pipeline != "alloy-apps" {
		t.Errorf("held too much: %+v", out[2:4])
	}
	if out[4].Pipeline != "" {
		t.Errorf("unknown labels not held: %+v", out[4])
	}
	if len(gaps) != 2 || !strings.Contains(gaps[0]+gaps[1], "1 labels held") {
		t.Errorf("gaps %v", gaps)
	}
}

// TestHoldLabelRewritesOTelKey: an OTel attribute key is held under the
// label name Prometheus stores it as (deployment.environment ->
// deployment_environment).
func TestHoldLabelRewritesOTelKey(t *testing.T) {
	out := []graph.PipelineTarget{
		{Subject: "prom:label:coding_agent_token_usage_total:deployment_environment", Pipeline: "otelcol-agent"},
		{Subject: "prom:label:coding_agent_token_usage_total:session_id", Pipeline: "otelcol-agent"},
	}
	in := Inputs{Rewrites: map[string][]propose.Rewrite{"otelcol-agent": {
		{File: "c.yaml", Component: "processor attributes/env", Label: true, Labels: []string{"deployment.environment"}, All: true, Unfollowable: "upsert sets label deployment.environment"},
	}}}
	holdUnfollowable(in, out, "prom:metric:", "prom:label:")
	if out[0].Pipeline != "" || out[1].Pipeline != "otelcol-agent" {
		t.Errorf("%+v", out)
	}
	if storedLabel("9lives") != "key_9lives" {
		t.Error(storedLabel("9lives"))
	}
}
