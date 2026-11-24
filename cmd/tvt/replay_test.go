package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/collect/prometheus"
	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/report"
)

// fakeReplay answers like a Prometheus whose metric has series by route
// and user_id: grouping by route survives removing user_id, not route.
type fakeReplay struct{ queries []string }

func (f *fakeReplay) Replay(_ context.Context, q string) ([]prometheus.Sample, error) {
	f.queries = append(f.queries, q)
	if strings.Contains(q, "without (route)") {
		return []prometheus.Sample{{Labels: map[string]string{}, Value: 3}}, nil
	}
	return []prometheus.Sample{{Labels: map[string]string{"route": "/a"}, Value: 1}, {Labels: map[string]string{"route": "/b"}, Value: 2}}, nil
}

// TestReplayLabelRemovals (S8 item 4): the alert groups by route, so
// removing route changes its grouping and is blocked; removing user_id
// leaves it unchanged and goes through. Every replayed query carries the
// replay comment, so query log readers drop it.
func TestReplayLabelRemovals(t *testing.T) {
	in := report.Input{
		Scan: report.Scan{ID: 1, StartedAt: time.Now(), WindowDays: 30},
		Signals: []graph.Signal{
			{ID: "prom:metric:http_requests_total", Kind: graph.KindMetric, Name: "http_requests_total", Source: "prom", Class: "hard_keep"},
		},
		Consumers: []graph.Consumer{{ID: "rule:prometheus:f/g/HighErrors", Kind: graph.ConsumerAlertRule, Name: "HighErrors",
			Expr: `sum by (route) (rate(http_requests_total[5m])) > 1`}},
		References: []graph.Reference{{SignalID: "prom:metric:http_requests_total", ConsumerID: "rule:prometheus:f/g/HighErrors", Evidence: "declared"}},
	}
	rec := func(label string) report.Recommendation {
		return report.Recommendation{ID: "label_removal:prom:label:http_requests_total:" + label, Action: report.ActionLabelRemoval, Pipeline: "scrape",
			Signal: report.SignalRef{ID: "prom:label:http_requests_total:" + label, Kind: "label", Name: label, Parent: "http_requests_total", Source: "prom"}}
	}
	f := &fakeReplay{}
	keep, skipped, lines := replayLabelRemovals(context.Background(), in, []report.Recommendation{rec("route"), rec("user_id")}, f)
	if len(keep) != 1 || keep[0].Signal.Name != "user_id" {
		t.Errorf("kept %v", keep)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Note, "HighErrors: the query returns 1 series without route, 2 as written") {
		t.Errorf("skipped %+v", skipped)
	}
	if len(lines["scrape"]) != 1 || !strings.Contains(lines["scrape"][0], "1 protected queries replayed without user_id, results unchanged") {
		t.Errorf("lines %v", lines)
	}
	for _, q := range f.queries {
		if !collect.IsReplay(q) {
			t.Errorf("a replayed query without the replay comment: %q", q)
		}
	}
}
