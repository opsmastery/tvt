package report

import (
	"fmt"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
)

// Expected is what should be true after a recommendation merges (S9 item
// 5): the owner reads it before merging, and tvt verify (S11) checks it.
type Expected struct {
	// Statements are the plain-language expectations, in order.
	Statements []string `json:"statements"`
	// Within is how soon after the deploy the change should show.
	WithinSec float64 `json:"within_sec"`
	// SeriesAfter: a metric's active series after the change (0 for a
	// drop; fewer for an aggregating label removal); -1 when not a metric.
	SeriesAfter int64 `json:"series_after"`
	// HotBytesPerDayFall is how many bytes a day leave the hot tier (a
	// reroute); SharePct is that as a share of every stream's bytes.
	HotBytesPerDayFall int64   `json:"hot_bytes_per_day_fall,omitempty"`
	SharePct           float64 `json:"share_pct,omitempty"`
	// EmptyReaders are readers that would go empty: none, by construction,
	// for a recommendation (anything read is protected); listed so verify
	// can check the same holds after the merge.
	EmptyReaders []string `json:"empty_readers"`
	// MonthlySaving is the saving tvt claims.
	MonthlySaving float64 `json:"monthly_saving"`
}

// expectedWithin is how long after a deploy tvt expects a change to show:
// an hour covers config reloads, restarts and a few scrape intervals.
const expectedWithin = time.Hour

// expected states rec's end state from its cost; totalStreamBytes is every
// stream's bytes a day, for a reroute's share.
func expected(rec Recommendation, c graph.Cost, totalStreamBytes int64) Expected {
	e := Expected{WithinSec: expectedWithin.Seconds(), SeriesAfter: -1, EmptyReaders: []string{}, MonthlySaving: rec.Savings.Monthly}
	money := fmt.Sprintf("%.2f", rec.Savings.Monthly)
	if rec.Savings.Currency != "" {
		money += " " + rec.Savings.Currency
	}
	switch rec.Action {
	case ActionDrop:
		e.SeriesAfter = 0
		e.Statements = []string{
			fmt.Sprintf("%s returns no new samples within an hour of the deploy (it has %d active series now)", rec.Signal.Name, c.ActiveSeries),
			"no rule, alert or panel goes empty: nothing reads it",
			"ingest cost falls by about " + money + " a month",
		}
	case ActionLabelRemoval:
		after := c.ActiveSeries
		if rec.Removal == "aggregate" {
			after = c.ActiveSeries - rec.Merges
		}
		e.SeriesAfter = after
		e.Statements = []string{
			fmt.Sprintf("new %s series carry no %s label within an hour of the deploy", rec.Signal.Parent, rec.Signal.Name),
			fmt.Sprintf("%s has about %d active series after, from %d", rec.Signal.Parent, after, c.ActiveSeries),
			"no rule, alert or panel changes its grouping or goes empty: nothing groups or filters on " + rec.Signal.Name,
			"cost falls by about " + money + " a month",
		}
	case ActionBucket:
		e.SeriesAfter = c.ActiveSeries - rec.Merges
		e.Statements = []string{
			fmt.Sprintf("new %s series carry %s bucketed to its class (503 to 500) within an hour of the deploy", rec.Signal.Parent, rec.Signal.Name),
			fmt.Sprintf("%s has about %d active series after, from %d", rec.Signal.Parent, e.SeriesAfter, c.ActiveSeries),
			"every reader of " + rec.Signal.Name + " returns the same result: each matches it only by class",
			"cost falls by about " + money + " a month",
		}
	case ActionReroute:
		e.HotBytesPerDayFall = c.BytesPerDay
		share := ""
		if totalStreamBytes > 0 && c.BytesPerDay > 0 {
			e.SharePct = 100 * float64(c.BytesPerDay) / float64(totalStreamBytes)
			share = fmt.Sprintf(" (about %.0f%% of all stream bytes)", e.SharePct)
		}
		e.Statements = []string{
			fmt.Sprintf("%s stops arriving in the hot tier within an hour of the deploy and arrives in cold storage instead", rec.Signal.Name),
			fmt.Sprintf("hot-tier log bytes fall by about %s a day%s", humanBytes(c.BytesPerDay), share),
			"no query, alert or panel goes empty: nothing reads the stream",
			"lines naming an error, exception, traceback, critical or fatal stay in the hot tier",
		}
	}
	return e
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
