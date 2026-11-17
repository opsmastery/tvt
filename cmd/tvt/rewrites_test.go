package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
)

// TestRewrites checks the writers state's rewrites ground truth (S7 item
// 9): prometheus.relabel "spans" (regex = sys.env(...)) holds the span
// metrics and target_info as an evidence gap, with no proposal for them,
// and the shop_search rename leaves shop_search_requests_total keep_high,
// mapped, and reported under its stored name only.
func TestRewrites(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "writers" {
			st = s
		}
	}
	if st.Name == "" {
		t.Fatal("no writers state in benchmark.yaml")
	}
	db := benchDB(t, st, dirs[st.Name])
	in, _, err := readLatest(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]graph.PipelineTarget{}
	for _, pt := range in.PipelineTargets {
		targets[pt.Subject] = pt
	}
	held := 0
	for subj, pt := range targets {
		name, ok := strings.CutPrefix(subj, "prom:metric:")
		if !ok {
			continue
		}
		span := strings.HasPrefix(name, "traces_span_metrics_") || name == "target_info"
		isHeld := strings.HasPrefix(pt.Note, scan.PipelineUnfollowable)
		if span != isHeld {
			t.Errorf("%s: held %v (note %q), want %v", name, isHeld, pt.Note, span)
		}
		if isHeld {
			held++
			if pt.Pipeline != "" || !strings.Contains(pt.Note, `prometheus.relabel "spans"`) || !strings.Contains(pt.Note, "sys.env") {
				t.Errorf("%s: target %+v", name, pt)
			}
		}
	}
	if held == 0 {
		t.Fatal("no span metric held")
	}
	t.Logf("%d metrics held by the spans rule", held)

	r := report.Build(in, 0)
	for _, rec := range r.Recommendations {
		if strings.HasPrefix(rec.Signal.Name, "traces_span_metrics_") && rec.Proposable {
			t.Errorf("%s is proposable while the spans rule is a gap", rec.ID)
		}
		// The rename matches shop_search_queries_total only; its _created
		// series is stored under the source name and is a real signal.
		if rec.Signal.Name == "shop_search_queries_total" {
			t.Errorf("%s: a source name that's never stored was reported", rec.ID)
		}
	}
	gap := ""
	for _, s := range r.Sources {
		if s.Name == scan.SourcePipelines {
			gap = s.Gap
		}
	}
	if !strings.Contains(gap, `pipeline alloy-apps: prometheus.relabel "spans" in alloy/config.alloy can't be followed`) {
		t.Errorf("pipelines gap lacks the spans rule:\n%s", gap)
	}
	var search graph.Signal
	for _, s := range in.Signals {
		if s.ID == "prom:metric:shop_search_requests_total" {
			search = s
		}
	}
	if search.Class != "keep_high" || targets[search.ID].Pipeline != "alloy-apps" {
		t.Errorf("shop_search_requests_total: class %q, pipeline %q; want keep_high in alloy-apps", search.Class, targets[search.ID].Pipeline)
	}
}
