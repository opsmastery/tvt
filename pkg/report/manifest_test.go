package report

import (
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func TestManifests(t *testing.T) {
	start := time.Date(2026, 11, 17, 7, 6, 36, 0, time.UTC)
	in := Input{
		Scan: Scan{ID: 7, StartedAt: start},
		Signals: []graph.Signal{
			{ID: "prom:metric:b_alerted", Kind: graph.KindMetric, Name: "b_alerted", Class: "hard_keep", Owner: "shop"},
			{ID: "prom:metric:a_viewed", Kind: graph.KindMetric, Name: "a_viewed", Class: "keep_high"},
			{ID: "prom:metric:c_dormant", Kind: graph.KindMetric, Name: "c_dormant", Class: "dormant", Dormant: "error by name"},
			{ID: "prom:metric:d_unread", Kind: graph.KindMetric, Name: "d_unread", Class: "candidate"},
			{ID: "prom:metric:e_unmapped", Kind: graph.KindMetric, Name: "e_unmapped", Class: "hard_keep"},
			{ID: "prom:metric:f_norepo", Kind: graph.KindMetric, Name: "f_norepo", Class: "hard_keep"},
		},
		Consumers: []graph.Consumer{
			{ID: "rule:alert:Hot", Kind: graph.ConsumerAlertRule, Name: "Hot", Expr: "b_alerted > 1"},
			{ID: "dash:p1", Kind: graph.ConsumerDashboardPanel, Name: "Shop / panel 1"},
			{ID: "qlog:sweep", Kind: graph.ConsumerAdhocQuery, Name: "agent sweep"},
		},
		References: []graph.Reference{
			{SignalID: "prom:metric:b_alerted", ConsumerID: "dash:p1", Evidence: graph.EvidenceObserved},
			{SignalID: "prom:metric:b_alerted", ConsumerID: "rule:alert:Hot", Evidence: graph.EvidenceDeclared},
			{SignalID: "prom:metric:b_alerted", ConsumerID: "qlog:sweep", Evidence: graph.EvidenceObserved, Sweep: true},
			{SignalID: "prom:metric:a_viewed", ConsumerID: "dash:p1", Evidence: graph.EvidenceObserved},
		},
		PipelineTargets: []graph.PipelineTarget{
			{Subject: "prom:metric:a_viewed", Pipeline: "alloy"},
			{Subject: "prom:metric:b_alerted", Pipeline: "scrape"},
			{Subject: "prom:metric:c_dormant", Pipeline: "alloy"},
			{Subject: "prom:metric:d_unread", Pipeline: "alloy"},
			{Subject: "prom:metric:e_unmapped", Note: "claimed by several pipelines"},
			{Subject: "prom:metric:f_norepo", Pipeline: "local"},
		},
		Acknowledged: []Acknowledgement{{Source: "grafana", Reason: "z"}, {Source: "agents", Reason: "a"}},
	}
	repos := map[string]string{"alloy": "github.com/acme/pipes", "scrape": "github.com/acme/pipes", "other": "github.com/acme/other", "local": ""}
	ms := Manifests(in, repos)
	if len(ms) != 2 {
		t.Fatalf("got %d manifests, want 2 (one per repo with a repo): %v", len(ms), ms)
	}
	if o := ms["github.com/acme/other"]; len(o.Signals) != 0 || o.Signals == nil || o.Decisions == nil {
		t.Errorf("other repo: %+v, want empty (not nil) lists", o)
	}
	m := ms["github.com/acme/pipes"]
	if m.Version != ManifestVersion || m.Scan != 7 || !m.GeneratedAt.Equal(start) {
		t.Errorf("header %d %d %v", m.Version, m.Scan, m.GeneratedAt)
	}
	if len(m.Pipelines) != 2 || m.Pipelines[0] != "alloy" || m.Pipelines[1] != "scrape" {
		t.Errorf("pipelines %v", m.Pipelines)
	}
	var ids []string
	for _, s := range m.Signals {
		ids = append(ids, s.ID)
	}
	want := []string{"prom:metric:a_viewed", "prom:metric:b_alerted", "prom:metric:c_dormant"}
	if len(ids) != len(want) {
		t.Fatalf("signals %v, want %v (no candidate, unmapped or repo-less signal)", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("signal %d: %s, want %s", i, ids[i], want[i])
		}
	}
	b := m.Signals[1]
	if b.Pipeline != "scrape" || b.Owner != "shop" || len(b.Protectors) != 2 {
		t.Fatalf("b_alerted: %+v, want scrape, owner shop, two protectors (the sweep never protects)", b)
	}
	if b.Protectors[0].Consumer != "rule:alert:Hot" || b.Protectors[0].Evidence != "declared" {
		t.Errorf("protectors %+v: the declared alert comes first", b.Protectors)
	}
	if m.Signals[2].Dormant != "error by name" || len(m.Signals[2].Protectors) != 0 {
		t.Errorf("c_dormant: %+v", m.Signals[2])
	}
	if m.Acknowledged[0].Source != "agents" {
		t.Errorf("acknowledgements not sorted: %+v", m.Acknowledged)
	}
}

func TestManifestDir(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/acme/pipes":        "github.com_acme_pipes",
		"gitlab.example.com/a b/c.git": "gitlab.example.com_a_b_c.git",
	} {
		if got := ManifestDir(in); got != want {
			t.Errorf("ManifestDir(%q) = %q, want %q", in, got, want)
		}
	}
}
