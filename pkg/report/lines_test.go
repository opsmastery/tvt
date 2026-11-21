package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func lineFixture() Input {
	in := fixture()
	in.LineFindings = []graph.LineFinding{{
		Stream: `{app="shop", component="api", level="warn"}`, Pattern: `msg="deprecated config key ignored" key=cache.ttl`,
		Kind: graph.LineRepeated, Lines: 10000, Window: 90 * time.Second, Share: 0.55, BytesPerDay: 2e6, MonthlyCost: 0.5,
	}}
	return in
}

func TestLineFindingRows(t *testing.T) {
	r := Build(lineFixture(), 0)
	if len(r.LineFindings) != 1 {
		t.Fatalf("line findings %+v", r.LineFindings)
	}
	f := r.LineFindings[0]
	if f.Kind != graph.LineRepeated || f.Lines != 10000 || f.WindowSeconds != 90 || f.PerSecond != 10000.0/90 || f.Share != 0.55 || f.BytesPerDay != 2e6 {
		t.Errorf("row %+v", f)
	}
	if f.Savings != (Savings{Monthly: 0.5, Currency: "USD", Approximate: true}) || f.Handoff != ZeroreadsURL {
		t.Errorf("savings %+v, handoff %q", f.Savings, f.Handoff)
	}
	if !strings.Contains(f.Action, "10000 times in 1m") || !strings.Contains(f.Action, "zeroreads") || !strings.Contains(f.Action, "proposes collapsing them") {
		t.Errorf("action %q", f.Action)
	}
	if again := Build(lineFixture(), 0).LineFindings[0].ID; !strings.HasPrefix(f.ID, "line-") || again != f.ID {
		t.Errorf("ids %q, %q: want stable", f.ID, again)
	}
	// A line finding is not a recommendation; its repeated line in a
	// stream nothing reads adds one collapse (S10 item 4), and only that.
	plain := Build(fixture(), 0)
	collapses := 0
	for _, rec := range r.Recommendations {
		if rec.Action == ActionCollapse {
			collapses++
		}
	}
	want := plain.Totals
	want.Recommendations++
	got := r.Totals
	if collapses != 1 || len(r.Recommendations) != len(plain.Recommendations)+1 || got.StreamSavings <= want.StreamSavings {
		t.Errorf("want one collapse added: %d collapses, %+v vs %+v", collapses, got, plain.Totals)
	}
	got.StreamSavings = want.StreamSavings
	if got != want {
		t.Errorf("totals beyond the collapse changed: %+v vs %+v", r.Totals, want)
	}
	if len(plain.LineFindings) != 0 || plain.LineFindings == nil {
		t.Errorf("no findings: %#v, want an empty list", plain.LineFindings)
	}

	// Repricing reprices the findings at the new card's log price.
	card := cost.PriceCard{Version: 1, Name: "logs", Currency: "EUR", Logs: cost.LogPricing{PerGBIngested: 1}}
	out, err := Reprice(lineFixture(), card, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := Build(out, 0).LineFindings[0].Savings; got.Monthly != card.LogMonthly(2e6) || got.Currency != "EUR" {
		t.Errorf("repriced %+v, want %v EUR", got, card.LogMonthly(2e6))
	}

	// The HTML page shows them, escaped, with the zeroreads link.
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var page bytes.Buffer
	if err := RenderHTMLFromJSON(&page, b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Line-level findings", "repeated line", `msg=&#34;deprecated config key ignored&#34; key=cache.ttl`, ZeroreadsURL, `id="` + f.ID + `"`} {
		if !strings.Contains(page.String(), want) {
			t.Errorf("page has no %q", want)
		}
	}
}
