package report

import (
	"sort"
	"strings"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
)

// ScanCosts is one stored scan's costs, for the ledger's before and after
// (S11 item 1).
type ScanCosts struct {
	StartedAt time.Time
	Costs     map[string]graph.Cost
	// Retention is the scan's self-hosted retention, 0 for a vendor card.
	Retention time.Duration
}

// Ledger is what merged tvt changes saved (S11): each change measured from
// the scan before its merge to the first scan after it settled
// (LedgerSettle), apart from what the bill did meanwhile.
type Ledger struct {
	Rows []LedgerRow `json:"rows"`
	// Saved is the measured monthly saving of the measured rows, and
	// Expected what those rows claimed when proposed.
	Saved    float64 `json:"saved_monthly"`
	Expected float64 `json:"expected_monthly"`
	Currency string  `json:"currency,omitempty"`
	Pending  int     `json:"pending"`
	// Settling counts rows whose only scans after the merge came before
	// the backend's measurement caught up (LedgerSettle).
	Settling int `json:"settling,omitempty"`
	// Growth is the bill apart from the savings, from the earliest scan
	// before a merge to this scan.
	Growth *Growth `json:"growth,omitempty"`
	// DiskFreedBytes is the self-hosted disk the measured rows freed, and
	// RetentionDaysGained how many more days of history that disk holds at
	// this scan's rate (S11 item 3); 0 for a vendor card.
	DiskFreedBytes      int64   `json:"disk_freed_bytes,omitempty"`
	RetentionDaysGained float64 `json:"retention_days_gained,omitempty"`
	// Collector is what each OTel Collector exporter sent per second
	// before the first merge and since the last one, from the Collector's
	// own counters (S11 item 4).
	Collector []CollectorRate `json:"collector,omitempty"`
}

// CollectorRate is one Collector exporter's send rate for one signal:
// Before over the last two scans before the first merge, After over the
// last two scans after the last merge. A pair with a counter reset (a
// restart) between them has no rate: Known says which are.
type CollectorRate struct {
	Pipeline    string  `json:"pipeline"`
	Exporter    string  `json:"exporter"`
	Signal      string  `json:"signal"`
	Before      float64 `json:"before_per_second"`
	After       float64 `json:"after_per_second"`
	BeforeKnown bool    `json:"before_known"`
	AfterKnown  bool    `json:"after_known"`
}

// LedgerRow is one merged recommendation.
type LedgerRow struct {
	Repo           string    `json:"repo"`
	Recommendation string    `json:"recommendation"`
	Action         string    `json:"action"`
	Signal         string    `json:"signal"`
	MergedAt       time.Time `json:"merged_at"`
	// Status: measured (a scan before the merge and one after it has
	// settled), settling (scans after the merge, but none past SettlesAt
	// yet: the After figures are provisional and not counted), pending
	// (no scan after it yet) or unmeasured (no scan before it in this
	// database).
	Status     string      `json:"status"`
	SettlesAt  time.Time   `json:"settles_at,omitempty"`
	BeforeScan int64       `json:"before_scan,omitempty"`
	AfterScan  int64       `json:"after_scan,omitempty"`
	Before     LedgerPoint `json:"before"`
	After      LedgerPoint `json:"after"`
	Saved      float64     `json:"saved_monthly"`
	Expected   float64     `json:"expected_monthly"`
}

// LedgerPoint is a signal's size and price in one scan. A signal the scan
// didn't see (a dropped metric) is zero.
type LedgerPoint struct {
	Series      int64   `json:"series,omitempty"`
	BytesPerDay int64   `json:"bytes_per_day,omitempty"`
	Monthly     float64 `json:"monthly"`
	DiskBytes   int64   `json:"disk_bytes,omitempty"`
}

// Growth splits the bill's change apart from the savings: Total is this
// scan's cost minus the baseline scan's (metrics and streams; labels are
// shares of their metrics), Growth that change with the savings added
// back, NewSignals the cost of signals the baseline didn't have, and
// Existing the rest (more series or bytes on signals already there:
// replicas, a release).
type Growth struct {
	BaselineScan int64   `json:"baseline_scan"`
	Baseline     float64 `json:"baseline_monthly"`
	Now          float64 `json:"now_monthly"`
	Total        float64 `json:"total_change"`
	Growth       float64 `json:"growth"`
	NewSignals   float64 `json:"new_signals"`
	Existing     float64 `json:"existing_signals"`
}

const (
	LedgerMeasured   = "measured"
	LedgerPending    = "pending"
	LedgerUnmeasured = "unmeasured"
	LedgerSettling   = "settling"
)

// LedgerSettle is how long after a merge a scan still sees the old volume
// (found in the S11 rehearsal, Nov 21: 40 minutes after the merge, dropped
// metrics still counted every series and a rerouted stream 97% of its
// bytes). Prometheus and Mimir keep a series in the head block until head
// compaction, up to about 3 hours; Loki's volume API sums the last 24
// hours. A label is measured from series, so it settles as metrics do.
func LedgerSettle(signal string) time.Duration {
	if strings.HasPrefix(signal, "loki:") {
		return 24 * time.Hour
	}
	return 3 * time.Hour
}

// buildLedger measures each merge against the stored scans. nil when
// nothing has merged.
func buildLedger(merges []graph.Merge, scans map[int64]ScanCosts, current int64, currency string) *Ledger {
	if len(merges) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(scans))
	for id := range scans {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return scans[ids[i]].StartedAt.Before(scans[ids[j]].StartedAt) })
	l := &Ledger{Rows: []LedgerRow{}, Currency: currency}
	point := func(scan int64, signal string) LedgerPoint {
		c := scans[scan].Costs[signal]
		return LedgerPoint{Series: c.ActiveSeries, BytesPerDay: c.BytesPerDay, Monthly: c.MonthlyCost, DiskBytes: c.DiskBytes}
	}
	var baseline int64
	for _, m := range merges {
		row := LedgerRow{Repo: m.Repo, Recommendation: m.Recommendation, Signal: m.SignalID, MergedAt: m.MergedAt, Expected: m.Expected}
		row.Action, _, _ = strings.Cut(m.Recommendation, ":")
		// Before: the proposal's own scan when it's here and before the
		// merge, else the last scan before the merge.
		if s, ok := scans[m.ProposalScan]; ok && s.StartedAt.Before(m.MergedAt) {
			row.BeforeScan = m.ProposalScan
		} else {
			for _, id := range ids {
				if scans[id].StartedAt.Before(m.MergedAt) {
					row.BeforeScan = id
				}
			}
		}
		row.SettlesAt = m.MergedAt.Add(LedgerSettle(m.SignalID))
		var early int64 // the last scan after the merge but before it settled
		for _, id := range ids {
			at := scans[id].StartedAt
			if at.After(row.SettlesAt) {
				row.AfterScan = id
				break
			}
			if at.After(m.MergedAt) {
				early = id
			}
		}
		switch {
		case row.BeforeScan == 0:
			row.Status = LedgerUnmeasured
		case row.AfterScan == 0 && early != 0:
			row.Status = LedgerSettling
			row.AfterScan = early
			row.Before, row.After = point(row.BeforeScan, m.SignalID), point(early, m.SignalID)
			l.Settling++
		case row.AfterScan == 0:
			row.Status = LedgerPending
			row.Before = point(row.BeforeScan, m.SignalID)
			l.Pending++
		default:
			row.Status = LedgerMeasured
			row.Before, row.After = point(row.BeforeScan, m.SignalID), point(row.AfterScan, m.SignalID)
			row.Saved = row.Before.Monthly - row.After.Monthly
			l.Saved += row.Saved
			l.Expected += row.Expected
			l.DiskFreedBytes += row.Before.DiskBytes - row.After.DiskBytes
			if baseline == 0 || scans[row.BeforeScan].StartedAt.Before(scans[baseline].StartedAt) {
				baseline = row.BeforeScan
			}
		}
		l.Rows = append(l.Rows, row)
	}
	if baseline != 0 && baseline != current {
		if _, ok := scans[current]; ok {
			l.Growth = growth(scans[baseline].Costs, scans[current].Costs, baseline, l.Saved)
		}
	}
	if cur, ok := scans[current]; ok && cur.Retention > 0 && l.DiskFreedBytes > 0 {
		var disk int64
		for id, c := range cur.Costs {
			if !strings.Contains(id, ":label:") {
				disk += c.DiskBytes
			}
		}
		if days := cur.Retention.Hours() / 24; disk > 0 && days > 0 {
			l.RetentionDaysGained = float64(l.DiskFreedBytes) / (float64(disk) / days)
		}
	}
	return l
}

// growth compares the bill of two scans, over metrics and streams.
func growth(before, now map[string]graph.Cost, baseline int64, saved float64) *Growth {
	g := &Growth{BaselineScan: baseline}
	whole := func(id string) bool { return !strings.Contains(id, ":label:") }
	for id, c := range before {
		if whole(id) {
			g.Baseline += c.MonthlyCost
		}
	}
	for id, c := range now {
		if !whole(id) {
			continue
		}
		g.Now += c.MonthlyCost
		if _, ok := before[id]; !ok {
			g.NewSignals += c.MonthlyCost
		}
	}
	g.Total = g.Now - g.Baseline
	g.Growth = g.Total + saved
	g.Existing = g.Growth - g.NewSignals
	return g
}

// collectorRates turns the Collector counters into send rates before the
// first merge and after the last one.
func collectorRates(merges []graph.Merge, sent map[int64][]graph.CollectorSent, times map[int64]time.Time) []CollectorRate {
	if len(merges) == 0 || len(sent) == 0 {
		return nil
	}
	first, last := merges[0].MergedAt, merges[0].MergedAt
	for _, m := range merges {
		if m.MergedAt.Before(first) {
			first = m.MergedAt
		}
		if m.MergedAt.After(last) {
			last = m.MergedAt
		}
	}
	var ids []int64
	for id := range sent {
		if _, ok := times[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return times[ids[i]].Before(times[ids[j]]) })
	var before, after []int64
	for _, id := range ids {
		switch {
		case times[id].Before(first):
			before = append(before, id)
		case times[id].After(last):
			after = append(after, id)
		}
	}
	type key struct{ pipeline, exporter, signal string }
	value := func(id int64) map[key]float64 {
		out := map[key]float64{}
		for _, c := range sent[id] {
			out[key{c.Pipeline, c.Exporter, c.Signal}] = c.Sent
		}
		return out
	}
	rows := map[key]*CollectorRate{}
	row := func(k key) *CollectorRate {
		if rows[k] == nil {
			rows[k] = &CollectorRate{Pipeline: k.pipeline, Exporter: k.exporter, Signal: k.signal}
		}
		return rows[k]
	}
	rate := func(pair []int64, set func(*CollectorRate, float64)) {
		if len(pair) < 2 {
			return
		}
		a, b := pair[len(pair)-2], pair[len(pair)-1]
		secs := times[b].Sub(times[a]).Seconds()
		if secs <= 0 {
			return
		}
		va, vb := value(a), value(b)
		for k, v := range vb {
			if prev, ok := va[k]; ok && v >= prev {
				set(row(k), (v-prev)/secs)
			} else {
				row(k)
			}
		}
	}
	rate(before, func(r *CollectorRate, v float64) { r.Before, r.BeforeKnown = v, true })
	rate(after, func(r *CollectorRate, v float64) { r.After, r.AfterKnown = v, true })
	out := make([]CollectorRate, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Pipeline != b.Pipeline {
			return a.Pipeline < b.Pipeline
		}
		if a.Exporter != b.Exporter {
			return a.Exporter < b.Exporter
		}
		return a.Signal < b.Signal
	})
	return out
}
