package report

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// IntervalSteps are the scrape intervals tvt proposes, shortest first
// (S10 item 5).
var IntervalSteps = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

// Interval guards (ADR 0010 decision 3, S8 item 5).
const (
	// SamplesPerWindow: every range a reader uses over raw samples must
	// hold at least this many samples at the new interval.
	SamplesPerWindow = 4
	// InstantMaxInterval: a selector read without a range sees a series
	// only within the lookback delta (5m by default) of its last sample;
	// at most 2m keeps two samples in it, so one missed scrape doesn't
	// blank a panel or resolve an alert.
	InstantMaxInterval = 2 * time.Minute
)

// IntervalChange is a scrape job whose interval can go up instead of
// anything being dropped (S10 item 5, ADR 0010: after label removal,
// before retention and drops). Held says why it can't, for a job with
// readers.
type IntervalChange struct {
	Job         string  `json:"job"`
	CurrentSec  float64 `json:"current_seconds"`
	ProposedSec float64 `json:"proposed_seconds,omitempty"`
	Metrics     int     `json:"metrics"`
	Readers     int     `json:"readers"`
	// Shortest is the shortest range any reader uses over the job's raw
	// samples, and ShortestBy that reader; empty when none uses a range.
	Shortest   string `json:"shortest_window,omitempty"`
	ShortestBy string `json:"shortest_by,omitempty"`
	// Bound says which guard set the proposal (or, held, which one leaves
	// no step above the current interval).
	Bound string `json:"bound,omitempty"`
	// Monthly is the storage saved a month on a self-hosted card (samples
	// fall with the interval; an upper bound, as head and WAL don't), 0 on
	// a vendor card, whose price here follows series, not samples.
	Monthly float64 `json:"monthly_saving"`
	Held    string  `json:"held,omitempty"`
}

// intervalRows proposes a longer scrape interval per job. Every reader of
// every metric the job carries must have a window tvt can read: an
// unreadable one (a $__rate_interval panel, a query that doesn't parse)
// holds the job, since Grafana's $__rate_interval follows the data
// source's scrape interval setting, which tvt doesn't change.
func intervalRows(in Input, refs map[string][]graph.Reference, consumers map[string]graph.Consumer, costs map[string]graph.Cost) []IntervalChange {
	jobMetrics := map[string][]string{}
	for _, pt := range in.PipelineTargets {
		if !strings.Contains(pt.Subject, ":metric:") {
			continue
		}
		for _, j := range pt.Jobs {
			if !slices.Contains(jobMetrics[j], pt.Subject) {
				jobMetrics[j] = append(jobMetrics[j], pt.Subject)
			}
		}
	}
	gw := in.GroupWait
	if gw == 0 {
		gw = DefaultGroupWait
	}
	alertsByJob := map[string][]AlertTiming{}
	for _, a := range AlertTimings(in) {
		for _, j := range a.Jobs {
			alertsByJob[j] = append(alertsByJob[j], a)
		}
	}
	jobs := make([]string, 0, len(jobMetrics))
	for j := range jobMetrics {
		jobs = append(jobs, j)
	}
	sort.Strings(jobs)
	var out []IntervalChange
	for _, job := range jobs {
		ms := jobMetrics[job]
		row := IntervalChange{Job: job, Metrics: len(ms)}
		var current time.Duration
		for _, m := range ms {
			if c := costs[m]; c.IntervalAssumed == "" {
				current = max(current, c.Interval)
			}
		}
		row.CurrentSec = current.Seconds()
		seen := map[string]bool{}
		var shortest time.Duration
		instant := false
		for _, m := range ms {
			for _, r := range refs[m] {
				id := r.ConsumerID
				if r.Via != "" {
					// The rule reads the raw samples; its readers read the
					// rule's output, which the scrape interval doesn't set.
					id = r.Via
				}
				con, ok := consumers[id]
				if !ok || seen[id] {
					continue
				}
				seen[id] = true
				if row.Held != "" {
					continue
				}
				if con.Unparsed != "" {
					row.Held = fmt.Sprintf("%s's query wasn't read (%s)", con.Name, con.Unparsed)
					continue
				}
				if con.Expr == "" {
					continue
				}
				w, err := parse.QueryWindows(con.Expr)
				if err != nil {
					row.Held = fmt.Sprintf("%s's window is unknown: %v", con.Name, err)
					continue
				}
				instant = instant || w.Instant
				if w.Shortest > 0 && (shortest == 0 || w.Shortest < shortest) {
					shortest, row.ShortestBy = w.Shortest, con.Name
				}
			}
		}
		row.Readers = len(seen)
		if row.Readers == 0 {
			continue // nothing reads it: the drops cover it
		}
		if shortest > 0 {
			row.Shortest = shortDuration(shortest)
		}
		if row.Held == "" && current <= 0 {
			row.Held = "the job's scrape interval wasn't measured"
		}
		if row.Held != "" {
			out = append(out, row)
			continue
		}
		var best time.Duration
		bound := "the longest step tvt proposes"
		for _, step := range IntervalSteps {
			if step <= current {
				continue
			}
			if shortest > 0 && shortest < SamplesPerWindow*step {
				bound = fmt.Sprintf("%s's %s range needs %d samples", row.ShortestBy, row.Shortest, SamplesPerWindow)
				break
			}
			if instant && step > InstantMaxInterval {
				bound = fmt.Sprintf("a reader without a range keeps two samples in the 5m lookback at %s", shortDuration(InstantMaxInterval))
				break
			}
			if a, why := pastObjective(alertsByJob[job], step, gw, in.Objectives); a != "" {
				bound = why
				break
			}
			best = step
		}
		row.Bound = bound
		if best == 0 {
			row.Held = "no longer step passes: " + bound
			out = append(out, row)
			continue
		}
		row.ProposedSec = best.Seconds()
		if in.Retention > 0 {
			for _, m := range ms {
				row.Monthly += costs[m].MonthlyCost * (1 - current.Seconds()/best.Seconds())
			}
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Held == "") != (out[j].Held == "") {
			return out[i].Held == ""
		}
		return out[i].Monthly > out[j].Monthly
	})
	return out
}

// pastObjective returns the first alert whose worst-case detection delay
// at scrape would pass its severity's objective, with the reason.
func pastObjective(alerts []AlertTiming, scrape, gw time.Duration, objectives map[string]time.Duration) (string, string) {
	for _, a := range alerts {
		obj, ok := objectives[a.Severity]
		if !ok || a.Logs {
			continue
		}
		if d, terms := a.Delay(scrape.Seconds(), gw); d > obj {
			return a.Name, fmt.Sprintf("%s would detect in %s, past the %s objective %s (%s)", a.Name, shortDuration(d), a.Severity, shortDuration(obj), terms)
		}
	}
	return "", ""
}
