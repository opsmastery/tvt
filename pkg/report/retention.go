package report

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/parse"
)

// DefaultDormantRetention is how much history a dormant metric keeps in
// its retention tier (S10 item 2) when tvt.yaml doesn't say: a week,
// enough for an owner to look back at a recent incident while they
// decide whether the signal can go.
const DefaultDormantRetention = 7 * 24 * time.Hour

// RetentionTier is ADR 0010's step between keeping a dormant metric
// whole and archiving or dropping it: keep a short history instead of
// all of it. It's on a confirm row, as an answer the owner can give
// ("keep a week") rather than keep or drop.
type RetentionTier struct {
	// KeepDays is the tier's retention: the dormant floor, or the
	// longest lookback a reader uses if that's longer, in whole days.
	KeepDays int `json:"keep_days"`
	// CurrentDays is the backend's retention now (measured or from the
	// price card), rounded down.
	CurrentDays float64 `json:"current_days"`
	// Lookback is the longest window any reader of the signal reads back
	// (range plus offset), and Reader the one that reads it; empty when
	// no reader has a window (a mention, an instant query).
	Lookback string `json:"lookback,omitempty"`
	Reader   string `json:"reader,omitempty"`
	// Monthly is the storage it saves a month, and DiskBytes the disk:
	// an upper bound, since the head and WAL share doesn't shrink with
	// retention.
	Monthly   float64 `json:"monthly_saving"`
	DiskBytes int64   `json:"disk_bytes,omitempty"`
	// Writer says where the tier can be written. Prometheus and Mimir
	// keep one retention per server or tenant, so tvt reports the tier
	// and writes nothing yet (VictoriaMetrics retention filters and
	// per-tenant routing come later).
	Writer string `json:"writer"`
}

// WriterNone is RetentionTier.Writer when no pipeline kind tvt writes
// can set retention per signal.
const WriterNone = "none yet: Prometheus and Mimir set retention per server or tenant, not per metric (set tiers.dormant_tenant for a Mimir tier tenant)"

// WriterTenant starts RetentionTier.Writer when tvt writes the tier as a
// route to a Mimir tenant (tiers.dormant_tenant).
const WriterTenant = "Mimir tenant "

// ActionRetentionTier: send a dormant metric to the tier tenant, which
// keeps a short history (S10 item 2, ADR 0010: before the archive and a
// drop).
const ActionRetentionTier = "retention_tier"

// retentionRec is a confirm row's tier as a recommendation: its owner
// confirms the shorter history on the PR, as for any change.
func retentionRec(c Confirm, s graph.Signal, meta Scan) Recommendation {
	t := c.Retention
	money := fmt.Sprintf("%.2f", t.Monthly)
	if meta.Currency != "" {
		money += " " + meta.Currency
	}
	rec := Recommendation{
		ID:     ActionRetentionTier + ":" + c.Signal.ID,
		Action: ActionRetentionTier,
		Signal: c.Signal,
		Claim: fmt.Sprintf("%s is dormant (%s): keeping %d days of it in its own tenant instead of %.1f saves %s a month, and the history an incident would need stays.",
			c.Signal.Name, c.Reason, t.KeepDays, t.CurrentDays, money),
		Evidence:    Evidence{References: []string{}, QueryLog: []LoggedQuery{}, Gaps: append([]string{}, c.Gaps...), Acknowledged: c.Acknowledged},
		Downstream:  []Reader{},
		Assumptions: []string{"the tier tenant's retention is set to the tier's days in Mimir's runtime overrides (compactor_blocks_retention_period)"},
		Owner:       c.Owner, OwnerSource: c.OwnerSource, OwnerInferred: c.OwnerInferred,
		Status:        StatusOpen,
		Savings:       Savings{Monthly: t.Monthly, Currency: meta.Currency, DiskBytes: t.DiskBytes, Approximate: true},
		RiskIfWrong:   Risk{Evidence: "none", Dormant: true, Tier: s.Tier},
		CheapestCheck: c.Question,
		CheckMinutes:  5,
		KeepDays:      t.KeepDays,
	}
	if rec.Evidence.Gaps == nil {
		rec.Evidence.Gaps = []string{}
	}
	if t.Lookback != "" {
		rec.Assumptions = append(rec.Assumptions, fmt.Sprintf("the longest window a reader reads back is %s (%s), inside the tier", t.Lookback, t.Reader))
	}
	rec.EvidenceHash = EvidenceHash(rec.Evidence)
	rec.SavingsPerMinute = rec.Savings.Monthly / float64(rec.CheckMinutes)
	rec.Expected = Expected{WithinSec: expectedWithin.Seconds(), SeriesAfter: -1, EmptyReaders: []string{}, MonthlySaving: t.Monthly,
		Statements: []string{
			fmt.Sprintf("%s stops arriving in the main tenant within an hour of the deploy and arrives in the tier tenant instead", c.Signal.Name),
			fmt.Sprintf("the tier tenant keeps %d days of it", t.KeepDays),
			"no rule, alert or panel goes empty: nothing reads it",
		}}
	return rec
}

// retentionTier is the tier for a dormant metric, or nil with the reason
// when there's none: not a self-hosted metric with a known retention, a
// reader whose window tvt can't read, or a tier no shorter than now.
func retentionTier(s graph.Signal, c graph.Cost, refs []graph.Reference, consumers map[string]graph.Consumer,
	current, floor time.Duration) (*RetentionTier, string) {
	if s.Kind != graph.KindMetric {
		return nil, "only metrics get a retention tier so far"
	}
	if current <= 0 || c.DiskBytes <= 0 {
		return nil, "no self-hosted retention to shorten"
	}
	if floor <= 0 {
		floor = DefaultDormantRetention
	}
	var longest time.Duration
	var longestBy string
	for _, r := range refs {
		con, ok := consumers[r.ConsumerID]
		if !ok {
			continue
		}
		if r.Via != "" {
			// Read through a recording rule: the reader's window applies to
			// the rule's output, which the tier doesn't touch, and the
			// rule's own window to the signal. Both are under refs.
			if rule, ok := consumers[r.Via]; ok {
				con = rule
			}
		}
		if con.Expr == "" {
			continue
		}
		if con.Unparsed != "" {
			return nil, fmt.Sprintf("%s's query wasn't read (%s), so its window is unknown", con.Name, con.Unparsed)
		}
		d, err := parse.Lookback(con.Expr)
		if err != nil {
			return nil, fmt.Sprintf("%s's window is unknown: %v", con.Name, err)
		}
		if d > longest {
			longest, longestBy = d, con.Name
		}
	}
	keep := max(floor, longest)
	keepDays := int(math.Ceil(keep.Hours() / 24))
	keep = time.Duration(keepDays) * 24 * time.Hour
	if keep >= current {
		return nil, fmt.Sprintf("a %d-day tier is no shorter than the %s retention now", keepDays, days(current))
	}
	share := 1 - keep.Seconds()/current.Seconds()
	t := &RetentionTier{
		KeepDays:    keepDays,
		CurrentDays: math.Floor(current.Hours()/24*10) / 10,
		Monthly:     c.MonthlyCost * share,
		DiskBytes:   int64(float64(c.DiskBytes) * share),
		Writer:      WriterNone,
	}
	if longest > 0 {
		t.Lookback, t.Reader = shortDuration(longest), longestBy
	}
	return t, ""
}

func days(d time.Duration) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", d.Hours()/24), ".0") + "-day"
}

// shortDuration writes a lookback as PromQL would (1d5m, 30d).
func shortDuration(d time.Duration) string {
	var b strings.Builder
	for _, u := range []struct {
		n time.Duration
		s string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if d >= u.n {
			fmt.Fprintf(&b, "%d%s", d/u.n, u.s)
			d %= u.n
		}
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}
