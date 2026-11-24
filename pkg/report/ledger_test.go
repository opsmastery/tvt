package report

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
)

// TestLedger: a merged drop is measured from the scan before its merge to
// the first scan after it; a merge with no scan after it is pending, one
// with no scan before it unmeasured; the bill's change is split into the
// savings, new signals and growth on signals already there; self-hosted,
// the freed disk becomes days of retention.
func TestLedger(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	merged := t0.Add(36 * time.Hour)
	cost := func(monthly float64, disk int64) graph.Cost {
		return graph.Cost{MonthlyCost: monthly, ActiveSeries: 10, DiskBytes: disk}
	}
	scans := map[int64]ScanCosts{
		1: {StartedAt: t0, Costs: map[string]graph.Cost{"prom:metric:a": cost(4, 400), "prom:metric:b": cost(10, 1000), "prom:label:b:x": cost(3, 0)}},
		2: {StartedAt: t0.Add(24 * time.Hour), Costs: map[string]graph.Cost{"prom:metric:a": cost(4, 400), "prom:metric:b": cost(10, 1000)}},
		3: {StartedAt: t0.Add(48 * time.Hour), Retention: 10 * 24 * time.Hour,
			Costs: map[string]graph.Cost{"prom:metric:b": cost(12, 1100), "prom:metric:new": cost(1, 100)}},
	}
	merges := []graph.Merge{
		{Repo: "r", ProposalScan: 1, Recommendation: "drop:prom:metric:a", SignalID: "prom:metric:a", MergedAt: merged, Expected: 4},
		{Repo: "r", ProposalScan: 9, Recommendation: "drop:prom:metric:late", SignalID: "prom:metric:late", MergedAt: t0.Add(72 * time.Hour), Expected: 2},
		{Repo: "r", ProposalScan: 9, Recommendation: "drop:prom:metric:early", SignalID: "prom:metric:early", MergedAt: t0.Add(-time.Hour)},
	}
	l := buildLedger(merges, scans, 3, "USD")
	if l == nil || len(l.Rows) != 3 {
		t.Fatalf("ledger %+v", l)
	}
	a := l.Rows[0]
	if a.Status != LedgerMeasured || a.BeforeScan != 1 || a.AfterScan != 3 || a.Saved != 4 || a.Action != "drop" || a.After.Monthly != 0 {
		t.Errorf("a %+v", a)
	}
	if l.Rows[1].Status != LedgerPending || l.Rows[1].BeforeScan != 3 || l.Pending != 1 {
		t.Errorf("late %+v", l.Rows[1])
	}
	if l.Rows[2].Status != LedgerUnmeasured {
		t.Errorf("early %+v", l.Rows[2])
	}
	if l.Saved != 4 || l.Expected != 4 {
		t.Errorf("saved %v, expected %v", l.Saved, l.Expected)
	}
	// Baseline scan 1: 14 a month (labels left out), now 13; the drop saved
	// 4, so the bill grew 3: 1 from the new metric, 2 on b.
	g := l.Growth
	if g == nil || g.BaselineScan != 1 || g.Baseline != 14 || g.Now != 13 || g.Total != -1 || g.Growth != 3 || g.NewSignals != 1 || g.Existing != 2 {
		t.Errorf("growth %+v", g)
	}
	// 400 bytes freed; 1200 bytes hold 10 days, so 120 a day: 3.3 days.
	if l.DiskFreedBytes != 400 || math.Abs(l.RetentionDaysGained-400.0/120) > 1e-9 {
		t.Errorf("disk %d, days %v", l.DiskFreedBytes, l.RetentionDaysGained)
	}
	if buildLedger(nil, scans, 3, "USD") != nil {
		t.Error("a ledger with nothing merged")
	}
	// The HTML page shows it.
	r := Report{Version: Version, Scan: Scan{ID: 3, Currency: "USD"}, Ledger: l, Recommendations: []Recommendation{}, Confirm: []Confirm{}, Sources: []Source{}, Gaps: []Gap{}}
	var page bytes.Buffer
	if err := RenderHTML(&page, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h2>Savings ledger</h2>", "drop:prom:metric:a", "4.00 USD a month", "pending", "grew 3.00 USD"} {
		if !strings.Contains(page.String(), want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// TestCollectorRates: the Collector's sent counters become rates over the
// last two scans before the first merge and the last two after the last;
// a counter that went down (a restart) has no rate.
func TestCollectorRates(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	times := map[int64]time.Time{1: t0, 2: t0.Add(time.Hour), 3: t0.Add(3 * time.Hour), 4: t0.Add(4 * time.Hour)}
	s := func(exp, sig string, v float64) graph.CollectorSent {
		return graph.CollectorSent{Pipeline: "agent", Exporter: exp, Signal: sig, Sent: v}
	}
	sent := map[int64][]graph.CollectorSent{
		1: {s("otlp_http/loki", "logs", 0), s("rw", "metrics", 100)},
		2: {s("otlp_http/loki", "logs", 3600), s("rw", "metrics", 200)},
		// The merge at 2h, then a restart: counters start over.
		3: {s("otlp_http/loki", "logs", 100), s("file/cold", "logs", 50), s("rw", "metrics", 10)},
		4: {s("otlp_http/loki", "logs", 460), s("file/cold", "logs", 3650), s("rw", "metrics", 5)},
	}
	merges := []graph.Merge{{MergedAt: t0.Add(2 * time.Hour)}}
	got := collectorRates(merges, sent, times)
	byKey := map[string]CollectorRate{}
	for _, r := range got {
		byKey[r.Exporter] = r
	}
	if r := byKey["otlp_http/loki"]; !r.BeforeKnown || r.Before != 1 || !r.AfterKnown || r.After != 0.1 {
		t.Errorf("loki %+v: want 1/s before, 0.1/s after", r)
	}
	if r := byKey["file/cold"]; r.BeforeKnown || !r.AfterKnown || r.After != 1 {
		t.Errorf("cold %+v", r)
	}
	if r := byKey["rw"]; !r.BeforeKnown || r.AfterKnown {
		t.Errorf("rw %+v: the counter went down after, so no rate", r)
	}
	if collectorRates(nil, sent, times) != nil {
		t.Error("rates with nothing merged")
	}
}

// TestLedgerSettling: a scan 40 minutes after the merge still sees the old
// volume (the S11 rehearsal); it shows as settling and isn't counted, and
// the first scan past the window measures the change.
func TestLedgerSettling(t *testing.T) {
	merged := time.Date(2026, 11, 21, 22, 44, 0, 0, time.UTC)
	cost := func(m float64) map[string]graph.Cost {
		return map[string]graph.Cost{"prom:metric:a": {MonthlyCost: m}, "loki:stream:{a=\"b\"}": {MonthlyCost: m}}
	}
	scans := map[int64]ScanCosts{
		3: {StartedAt: merged.Add(-7 * time.Minute), Costs: cost(10)},
		4: {StartedAt: merged.Add(40 * time.Minute), Costs: cost(10)},
	}
	merges := []graph.Merge{
		{Repo: "r", ProposalScan: 3, Recommendation: "drop:prom:metric:a", SignalID: "prom:metric:a", MergedAt: merged, Expected: 10},
		{Repo: "r", ProposalScan: 3, Recommendation: "reroute:loki:stream:{a=\"b\"}", SignalID: "loki:stream:{a=\"b\"}", MergedAt: merged, Expected: 10},
	}
	l := buildLedger(merges, scans, 4, "USD")
	if l.Settling != 2 || l.Saved != 0 || l.Expected != 0 || l.Rows[0].Status != LedgerSettling || !l.Rows[0].SettlesAt.Equal(merged.Add(3*time.Hour)) {
		t.Fatalf("40 minutes after: %+v", l)
	}
	scans[5] = ScanCosts{StartedAt: merged.Add(3*time.Hour + time.Minute), Costs: cost(0)}
	l = buildLedger(merges, scans, 5, "USD")
	if l.Rows[0].Status != LedgerMeasured || l.Rows[0].AfterScan != 5 || l.Rows[0].Saved != 10 {
		t.Errorf("metric after 3h: %+v", l.Rows[0])
	}
	if l.Rows[1].Status != LedgerSettling || l.Settling != 1 {
		t.Errorf("stream after 3h is still settling: %+v", l.Rows[1])
	}
	scans[6] = ScanCosts{StartedAt: merged.Add(25 * time.Hour), Costs: cost(0)}
	if l = buildLedger(merges, scans, 6, "USD"); l.Rows[1].Status != LedgerMeasured || l.Saved != 20 || l.Settling != 0 {
		t.Errorf("after 24h: %+v", l)
	}
}
