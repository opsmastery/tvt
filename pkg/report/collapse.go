package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// ActionCollapse: collapse a stream's identical lines into one record a
// window with a count (S10 item 4, ADR 0010: after a cap, before level
// tiers). The OTel Collector's log_dedup does it; a count over the
// stream then has to sum the count attribute, so tvt collapses only
// streams nothing reads.
const ActionCollapse = "collapse"

// CollapseWindow is the window identical lines collapse over: the
// writer's default (propose.DefaultDedupInterval).
const CollapseWindow = time.Minute

// collapseRecs recommends a collapse for each stream with repeated-line
// findings (Loki's pattern API: identical text, at least a quarter of the
// stream's bytes) that nothing reads and that isn't a record. The saving
// is each pattern's bytes but one line a window: a line repeated at r per
// second becomes one record per CollapseWindow.
func collapseRecs(in Input, signals map[string]graph.Signal, refs map[string][]graph.Reference, consumers map[string]graph.Consumer, gs *gaps, listed map[string]bool) []Recommendation {
	type agg struct {
		monthly  float64
		patterns []string
		lines    int64
		window   time.Duration
	}
	by := map[string]*agg{}
	var order []string
	for _, f := range in.LineFindings {
		if f.Kind != graph.LineRepeated || f.MonthlyCost <= 0 {
			continue
		}
		perWindow := f.PerSecond() * CollapseWindow.Seconds()
		if perWindow <= 1 {
			continue // at most one line a window: nothing collapses
		}
		a := by[f.Stream]
		if a == nil {
			a = &agg{}
			by[f.Stream] = a
			order = append(order, f.Stream)
		}
		a.monthly += f.MonthlyCost * (1 - 1/perWindow)
		a.patterns = append(a.patterns, f.Pattern)
		a.lines += f.Lines
		if f.Window > a.window {
			a.window = f.Window
		}
	}
	sort.Strings(order)
	var out []Recommendation
	for _, sel := range order {
		a := by[sel]
		id := "loki:stream:" + sel
		s, ok := signals[id]
		if !ok {
			s = graph.Signal{ID: id, Kind: graph.KindStream, Name: sel, Source: "loki"}
		}
		if s.Record != "" || streamRead(sel, in.Signals, refs) {
			continue
		}
		rec := Recommendation{
			ID:     ActionCollapse + ":" + id,
			Action: ActionCollapse,
			Signal: SignalRef{ID: id, Kind: string(graph.KindStream), Name: sel, Source: s.Source},
			Claim: fmt.Sprintf("%s repeats %s, identical, and nothing reads the stream: collapsing identical lines to one record a minute with a count saves %.2f%s a month.",
				sel, plural64(len(a.patterns), "line", "lines"), a.monthly, map[bool]string{true: " " + in.Scan.Currency, false: ""}[in.Scan.Currency != ""]),
			Evidence:   Evidence{References: []string{}, QueryLog: []LoggedQuery{}},
			Downstream: []Reader{},
			Assumptions: []string{"Loki's pattern API sampled the stream's lines; repeated lines arrive evenly, so each window holds more than one",
				"a level tier or reroute of the same stream moves some of the same bytes: the savings overlap"},
			Owner: s.Owner, OwnerSource: s.OwnerSource, OwnerInferred: s.OwnerInferred,
			Status:      StatusOpen,
			Savings:     Savings{Monthly: a.monthly, Currency: in.Scan.Currency, Approximate: true},
			RiskIfWrong: Risk{Evidence: "none", Tier: s.Tier},
		}
		rec.Evidence.Gaps, rec.Evidence.Acknowledged = split(gs.forSignal(s, listed))
		if rec.Evidence.Gaps == nil {
			rec.Evidence.Gaps = []string{}
		}
		rec.CheapestCheck = "Check that nothing counts the stream's lines (a count would see one line a minute): tvt why '" + sel + "'"
		rec.CheckMinutes = 5
		rec.EvidenceHash = EvidenceHash(rec.Evidence)
		rec.SavingsPerMinute = rec.Savings.Monthly / float64(rec.CheckMinutes)
		rec.Expected = Expected{WithinSec: expectedWithin.Seconds(), SeriesAfter: -1, EmptyReaders: []string{}, MonthlySaving: a.monthly,
			Statements: []string{
				fmt.Sprintf("%s's repeated lines arrive as one record a minute with a log_count attribute within an hour of the deploy", sel),
				"its other lines and its records are unchanged",
				"no query, alert or panel changes: nothing reads the stream",
			}}
		out = append(out, rec)
	}
	return out
}

// streamRead: some reader (not a sweep) reads a stream signal whose
// selector matches the stream's labels, or every stream.
func streamRead(sel string, signals []graph.Signal, refs map[string][]graph.Reference) bool {
	labels, ok := selectorLabels(sel)
	if !ok {
		return true // can't tell: count it as read
	}
	for _, s := range signals {
		if s.Kind != graph.KindStream {
			continue
		}
		if s.Name != "*" {
			rs, err := parse.LogQL(s.Name)
			if err != nil || len(rs) != 1 {
				continue
			}
			if m, err := parse.MatchesLabels(rs[0].Matchers, labels); err != nil || !m {
				continue
			}
		}
		for _, ref := range refs[s.ID] {
			if !ref.Sweep {
				return true
			}
		}
	}
	return false
}

func plural64(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, strings.TrimSpace(many))
}
