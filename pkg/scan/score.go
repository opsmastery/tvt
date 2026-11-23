package scan

import (
	"sort"
	"strings"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/score"
)

// Usage-log evidence sources, as stored in scan_sources.
const (
	SourcePromQueryLog  = "prometheus query log"
	SourceMimirQueryLog = "mimir query log"
	SourceLokiQueryLog  = "loki query log"
	SourceGrafanaLog    = "grafana router log"
	// SourceGrafanaUsageInsights is Grafana Cloud's view events, in place
	// of the router log.
	SourceGrafanaUsageInsights = "grafana usage insights"
	// SourceCloudDirectQueries is the gap Grafana Cloud leaves: queries
	// sent straight to its Mimir and Loki APIs are logged without text. A
	// gap, not a log, so it is left out of LogCoverage.
	SourceCloudDirectQueries = "grafana cloud direct queries"
)

// viewCoverage is the coverage of whichever view source the scan read:
// the router log (OSS) or Usage Insights (Cloud).
func viewCoverage(coverage map[string]float64) float64 {
	return max(coverage[SourceGrafanaLog], coverage[SourceGrafanaUsageInsights])
}

// History is what earlier scans in the store know about signals; the zero
// value is a first scan.
type History struct {
	// FirstSeen is each stored signal's first sighting.
	FirstSeen map[string]time.Time
	// LastReads are the most recent reads query logs ever showed, kept
	// after they leave every log (store.LastReads), sweeps excluded.
	LastReads []PastRead
	// LastViewed is each stored consumer's latest view (dashboards, and
	// query-log panels), kept after the Grafana log line rotates away.
	LastViewed map[string]time.Time
}

// PastRead is a signal's most recent read in any earlier scan.
type PastRead struct {
	SignalID string
	At       time.Time
}

// scoreCandidates ranks every candidate signal (Sprint 3 item 7). Evidence
// comes from this scan's references and, for reads that have left the
// logs, from History; protected and dormant signals aren't scored.
//
// A signal's coverage is the mean coverage of the usage logs that could
// have shown it read: its backend's query log and Grafana's router log
// (dashboard views). Its first sighting counts only when it came after
// tvt started watching its backend (the earliest first sighting of any
// signal from that backend): a signal from that first scan predates tvt,
// and its age is unknown. A backend no earlier scan saw is the same.
func scoreCandidates(in Inputs, signals []graph.Signal, consumers []graph.Consumer, refs []graph.Reference, costs []graph.Cost, sources []graph.SourceCoverage, since time.Time) []graph.Score {
	byID := make(map[string]graph.Consumer, len(consumers))
	for _, c := range consumers {
		byID[c.ID] = c
	}
	evidence := map[string][]score.Evidence{}
	for _, r := range refs {
		evidence[r.SignalID] = append(evidence[r.SignalID], refEvidence(r, byID[r.ConsumerID], since, in.Start))
	}
	for _, p := range in.History.LastReads {
		evidence[p.SignalID] = append(evidence[p.SignalID], score.Evidence{
			Usage: score.UsageAdhocQuery, Class: score.EvidenceObserved, LastSeen: p.At,
		})
	}
	monthly := make(map[string]float64, len(costs))
	for _, c := range costs {
		monthly[c.SignalID] = c.MonthlyCost
	}
	coverage := map[string]float64{}
	for _, s := range sources {
		if s.Configured {
			coverage[s.Source] = s.Coverage
		}
	}
	watched := map[string]time.Time{} // when tvt first saw each signal source
	for id, first := range in.History.FirstSeen {
		if src, _, ok := strings.Cut(id, ":"); ok {
			if w, seen := watched[src]; !seen || first.Before(w) {
				watched[src] = first
			}
		}
	}

	ctx := score.Context{
		Now:           in.Start,
		WindowDays:    int(in.Window / (24 * time.Hour)),
		MinWindowDays: in.MinWindowDays,
		Weights:       score.DefaultWeights,
	}
	var out []graph.Score
	for _, s := range signals {
		if s.Class != string(score.ClassCandidate) {
			continue
		}
		cov := (coverage[queryLogSource(s.Source)] + viewCoverage(coverage)) / 2
		input := score.Input{
			SignalID:    s.ID,
			MonthlyCost: monthly[s.ID],
			Evidence:    evidence[s.ID],
			Coverage:    &cov,
		}
		if since, ok := watched[s.Source]; ok {
			if first, ok := in.History.FirstSeen[s.ID]; ok && first.After(since) {
				input.FirstSeen = first
			} else if !ok {
				input.FirstSeen = in.Start // appeared in this scan
			}
		}
		r, ok := score.Score(input, ctx)
		if !ok {
			continue
		}
		out = append(out, graph.Score{
			SignalID:    r.SignalID,
			MonthlyCost: r.MonthlyCost,
			UsageWeight: r.UsageWeight,
			Strongest:   r.Strongest.String(),
			Confidence:  r.Confidence,
			Priority:    r.Priority,
			Held:        r.Held,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].SignalID < out[j].SignalID
	})
	return out
}

// refEvidence is what one reference says about use. A dashboard not viewed
// in the window weighs as stale whatever its last view; a viewed one is
// observed and decays from that view.
func refEvidence(r graph.Reference, c graph.Consumer, since, until time.Time) score.Evidence {
	e := score.Evidence{Class: score.EvidenceClass(r.Evidence), LastSeen: r.LastSeen}
	switch {
	case r.Sweep:
		e.Usage, e.Class = score.UsageSweep, score.EvidenceObserved
	case c.Kind.HardKeep():
		e.Usage = score.UsageHardKeep
	case isDashboard(c.Kind):
		if !c.LastViewed.IsZero() && !c.LastViewed.Before(since) && !c.LastViewed.After(until) {
			e.Usage, e.Class, e.LastSeen = score.UsageViewedDashboard, score.EvidenceObserved, c.LastViewed
		} else {
			e.Usage, e.Class, e.LastSeen = score.UsageStaleDashboard, score.EvidenceDeclared, time.Time{}
		}
	case c.Kind == graph.ConsumerRecordingRule:
		e.Usage = score.UsageRecordingRule
	case c.Kind == graph.ConsumerAgentQuery:
		e.Usage = score.UsageAgentQuery
	default: // ad hoc queries, and anything newer that reads the signal
		e.Usage = score.UsageAdhocQuery
	}
	return e
}

// queryLogSource is the query log that shows reads of a signal source.
func queryLogSource(signalSource string) string {
	switch signalSource {
	case "prom":
		return SourcePromQueryLog
	case "mimir":
		return SourceMimirQueryLog
	case "loki":
		return SourceLokiQueryLog
	}
	return ""
}
