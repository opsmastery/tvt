package scan

import (
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/propose"
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
