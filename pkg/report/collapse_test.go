package report

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
)

// TestCollapseRecs: a stream nothing reads whose identical lines repeat
// several times a minute gets a collapse, saving all but one line a
// window; a read stream, a record, and a line that repeats less than
// once a minute get none.
func TestCollapseRecs(t *testing.T) {
	const rec = `{service_name="reconciler"}`
	const slow = `{service_name="cron"}`
	const read = `{service_name="shop"}`
	in := Input{
		Scan: Scan{Currency: "USD"},
		LineFindings: []graph.LineFinding{
			// 2 lines a second for an hour: 120 a minute, 1 kept.
			{Stream: rec, Pattern: "reconciled 0 objects", Kind: graph.LineRepeated, Lines: 7200, Window: time.Hour, MonthlyCost: 1.2},
			{Stream: rec, Pattern: "lease renewed", Kind: graph.LineRepeated, Lines: 3600, Window: time.Hour, MonthlyCost: 0.6},
			{Stream: slow, Pattern: "tick", Kind: graph.LineRepeated, Lines: 30, Window: time.Hour, MonthlyCost: 0.1},
			{Stream: read, Pattern: "ok", Kind: graph.LineRepeated, Lines: 7200, Window: time.Hour, MonthlyCost: 1},
			{Stream: rec, Pattern: "GET <_>", Kind: graph.LineHandoff, Lines: 7200, Window: time.Hour, MonthlyCost: 5},
		},
		Signals: []graph.Signal{{ID: "q:shop", Kind: graph.KindStream, Name: read}},
	}
	refs := map[string][]graph.Reference{"q:shop": {{SignalID: "q:shop", ConsumerID: "panel:1"}}}
	gs := newGaps(nil, nil)
	got := collapseRecs(in, map[string]graph.Signal{}, refs, nil, gs, map[string]bool{})
	if len(got) != 1 {
		t.Fatalf("%d rows, want the reconciler's only: %+v", len(got), got)
	}
	r := got[0]
	want := 1.2*(1-1.0/120) + 0.6*(1-1.0/60)
	if r.ID != "collapse:loki:stream:"+rec || r.Action != ActionCollapse || math.Abs(r.Savings.Monthly-want) > 1e-9 || !strings.Contains(r.Claim, "2 lines") || r.EvidenceHash == "" {
		t.Errorf("row %+v, want saving %.4f", r, want)
	}
	// A record stream gets none.
	sigs := map[string]graph.Signal{"loki:stream:" + rec: {ID: "loki:stream:" + rec, Kind: graph.KindStream, Name: rec, Record: "listed"}}
	if got := collapseRecs(in, sigs, refs, nil, gs, map[string]bool{}); len(got) != 0 {
		t.Errorf("a record stream was collapsed: %+v", got)
	}
}
