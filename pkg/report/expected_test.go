package report

import (
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// TestExpected is S9 item 5: each kind of recommendation says what should
// be true after the merge, in words and in numbers verify can check.
func TestExpected(t *testing.T) {
	drop := expected(Recommendation{Action: ActionDrop, Signal: SignalRef{Name: "node_boot_time_seconds"}, Savings: Savings{Monthly: 1.5, Currency: "USD"}},
		graph.Cost{ActiveSeries: 3}, 0)
	if drop.SeriesAfter != 0 || drop.WithinSec != 3600 || len(drop.Statements) != 3 || !strings.Contains(drop.Statements[0], "node_boot_time_seconds returns no new samples within an hour") || !strings.Contains(drop.Statements[2], "1.50 USD") {
		t.Errorf("drop: %+v", drop)
	}
	agg := expected(Recommendation{Action: ActionLabelRemoval, Signal: SignalRef{Name: "user_id", Parent: "shop_cart_operations_total"}, Removal: "aggregate", Merges: 5900},
		graph.Cost{ActiveSeries: 6000}, 0)
	if agg.SeriesAfter != 100 || !strings.Contains(agg.Statements[1], "about 100 active series after, from 6000") {
		t.Errorf("aggregate: %+v", agg)
	}
	ld := expected(Recommendation{Action: ActionLabelRemoval, Signal: SignalRef{Name: "op_kind", Parent: "shop_cart_operations_total"}, Removal: "labeldrop"}, graph.Cost{ActiveSeries: 3500}, 0)
	if ld.SeriesAfter != 3500 {
		t.Errorf("labeldrop: %+v", ld)
	}
	route := expected(Recommendation{Action: ActionReroute, Signal: SignalRef{Name: `{app="shop", level="debug"}`}}, graph.Cost{BytesPerDay: 400 << 20}, 1000<<20)
	if route.HotBytesPerDayFall != 400<<20 || route.SharePct != 40 || !strings.Contains(route.Statements[1], "400.0 MiB a day (about 40% of all stream bytes)") || route.SeriesAfter != -1 {
		t.Errorf("reroute: %+v", route)
	}
}

// TestDigests is S9 item 6: one digest per owner with their open rows
// (capped), their reopened decisions and their questions; unowned rows in
// their own digest.
func TestDigests(t *testing.T) {
	r := Report{Scan: Scan{ID: 9, StartedAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), Currency: "USD"}}
	for i, o := range []string{"shop", "shop", "shop", "", "payments"} {
		r.Recommendations = append(r.Recommendations, Recommendation{ID: "drop:prom:metric:m" + string(rune('a'+i)), Owner: o, Status: StatusOpen,
			SavingsPerMinute: float64(10 - i), Savings: Savings{Monthly: 1}, EvidenceHash: "h", Expected: Expected{Statements: []string{"no samples"}}})
	}
	r.Recommendations = append(r.Recommendations, Recommendation{ID: "drop:prom:metric:done", Owner: "shop", Status: StatusConfirmed})
	r.Confirm = []Confirm{{ID: "confirm:x", Owner: "payments", Question: "payments: will you need x?"}}
	r.Decisions = []DecisionState{{Decision: Decision{Signal: "prom:metric:y", Action: DecisionDismiss, OwnerOfRecord: "shop"}, State: DecisionExpired, Why: "its review date 2026-11-01 has passed"},
		{Decision: Decision{Signal: "prom:metric:z", Action: DecisionConfirm, OwnerOfRecord: "shop"}, State: DecisionCurrent}}
	ds := Digests(r, 2)
	by := map[string]Digest{}
	for _, d := range ds {
		by[d.Owner] = d
	}
	if len(ds) != 3 || by["shop"].File != "shop.md" || by[Unowned].File != "unowned.md" {
		t.Fatalf("digests: %+v", ds)
	}
	shop := by["shop"].Markdown
	for _, want := range []string{"# tvt weekly digest: shop", "Recommendations waiting for you (3, 3.00 USD a month)", "`drop:prom:metric:ma`", "`drop:prom:metric:mb`",
		"1 more wait behind these (tvt.yaml queue.per_owner is 2)", "Decisions to look at again (1)", "your dismiss is expired: its review date 2026-11-01 has passed"} {
		if !strings.Contains(shop, want) {
			t.Errorf("shop digest lacks %q:\n%s", want, shop)
		}
	}
	if strings.Contains(shop, "metric:mc`") || strings.Contains(shop, "done") || strings.Contains(shop, "prom:metric:z") {
		t.Errorf("shop digest lists what it shouldn't:\n%s", shop)
	}
	if !strings.Contains(by["payments"].Markdown, "payments: will you need x?") || !strings.Contains(by[Unowned].Markdown, "No owner source") {
		t.Errorf("payments or unowned:\n%s\n%s", by["payments"].Markdown, by[Unowned].Markdown)
	}
}
