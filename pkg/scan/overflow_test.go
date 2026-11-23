package scan

import (
	"reflect"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/graph"
)

func TestOverflowFindings(t *testing.T) {
	in := Inputs{MetricsSource: "prom", OverflowRead: true, Overflow: []collect.Overflow{
		{Metric: "calls_total", Series: 1, Kept: []string{"job"}, Labels: []string{"customer_id", "job", "span_name"}},
		{Metric: "dur_bucket", Series: 7, Kept: []string{"job", "le"}, Labels: []string{"customer_id", "job", "le"}},
	}}
	lbl := func(use graph.LabelUse, name string) graph.LabelRef { return graph.LabelRef{Use: use, Name: name} }
	refs := []graph.Reference{
		// Filters on a capped label: warned.
		{SignalID: "prom:metric:calls_total", ConsumerID: "qlog:prom:a", Labels: []graph.LabelRef{lbl(graph.LabelMatch, "customer_id"), lbl(graph.LabelMatch, "job")}},
		// Groups by one, through the label signal: warned, that label only.
		{SignalID: "prom:label:calls_total:span_name", ConsumerID: "panel:1:d:2:0", Labels: []graph.LabelRef{lbl(graph.LabelGroup, "span_name"), lbl(graph.LabelGroup, "customer_id")}},
		// Uses a kept label only: not warned.
		{SignalID: "prom:metric:calls_total", ConsumerID: "rule:prom:x", Labels: []graph.LabelRef{lbl(graph.LabelMatch, "job")}},
		// A sweep filtering on it: not a reader.
		{SignalID: "prom:metric:calls_total", ConsumerID: "qlog:prom:sweep", Sweep: true, Labels: []graph.LabelRef{lbl(graph.LabelMatch, "customer_id")}},
		// le is never capped.
		{SignalID: "prom:metric:dur_bucket", ConsumerID: "panel:1:d:3:0", Labels: []graph.LabelRef{lbl(graph.LabelGroup, "le")}},
		// Another metric's capped-named label: not this one's reader.
		{SignalID: "prom:metric:other", ConsumerID: "qlog:prom:b", Labels: []graph.LabelRef{lbl(graph.LabelMatch, "customer_id")}},
	}
	got, src := overflowFindings(in, refs)
	want := []graph.OverflowFinding{
		{Signal: "prom:metric:calls_total", Series: 1, Capped: []string{"customer_id", "span_name"},
			Readers: []string{"panel:1:d:2:0\tspan_name", "qlog:prom:a\tcustomer_id"}},
		{Signal: "prom:metric:dur_bucket", Series: 7, Capped: []string{"customer_id"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if src.Source != SourceOverflow || !src.Configured || src.Coverage != 1 || strings.Count(src.Gap, "\n") != 1 ||
		!strings.Contains(src.Gap, "calls_total: 1 overflow series") || !strings.Contains(src.Gap, "2 reader(s)") {
		t.Errorf("source %+v", src)
	}

	// Not looked for: no source. A failed read: a gap, no findings.
	if f, s := overflowFindings(Inputs{}, refs); f != nil || s.Configured {
		t.Errorf("not read: %v %+v", f, s)
	}
	if f, s := overflowFindings(Inputs{OverflowErr: "prometheus: 503"}, refs); f != nil || !s.Configured || s.Coverage != 0 || !strings.Contains(s.Gap, "503") {
		t.Errorf("failed read: %v %+v", f, s)
	}
	// Looked for, none: coverage 1, no gap.
	if f, s := overflowFindings(Inputs{OverflowRead: true}, refs); f != nil || s.Coverage != 1 || s.Gap != "" {
		t.Errorf("none: %v %+v", f, s)
	}
}
