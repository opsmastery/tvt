package scan

import (
	"math"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func TestLineFindingsRepeated(t *testing.T) {
	const (
		warn   = `{app="shop", component="api", level="warn"}`
		info   = `{app="shop", component="api", level="info"}`
		errs   = `{app="shop", component="api", level="error"}`
		paying = `{app="payments", level="info"}`
		burst  = `msg="deprecated config key ignored" key=cache.ttl`
	)
	repeated := func(lines int64) collect.LinePattern {
		return collect.LinePattern{Pattern: burst, Lines: lines, Window: 90 * time.Second}
	}
	templated := func(p string, lines int64) collect.LinePattern {
		return collect.LinePattern{Pattern: p, Lines: lines, Window: 3 * time.Hour}
	}
	patterns := map[string][]collect.LinePattern{
		// The burst is about half the warn stream's estimated bytes.
		warn: {repeated(1000), templated("route=<_> status=404 user=<_>", 1300)},
		// A templated pattern, however big, is not a repeated line.
		info: {templated("route=/healthz status=200 replica=<_>", 9000), templated("route=<_> status=200", 100)},
		// An error stream's repeated line is a symptom, never a saving.
		errs: {repeated(5000)},
		// So is a tier 1 owner's.
		paying: {repeated(5000)},
	}
	vols := []graph.StreamVolume{{Selector: warn, BytesPerDay: 2e9}, {Selector: info, BytesPerDay: 5e9}, {Selector: errs, BytesPerDay: 1e9}, {Selector: paying, BytesPerDay: 1e9}}
	one := 1
	owners := []config.Owner{{Name: "payments", Tier: &one, Streams: []string{`{app="payments"}`}}}
	card, err := cost.Load("grafana-cloud")
	if err != nil {
		t.Fatal(err)
	}
	got, err := lineFindings(patterns, vols, owners, &card, noReaders)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("findings %+v, want only the warn stream's burst", got)
	}
	f := got[0]
	bl := int64(len(burst))
	tl := int64(len("route=<_> status=404 user=<_>") - 6 + 2*variableBytes)
	share := float64(1000*bl) / float64(1000*bl+1300*tl)
	if f.Stream != warn || f.Pattern != burst || f.Kind != graph.LineRepeated || f.Lines != 1000 || math.Abs(f.Share-share) > 1e-12 {
		t.Errorf("finding %+v, want share %v", f, share)
	}
	if want := int64(share * 2e9); f.BytesPerDay != want || f.MonthlyCost != card.LogMonthly(want) || f.MonthlyCost <= 0 {
		t.Errorf("bytes/day %d monthly %v, want %d at the card's log price", f.BytesPerDay, f.MonthlyCost, want)
	}
	if r := f.PerSecond(); math.Abs(r-1000.0/90) > 1e-9 {
		t.Errorf("rate %v", r)
	}

	// Below the share: nothing. No card: no cost. No patterns: nothing.
	small := map[string][]collect.LinePattern{warn: {repeated(10), templated("route=<_> status=404 user=<_>", 1300)}}
	if got, _ := lineFindings(small, vols, nil, nil, noReaders); len(got) != 0 {
		t.Errorf("a 1%% repeated line is a finding: %+v", got)
	}
	if got, _ := lineFindings(map[string][]collect.LinePattern{warn: {repeated(1000)}}, vols, nil, nil, noReaders); len(got) != 1 || got[0].MonthlyCost != 0 || got[0].Share != 1 {
		t.Errorf("without a card: %+v", got)
	}
	if got, err := lineFindings(nil, vols, nil, nil, noReaders); err != nil || len(got) != 0 {
		t.Errorf("no patterns: %+v %v", got, err)
	}
}

func noReaders(string) []graph.Consumer { return nil }

func TestMergePatterns(t *testing.T) {
	ps := []collect.LinePattern{
		{Pattern: "route=/healthz status=200 probe=kubelet-liveness replica=<_>", Lines: 100, Window: time.Hour},
		{Pattern: "route=/healthz status=200 probe=kubelet-readiness replica=<_>", Lines: 100, Window: 2 * time.Hour},
		{Pattern: "route=/healthz status=200 probe=alb replica=<_>", Lines: 100, Window: time.Hour},
		{Pattern: "route=/ status=200 user=<_>", Lines: 50},
		{Pattern: "route=/cart status=200 user=<_>", Lines: 50},
		{Pattern: "starting up", Lines: 1},
		{Pattern: "shutting down", Lines: 1},
	}
	got := mergePatterns(ps)
	want := []string{"route=/healthz status=200 probe=<_> replica=<_>", "route=<_> status=200 user=<_>", "shutting down", "starting up"}
	if len(got) != 4 {
		t.Fatalf("merged %+v", got)
	}
	for i, w := range want {
		if got[i].Pattern != w {
			t.Errorf("merged[%d] = %q, want %q", i, got[i].Pattern, w)
		}
	}
	if got[0].Lines != 300 || got[0].Window != 2*time.Hour || got[0].bytes != ps[0].Lines*patternBytes(ps[0].Pattern)+ps[1].Lines*patternBytes(ps[1].Pattern)+ps[2].Lines*patternBytes(ps[2].Pattern) {
		t.Errorf("healthz merged %+v", got[0])
	}
	// "starting up" and "shutting down" differ in both tokens: not merged
	// (checked by want above).
}

func TestFilterSelects(t *testing.T) {
	const p = "method=GET route=/healthz status=200 probe=<_>"
	for expr, want := range map[string]bool{
		`{app="shop"}`:                                     false, // shows every line, picks none out
		`{app="shop"} |= "route=/healthz"`:                 true,
		"{app=\"shop\"} |= `status=200`":                   true,
		`{app="shop"} |= "route=/healthz" != "status=200"`: false, // the on-call query: failures only
		`{app="shop"} |= "route=/checkout"`:                false,
		`{app="shop", level!="debug"} |= "healthz"`:        true, // a label matcher isn't a line filter
		`{app!~"x"}`: false,
		`sum by (level) (rate({app="shop"} |= "probe=" [5m]))`:  true,
		`{app="shop"} |~ "route=/healthz" !~ "status=200"`:      false,
		`{app="shop"} | json | line_format "{{.msg}}" |= "GET"`: true,
	} {
		if got := filterSelects(expr, p); got != want {
			t.Errorf("filterSelects(%s) = %v, want %v", expr, got, want)
		}
	}
}

func TestLineHandoff(t *testing.T) {
	const (
		info  = `{app="shop", component="api", level="info"}`
		audit = `{app="shop", component="audit", level="info"}`
		debug = `{app="shop", component="api", level="debug"}`
	)
	healthz := []collect.LinePattern{
		{Pattern: "route=/healthz status=200 probe=a replica=<_>", Lines: 400, Window: 3 * time.Hour},
		{Pattern: "route=/healthz status=200 probe=b replica=<_>", Lines: 400, Window: 3 * time.Hour},
		{Pattern: "route=<_> status=200 user=<_>", Lines: 300, Window: 3 * time.Hour},
	}
	patterns := map[string][]collect.LinePattern{info: healthz, audit: healthz, debug: healthz}
	vols := []graph.StreamVolume{{Selector: info, BytesPerDay: 1e9}, {Selector: audit, BytesPerDay: 1e9}, {Selector: debug, BytesPerDay: 1e9}}
	panel := graph.Consumer{ID: "panel:1:shop:6:0", Kind: graph.ConsumerDashboardPanel, Expr: `{app="shop", level=~"info|warn"}`}
	alert := graph.Consumer{ID: "rule:loki:x/g/0", Kind: graph.ConsumerAlertRule, Expr: `count_over_time({component="audit"} |= "denied" [5m])`}
	readers := func(stream string) []graph.Consumer {
		switch stream {
		case info:
			return []graph.Consumer{panel}
		case audit:
			return []graph.Consumer{panel, alert}
		}
		return nil // debug: unread, a whole-stream reroute
	}
	got, err := lineFindings(patterns, vols, nil, nil, readers)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("findings %+v, want one hand-off in the info stream", got)
	}
	f := got[0]
	if f.Stream != info || f.Kind != graph.LineHandoff || f.Pattern != "route=/healthz status=200 probe=<_> replica=<_>" || f.Lines != 800 || f.Share < 0.7 || f.Share > 0.8 {
		t.Errorf("hand-off %+v", f)
	}

	// A reader whose line filter picks out the pattern reads it: no hand-off.
	picky := graph.Consumer{ID: "adhoc", Kind: graph.ConsumerAdhocQuery, Expr: `{app="shop"} |= "route=/healthz"`}
	got, _ = lineFindings(map[string][]collect.LinePattern{info: healthz}, vols, nil, nil, func(string) []graph.Consumer { return []graph.Consumer{panel, picky} })
	if len(got) != 0 {
		t.Errorf("hand-off of a filtered-for pattern: %+v", got)
	}
	// A dominant pattern under LineShare: none.
	// Five equal patterns, no two mergeable: 20% each.
	spread := []collect.LinePattern{{Pattern: "a=<_> x1", Lines: 10}, {Pattern: "b=<_> x2", Lines: 10}, {Pattern: "c=<_> x3", Lines: 10}, {Pattern: "d=<_> x4", Lines: 10}, {Pattern: "e=<_> x5", Lines: 10}}
	if got, _ := lineFindings(map[string][]collect.LinePattern{info: spread}, vols, nil, nil, readers); len(got) != 0 {
		t.Errorf("hand-off of a small pattern: %+v", got)
	}
}
