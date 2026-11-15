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
	got, err := lineFindings(patterns, vols, owners, &card)
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
	if got, _ := lineFindings(small, vols, nil, nil); len(got) != 0 {
		t.Errorf("a 1%% repeated line is a finding: %+v", got)
	}
	if got, _ := lineFindings(map[string][]collect.LinePattern{warn: {repeated(1000)}}, vols, nil, nil); len(got) != 1 || got[0].MonthlyCost != 0 || got[0].Share != 1 {
		t.Errorf("without a card: %+v", got)
	}
	if got, err := lineFindings(nil, vols, nil, nil); err != nil || len(got) != 0 {
		t.Errorf("no patterns: %+v %v", got, err)
	}
}
