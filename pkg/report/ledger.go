package report

import (
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
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
// the scan before its merge to the first scan after it, apart from what
// the bill did meanwhile.
type Ledger struct {
	Rows []LedgerRow `json:"rows"`
	// Saved is the measured monthly saving of the measured rows, and
	// Expected what those rows claimed when proposed.
	Saved    float64 `json:"saved_monthly"`
	Expected float64 `json:"expected_monthly"`
	Currency string  `json:"currency,omitempty"`
	Pending  int     `json:"pending"`
	// Growth is the bill apart from the savings, from the earliest scan
	// before a merge to this scan.
	Growth *Growth `json:"growth,omitempty"`
	// DiskFreedBytes is the self-hosted disk the measured rows freed, and
	// RetentionDaysGained how many more days of history that disk holds at
	// this scan's rate (S11 item 3); 0 for a vendor card.
	DiskFreedBytes      int64   `json:"disk_freed_bytes,omitempty"`
	RetentionDaysGained float64 `json:"retention_days_gained,omitempty"`
}

// LedgerRow is one merged recommendation.
type LedgerRow struct {
	Repo           string    `json:"repo"`
	Recommendation string    `json:"recommendation"`
	Action         string    `json:"action"`
	Signal         string    `json:"signal"`
	MergedAt       time.Time `json:"merged_at"`
	// Status: measured (a scan before and after the merge), pending (no
	// scan after it yet) or unmeasured (no scan before it in this
	// database).
	Status     string      `json:"status"`
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
)

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
		for _, id := range ids {
			if scans[id].StartedAt.After(m.MergedAt) {
				row.AfterScan = id
				break
			}
		}
		switch {
		case row.BeforeScan == 0:
			row.Status = LedgerUnmeasured
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
