package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
)

// Gap kinds.
const (
	GapUnconfigured = "unconfigured" // the source isn't set up: it shows nothing
	GapUnproven     = "unproven"     // a query log whose probe never appeared
	GapPartial      = "partial"      // the source covers part of the window
	GapUnreadable   = "unreadable"   // a definition tvt couldn't read: a runbook, an SLO, a datasource it doesn't parse
	GapAssumed      = "assumed"      // a reference tvt inferred rather than read
	GapSeason       = "season"       // a season (tvt.yaml seasons) the window missed: its readers couldn't show
	GapStorage      = "storage"      // a storage hygiene warning (S5 item 6): no retention.size, or one too close to the volume; never a saving
	GapMonitors     = "monitors"     // a ServiceMonitor or PodMonitor no Prometheus selects, or one tvt can't decide (S5 item 8); never a saving
	GapUnmapped     = "unmapped"     // a signal no single pipeline claims (S6 item 4): reported, never proposable
	GapOverflow     = "overflow"     // a metric with OpenTelemetry overflow series (S6 item 5): its cost is a floor
)

// SourceAssumed names the gaps of assumed references.
const SourceAssumedRefs = "assumed references"

// Gap is one way the evidence falls short: a reader it would have shown
// can't be ruled out.
type Gap struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	// Acknowledged: tvt.yaml gaps.acknowledge accepts it, with Reason. An
	// acknowledged gap stays in the header but leaves the rows; an
	// unacknowledged one shows on every row it affects, and check fails
	// on it (S8).
	Acknowledged bool   `json:"acknowledged"`
	Reason       string `json:"reason,omitempty"`
}

// Acknowledgement accepts the gaps of Source whose text contains Match
// (every gap of the source when Match is empty), for Reason.
type Acknowledgement struct {
	Source string `json:"source"`
	Match  string `json:"match,omitempty"`
	Reason string `json:"reason"`
}

// sourceGaps are a source's gaps: unconfigured, or each line of its gap
// text, or partial coverage when it has no gap text.
func sourceGaps(s graph.SourceCoverage) []Gap {
	switch {
	case !s.Configured:
		return []Gap{{Source: s.Source, Kind: GapUnconfigured, Text: s.Source + " not configured"}}
	case s.Gap != "":
		kind := GapUnreadable
		switch {
		case isQueryLog(s.Source):
			kind = GapUnproven
		case strings.HasPrefix(s.Source, scan.SourceSeasonPrefix):
			kind = GapSeason
		case s.Source == scan.SourceStorage:
			kind = GapStorage
		case s.Source == scan.SourceMonitors:
			kind = GapMonitors
		case s.Source == scan.SourcePipelines:
			kind = GapUnmapped
		case s.Source == scan.SourceOverflow:
			kind = GapOverflow
		}
		var out []Gap
		for _, line := range strings.Split(s.Gap, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, Gap{Source: s.Source, Kind: kind, Text: s.Source + ": " + line})
			}
		}
		return out
	case s.Coverage < 1:
		return []Gap{{Source: s.Source, Kind: GapPartial, Text: fmt.Sprintf("%s covers %s of the window", s.Source, pct(s.Coverage))}}
	}
	return nil
}

func isQueryLog(source string) bool {
	switch source {
	case scan.SourcePromQueryLog, scan.SourceMimirQueryLog, scan.SourceLokiQueryLog, scan.SourceGrafanaLog, scan.SourceGrafanaUsageInsights:
		return true
	}
	return false
}

// gaps holds every gap of a scan and applies acknowledgements, keeping
// track of the ones that matched nothing.
type gaps struct {
	bySource map[string][]Gap
	acks     []Acknowledgement
	used     []bool
}

func newGaps(sources []graph.SourceCoverage, acks []Acknowledgement) *gaps {
	g := &gaps{bySource: map[string][]Gap{}, acks: acks, used: make([]bool, len(acks))}
	for _, s := range sources {
		for _, gap := range sourceGaps(s) {
			g.bySource[s.Source] = append(g.bySource[s.Source], g.ack(gap))
		}
	}
	return g
}

// ack marks gap acknowledged by the first acknowledgement that matches it.
func (g *gaps) ack(gap Gap) Gap {
	for i, a := range g.acks {
		if a.Source == gap.Source && strings.Contains(gap.Text, a.Match) {
			g.used[i] = true
			gap.Acknowledged, gap.Reason = true, a.Reason
			return gap
		}
	}
	return gap
}

// all is every source gap, in source order, plus header gaps for
// assumed references (one per consumer).
func (g *gaps) all(sources []graph.SourceCoverage, assumed []Gap) []Gap {
	out := []Gap{}
	for _, s := range sources {
		out = append(out, g.bySource[s.Source]...)
	}
	return append(out, assumed...)
}

// unmatched are the acknowledgements that matched no gap: stale config.
func (g *gaps) unmatched() []Acknowledgement {
	var out []Acknowledgement
	for i, a := range g.acks {
		if !g.used[i] {
			out = append(out, a)
		}
	}
	return out
}

// forSignal are the gaps in the sources that could have shown s read:
// its backend's query log, Grafana's router log or Usage Insights, Grafana
// Cloud's direct API queries, and the definition
// sources (datasources tvt can't parse, SLO files, runbooks). A backend
// whose query log source wasn't even listed counts as unconfigured.
func (g *gaps) forSignal(s graph.Signal, listed map[string]bool) []Gap {
	var out []Gap
	ql := queryLog(s.Source)
	if ql != "" && !listed[ql] {
		out = append(out, g.ack(Gap{Source: ql, Kind: GapUnconfigured, Text: ql + " not configured"}))
	}
	for _, n := range []string{ql, scan.SourceGrafanaLog, scan.SourceGrafanaUsageInsights, scan.SourceCloudDirectQueries, scan.SourceDatasources, scan.SourceSLOs, scan.SourceRunbooks} {
		if n != "" {
			out = append(out, g.bySource[n]...)
		}
	}
	return out
}

// assumedGaps are the header gaps for assumed references: one per
// consumer, with how many signals it is assumed to read and why.
func (g *gaps) assumedGaps(refs []graph.Reference, consumers map[string]graph.Consumer) []Gap {
	type agg struct {
		n       int
		reasons map[string]bool
	}
	by := map[string]*agg{}
	for _, r := range refs {
		if !r.Assumed {
			continue
		}
		a := by[r.ConsumerID]
		if a == nil {
			a = &agg{reasons: map[string]bool{}}
			by[r.ConsumerID] = a
		}
		a.n++
		for _, why := range r.Reasons {
			a.reasons[why] = true
		}
	}
	ids := make([]string, 0, len(by))
	for id := range by {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Gap
	for _, id := range ids {
		a := by[id]
		reasons := make([]string, 0, len(a.reasons))
		for why := range a.reasons {
			reasons = append(reasons, why)
		}
		sort.Strings(reasons)
		text := fmt.Sprintf("%s is assumed to read %s", readerName(consumers[id], id), plural(a.n, "signal", "signals"))
		if len(reasons) > 0 {
			text += " (" + strings.Join(reasons, "; ") + ")"
		}
		out = append(out, g.ack(Gap{Source: SourceAssumedRefs, Kind: GapAssumed, Text: text}))
	}
	return out
}

// rowGap is an assumed reference's gap on the row of the signal it reads.
func (g *gaps) rowGap(c graph.Consumer, id string, reasons []string) Gap {
	why := strings.Join(reasons, "; ")
	if why == "" {
		why = "unresolved"
	}
	return g.ack(Gap{Source: SourceAssumedRefs, Kind: GapAssumed, Text: fmt.Sprintf("%s is assumed to read it (%s)", readerName(c, id), why)})
}

// split separates a row's gaps into the unacknowledged texts the row shows
// and the acknowledged ones, each with its reason.
func split(gs []Gap) (open, acked []string) {
	open, acked = []string{}, []string{}
	for _, g := range uniqueGaps(gs) {
		if g.Acknowledged {
			acked = append(acked, g.Text+" (acknowledged: "+g.Reason+")")
		} else {
			open = append(open, g.Text)
		}
	}
	return open, acked
}

// uniqueGaps drops repeats, keeping the first: one assumed reader can
// reach a signal directly and through a recording rule, with the same text.
func uniqueGaps(gs []Gap) []Gap {
	seen := map[Gap]bool{}
	out := make([]Gap, 0, len(gs))
	for _, g := range gs {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
