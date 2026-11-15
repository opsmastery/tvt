package scan

import (
	"slices"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func streamSignal(sel string) graph.Signal {
	return graph.Signal{ID: "loki:stream:" + sel, Kind: graph.KindStream, Name: sel, Source: "loki"}
}

// Overlap from the streams the volume API lists, and from the matchers
// when a selector selects none of them (or there are no volumes).
func TestStreamOverlaps(t *testing.T) {
	const (
		panel   = `{app="shop", level=~"warn|error"}`
		oncall  = `{app="shop", component="api", level=~"warn|error"}`
		debug   = `{app="shop", component="api", level="debug"}`
		billing = `{app="billing"}`
		gone    = `{app="shop", component="worker", level="warn"}` // selects no stream today
		notShop = `{app!="shop"}`
	)
	vols := []collect.StreamVolume{
		{Labels: map[string]string{"app": "shop", "component": "api", "level": "warn"}},
		{Labels: map[string]string{"app": "shop", "component": "api", "level": "error"}},
		{Labels: map[string]string{"app": "shop", "component": "api", "level": "debug"}},
		{Labels: map[string]string{"app": "billing", "level": "info"}},
	}
	var signals []graph.Signal
	for _, sel := range []string{panel, oncall, debug, billing, gone, notShop} {
		signals = append(signals, streamSignal(sel))
	}
	signals = append(signals, graph.Signal{ID: "prom:metric:up", Kind: graph.KindMetric, Name: "up"})
	got := StreamOverlaps(signals, streamLabelSets(vols))
	id := func(sel string) string { return "loki:stream:" + sel }
	want := map[string][]string{
		id(panel):   {id(oncall), id(gone)}, // the warn and error streams; gone by its matchers
		id(oncall):  {id(panel)},            // gone wants component="worker", oncall "api"
		id(debug):   nil,                    // level="debug" is outside both warn|error selectors
		id(billing): {id(notShop)},          // the billing stream
		id(gone):    {id(panel)},
		id(notShop): {id(billing)},
	}
	for sel, w := range want {
		if g := got[sel]; !slices.Equal(g, w) {
			t.Errorf("%s overlaps %v, want %v", sel, g, w)
		}
	}
	if _, ok := got["prom:metric:up"]; ok {
		t.Error("a metric got stream overlaps")
	}

	// Without volumes, the matchers decide everything.
	got = StreamOverlaps(signals, nil)
	if g := got[id(oncall)]; !slices.Equal(g, []string{id(panel)}) {
		t.Errorf("no volumes: %s overlaps %v", oncall, g)
	}
	if g := got[id(debug)]; len(g) != 0 {
		t.Errorf("no volumes: %s overlaps %v", debug, g)
	}

	// A selector tvt can't read overlaps every stream signal.
	all := append(slices.Clone(signals[:2]), streamSignal(AllStreams), streamSignal(`{app=~"("}`))
	got = StreamOverlaps(all, streamLabelSets(vols))
	for _, sel := range []string{panel, oncall} {
		if g := got[id(sel)]; !slices.Contains(g, id(AllStreams)) || !slices.Contains(g, id(`{app=~"("}`)) {
			t.Errorf("%s overlaps %v: want the unreadable selectors", sel, g)
		}
	}
}

// The S5 debug-pass bug: a viewed panel reading the warn and error streams
// through one selector protects an ad hoc query's narrower selector over
// the same streams, so it is never rerouted; a selector over other
// streams stays a candidate, and an overlapping sweep keeps nothing.
func TestClassifySharesOverlappingReaders(t *testing.T) {
	until := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	since := until.AddDate(0, 0, -30)
	const (
		panel  = `{app="shop", level=~"warn|error"}`
		oncall = `{app="shop", component="api", level=~"warn|error"}`
		debug  = `{app="shop", component="api", level="debug"}`
		swept  = `{app="cart"}`
		cart   = `{app="cart", level="info"}`
	)
	signals := []graph.Signal{streamSignal(panel), streamSignal(oncall), streamSignal(debug), streamSignal(swept), streamSignal(cart)}
	consumers := []graph.Consumer{
		{ID: "panel", Kind: graph.ConsumerDashboardPanel, LastViewed: until.Add(-time.Hour)},
		{ID: "q", Kind: graph.ConsumerAdhocQuery},
		{ID: "agent", Kind: graph.ConsumerAgentQuery},
	}
	refs := []graph.Reference{
		{SignalID: "loki:stream:" + panel, ConsumerID: "panel", Evidence: graph.EvidenceObserved},
		{SignalID: "loki:stream:" + oncall, ConsumerID: "q", Evidence: graph.EvidenceObserved},
		{SignalID: "loki:stream:" + swept, ConsumerID: "agent", Evidence: graph.EvidenceObserved, Sweep: true},
	}
	vols := []collect.StreamVolume{
		{Labels: map[string]string{"app": "shop", "component": "api", "level": "warn"}},
		{Labels: map[string]string{"app": "shop", "component": "api", "level": "debug"}},
		{Labels: map[string]string{"app": "cart", "level": "info"}},
	}
	if err := classify(signals, consumers, refs, StreamOverlaps(signals, streamLabelSets(vols)), nil, nil, nil, since, until); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{panel: "keep_high", oncall: "keep_high", debug: "candidate", swept: "candidate", cart: "candidate"}
	for _, s := range signals {
		if s.Class != want[s.Name] {
			t.Errorf("%s: class %s, want %s", s.Name, s.Class, want[s.Name])
		}
	}
}
