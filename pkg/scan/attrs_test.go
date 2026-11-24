package scan

import (
	"testing"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/cost"
	"github.com/opsmastery/tvt/pkg/graph"
)

func TestStreamAttrs(t *testing.T) {
	sample := collect.StreamAttrs{Lines: 100, LineBytes: 100_000, Keys: []collect.AttrStat{
		{Key: "prompt", Lines: 50, ValueBytes: 80_000, OverCap: 50_000, MaxBytes: 3000, Distinct: 50},
		{Key: "level", Lines: 100, ValueBytes: 500, Distinct: 3},
	}}
	samples := map[string]collect.StreamAttrs{`{app="lines"}`: sample, `{app="metric"}`: sample, `{app="unread"}`: sample}
	vols := []graph.StreamVolume{{Selector: `{app="lines"}`, BytesPerDay: 1 << 30}, {Selector: `{app="metric"}`, BytesPerDay: 1 << 30}, {Selector: `{app="unread"}`, BytesPerDay: 1 << 30}}
	readers := func(sel string) []graph.Consumer {
		switch sel {
		case `{app="lines"}`:
			return []graph.Consumer{{ID: "panel:logs", Expr: `{app="lines"} | json`}}
		case `{app="metric"}`:
			return []graph.Consumer{{ID: "rule:errors", Expr: `sum by (level) (count_over_time({app="metric"} | json [5m]))`}}
		}
		return nil
	}
	card := &cost.PriceCard{}
	got := streamAttrs(samples, vols, card, readers)
	by := map[string]graph.StreamAttr{}
	for _, a := range got {
		by[a.Stream+" "+a.Key] = a
	}
	for k, want := range map[string]string{
		`{app="lines"} prompt`: graph.AttrReadLines, `{app="lines"} level`: graph.AttrReadLines,
		`{app="metric"} level`: graph.AttrReadNamed, `{app="metric"} prompt`: "",
		`{app="unread"} prompt`: "",
	} {
		if by[k].Read != want {
			t.Errorf("%s: read %q, want %q", k, by[k].Read, want)
		}
	}
	p := by[`{app="metric"} prompt`]
	day := float64(1 << 30)
	if p.BytesPerDay != int64(0.8*day) || p.CapBytesPerDay != int64(0.5*day) {
		t.Errorf("prompt sizes %+v", p)
	}
	if got[0].BytesPerDay < got[len(got)-1].BytesPerDay {
		t.Error("not sorted by bytes")
	}
}
