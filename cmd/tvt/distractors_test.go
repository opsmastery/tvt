package main

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/report"
)

// TestBenchmarkDistractors (S5 item 2): the three distractors tvt-lab #15
// seeded into the before-deploy state change no recommendation. The
// benchmark scores them through ground truth; this pins down how each one
// could go wrong.
func TestBenchmarkDistractors(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	i := slices.IndexFunc(bm.States, func(s benchState) bool { return s.Name == "before-deploy" })
	if i < 0 {
		t.Fatal("benchmark.yaml has no before-deploy state")
	}
	g, r, gt := scanState(t, bm.States[i], dirs["before-deploy"])
	recs := map[string]report.Recommendation{}
	for _, rec := range r.Recommendations {
		recs[rec.Signal.ID] = rec
	}
	confirm := map[string]bool{}
	for _, c := range r.Confirm {
		confirm[c.Signal.ID] = true
	}

	t.Run("near-identical name", func(t *testing.T) {
		// shop_cart_operation_total is one letter off the read
		// shop_cart_operations_total: it gets its own drop and borrows no
		// reader, and the read metric stays protected.
		near, read := "prom:metric:shop_cart_operation_total", "prom:metric:shop_cart_operations_total"
		if gt.Metrics["shop_cart_operation_total"].Class != "drop_metric" {
			t.Fatalf("ground truth: shop_cart_operation_total is %+v", gt.Metrics["shop_cart_operation_total"])
		}
		if rec, ok := recs[near]; !ok || rec.Action != report.ActionDrop {
			t.Errorf("%s: recommendation %+v (found %v), want a drop", near, rec.Action, ok)
		}
		for _, ref := range g.refsTo(near) {
			t.Errorf("%s is read by %s (%s)", near, ref.ConsumerID, g.consumers[ref.ConsumerID].Expr)
		}
		if c := g.signals[read].Class; c != "keep_high" {
			t.Errorf("%s: class %q, want keep_high", read, c)
		}
		if _, ok := recs[read]; ok {
			t.Errorf("%s is recommended", read)
		}
	})

	t.Run("metric through a variable", func(t *testing.T) {
		// Shop Overview panel 9 reads ${counter}, a custom variable over two
		// already-read counters: it reads exactly those, viewed, and no
		// other metric.
		var ids []string
		read := map[string]bool{}
		for id, c := range g.consumers {
			if !strings.HasPrefix(id, "panel:1:shop-overview:9:") {
				continue
			}
			ids = append(ids, id)
			if c.LastViewed.IsZero() {
				t.Errorf("%s: not viewed", id)
			}
			for _, ref := range g.refs {
				if ref.ConsumerID == id && g.signals[ref.SignalID].Kind == graph.KindMetric {
					read[g.signals[ref.SignalID].Name] = true
				}
			}
		}
		if len(ids) == 0 {
			t.Fatal("no consumer for Shop Overview panel 9")
		}
		var got []string
		for name := range read {
			got = append(got, name)
		}
		sort.Strings(got)
		if want := []string{"shop_cart_operations_total", "shop_http_requests_total"}; !slices.Equal(got, want) {
			t.Errorf("panel 9 (%v) reads %v, want %v", ids, got, want)
		}
	})

	t.Run("background errors", func(t *testing.T) {
		// The reconciler logs one identical error line all day. No
		// recommendation or confirm row may cover its stream: it is an
		// unread error stream (dormant_confirm), never a reroute or a
		// line-level saving.
		labels := streamLabels(t, `{app="reconciler", component="worker", level="error"}`)
		if gt.Streams[`{app="reconciler", component="worker", level="error"}`].Class != "dormant_confirm" {
			t.Fatal("ground truth: the reconciler stream isn't dormant_confirm")
		}
		for _, rec := range r.Recommendations {
			if rec.Signal.Kind == string(graph.KindStream) && selectorMatches(t, rec.Signal.Name, labels) {
				t.Errorf("recommendation %s (%s) covers the reconciler's error stream", rec.ID, rec.Action)
			}
		}
		for _, f := range r.LineFindings {
			if selectorMatches(t, f.Stream, labels) {
				t.Errorf("line finding %s (%s, %q) in the reconciler's error stream", f.ID, f.Kind, f.Pattern)
			}
		}
		for _, ref := range streamRefs(t, g, labels) {
			t.Errorf("the reconciler's stream is read by %s (%s)", ref.ConsumerID, g.consumers[ref.ConsumerID].Expr)
		}
	})
}
