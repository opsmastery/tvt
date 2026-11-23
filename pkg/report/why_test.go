package report

import (
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
)

func TestResolve(t *testing.T) {
	sigs := []graph.Signal{
		{ID: "prom:metric:up", Kind: graph.KindMetric, Name: "up"},
		{ID: "mimir:metric:up", Kind: graph.KindMetric, Name: "up"},
		{ID: "prom:metric:a:b", Kind: graph.KindMetric, Name: "a:b"},
		{ID: "prom:label:a:b:pod", Kind: graph.KindLabel, Name: "pod", Parent: "a:b"},
		{ID: `loki:stream:{app="shop"}`, Kind: graph.KindStream, Name: `{app="shop"}`},
	}
	for arg, want := range map[string]string{
		"prom:metric:up": "prom:metric:up",
		"a:b":            "prom:metric:a:b",
		"a:b{pod}":       "prom:label:a:b:pod",
		`{app="shop"}`:   `loki:stream:{app="shop"}`,
	} {
		if got, err := Resolve(sigs, arg); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", arg, got, err, want)
		}
	}
	if _, err := Resolve(sigs, "up"); err == nil || !strings.Contains(err.Error(), "mimir:metric:up, prom:metric:up") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := Resolve(sigs, "pod"); err == nil {
		t.Error("a bare label name resolved")
	}
}

func TestExplain(t *testing.T) {
	in := fixture()
	first := at.Add(-10 * 24 * time.Hour)
	in.Consumers = append(in.Consumers,
		graph.Consumer{ID: "rule:prom:a/g/rec", Kind: graph.ConsumerRecordingRule, Name: "job:queried:rate"})
	in.References = append(in.References,
		graph.Reference{SignalID: "prom:metric:queried", ConsumerID: "rule:prom:a/g/HighErrors", Via: "rule:prom:a/g/rec", Evidence: graph.EvidenceDeclared, FirstSeen: first,
			Labels: []graph.LabelRef{{Name: "job", Use: graph.LabelGroup}}},
		graph.Reference{SignalID: "prom:metric:queried", ConsumerID: "qlog:prometheus:agent:sweep:1", Evidence: graph.EvidenceObserved, Sweep: true},
		graph.Reference{SignalID: "prom:metric:queried", ConsumerID: "panel:1:d:3:0", Evidence: graph.EvidenceObserved, Assumed: true, Reasons: []string{"variable $metric"}})
	w, err := Explain(in, "prom:metric:queried")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range w.Readers {
		order = append(order, r.Consumer)
	}
	// Declared first, then exact before inferred, sweeps last.
	if want := "rule:prom:a/g/HighErrors qlog:prometheus:abc panel:1:d:3:0 qlog:prometheus:agent:sweep:1"; strings.Join(order, " ") != want {
		t.Errorf("order = %v", order)
	}
	rule := w.Readers[0]
	if !rule.FirstSeen.Equal(first) || rule.Labels[0] != "group:job" || len(rule.Inferred) != 1 || !strings.Contains(rule.Inferred[0], "job:queried:rate") {
		t.Errorf("rule reader = %+v", rule)
	}
	if w.Readers[2].Exact || w.Readers[2].Source != SourceAssumed || w.Readers[2].Inferred[0] != "variable $metric" {
		t.Errorf("assumed reader = %+v", w.Readers[2])
	}
	if w.LastRead == nil || w.LastRead.Actor != "alice" || w.Monthly != 80 || w.Score == nil || w.Row != "drop:prom:metric:queried" {
		t.Errorf("why = %+v", w)
	}
	var assumedGap bool
	for _, g := range w.Gaps {
		assumedGap = assumedGap || g.Kind == GapAssumed
	}
	if !assumedGap {
		t.Errorf("no assumed gap in %+v", w.Gaps)
	}

	// A dormant signal has its confirm row; an unscored one no score.
	in.Signals = append(in.Signals, graph.Signal{ID: "prom:metric:load", Kind: graph.KindMetric, Name: "load", Source: "prom", Class: "dormant", Dormant: "mentioned in runbook"})
	if w, err = Explain(in, "prom:metric:load"); err != nil || w.Row != "confirm:prom:metric:load" || w.Score != nil || w.Dormant == "" || len(w.Readers) != 0 {
		t.Errorf("dormant = %+v, %v", w, err)
	}
	if _, err := Explain(in, "nope"); err == nil {
		t.Error("unknown id explained")
	}
}

func TestWhyCommandInHTML(t *testing.T) {
	if got := WhyCommand(`m{a="it's"}`); got != `tvt why 'm{a="it'\''s"}'` {
		t.Errorf("WhyCommand = %s", got)
	}
	var b strings.Builder
	if err := RenderHTML(&b, Build(fixture(), 0)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "tvt why &#39;prom:metric:unread&#39;") {
		t.Error("HTML rows don't link to tvt why")
	}
}

// A label kept in an alert's result shows that alert, inferred.
func TestExplainResultLabels(t *testing.T) {
	in := fixture()
	in.References = append(in.References, graph.Reference{SignalID: "prom:metric:kept", ConsumerID: "rule:prom:a/g/HighErrors", Evidence: graph.EvidenceDeclared, ResultLabels: true})
	w, err := Explain(in, "prom:label:kept:pod")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Readers) != 1 || w.Readers[0].Consumer != "rule:prom:a/g/HighErrors" || len(w.Readers[0].Inferred) != 1 || !strings.Contains(w.Readers[0].Inferred[0], "result through kept") {
		t.Errorf("readers = %+v", w.Readers)
	}
}
