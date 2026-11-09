// Package scan turns collector output into usage graph rows. It does no
// I/O, so the mapping and its ID scheme are tested without a backend or a
// database; cmd/tvt runs the collectors and writes the result to the store.
//
// IDs are stable across scans, because the store carries first_seen and
// last_viewed forward by ID:
//
//	<src>:metric:<name>                  metric signal (src = "prom" or "mimir")
//	<src>:label:<metric>:<label>         label signal
//	rule:<source>:<namespace>/<group>/<name>
//	panel:<org>:<dashboard uid>:<panel id>:<query index>
//	var:<org>:<dashboard uid>:<variable name>
//
// Two consumers that would share an ID (duplicate alert names in one group,
// repeated panel IDs) get "#2", "#3"… in the order they were collected,
// skipping any ID already taken.
package scan

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// Inputs is everything one scan collected.
type Inputs struct {
	// Start is when the scan began; Window is the observation window.
	Start  time.Time
	Window time.Duration
	// MetricsSource is "prom" or "mimir": the prefix of signal IDs.
	MetricsSource string
	Metrics       []collect.MetricCardinality
	Labels        []collect.LabelCardinality
	Rules         []collect.Rule
	// OrgID is the Grafana organization the dashboards belong to.
	OrgID      int64
	Dashboards []collect.Dashboard
	// Views are dashboard loads in the window, tvt's own already excluded.
	Views []collect.DashboardView
	// Sources is the coverage of each evidence source, configured or not.
	Sources []graph.SourceCoverage
}

// Output is one scan's graph rows.
type Output struct {
	Signals   []graph.Signal
	Consumers []graph.Consumer
	Costs     []graph.Cost
	Loads     []graph.DashboardLoad
	Sources   []graph.SourceCoverage
	// LogCoverage is the mean coverage of Sources; an unconfigured source
	// counts as 0.
	LogCoverage float64
}

// Build maps inputs to graph rows. It rejects inputs a scan should never
// produce (no start, an unknown metrics source, coverage outside 0..1)
// rather than writing a graph that misrepresents usage.
func Build(in Inputs) (Output, error) {
	if in.Start.IsZero() || in.Window <= 0 {
		return Output{}, errors.New("scan: Start and Window are required")
	}
	if in.MetricsSource != "prom" && in.MetricsSource != "mimir" {
		return Output{}, fmt.Errorf("scan: metrics source %q, want prom or mimir", in.MetricsSource)
	}
	var out Output
	since := in.Start.Add(-in.Window)

	// Signals and their costs.
	metricIDs := map[string]bool{}
	for _, m := range in.Metrics {
		id := in.MetricsSource + ":metric:" + m.Metric
		if metricIDs[id] {
			continue
		}
		metricIDs[id] = true
		out.Signals = append(out.Signals, graph.Signal{ID: id, Kind: graph.KindMetric, Name: m.Metric, Source: in.MetricsSource})
		out.Costs = append(out.Costs, graph.Cost{SignalID: id, ActiveSeries: m.ActiveSeries, ObservedAt: in.Start})
	}
	for _, l := range in.Labels {
		if !metricIDs[in.MetricsSource+":metric:"+l.Metric] {
			return Output{}, fmt.Errorf("scan: label %q on %q, a metric the scan didn't collect", l.Label, l.Metric)
		}
		id := in.MetricsSource + ":label:" + l.Metric + ":" + l.Label
		out.Signals = append(out.Signals, graph.Signal{ID: id, Kind: graph.KindLabel, Name: l.Label, Parent: l.Metric, Source: in.MetricsSource})
		out.Costs = append(out.Costs, graph.Cost{SignalID: id, ActiveSeries: l.ActiveSeries, DistinctValues: l.Values, ObservedAt: in.Start})
	}

	ids := newIDSet()

	// Rules.
	for _, r := range in.Rules {
		kind := graph.ConsumerRecordingRule
		if r.Kind == collect.RuleAlert {
			kind = graph.ConsumerAlertRule
		}
		out.Consumers = append(out.Consumers, graph.Consumer{
			ID:   ids.unique("rule:" + r.Source + ":" + r.Namespace + "/" + r.Group + "/" + r.Name),
			Kind: kind,
			Name: r.Name,
			Expr: r.Expr,
		})
	}

	// Views: the latest browser view per dashboard sets last_viewed, and
	// every load is kept, aggregated, as evidence.
	org := strconv.FormatInt(in.OrgID, 10)
	lastViewed := map[string]time.Time{}
	type loadKey struct {
		org     int64
		uid     string
		login   string
		browser bool
	}
	loads := map[loadKey]*graph.DashboardLoad{}
	for _, v := range in.Views {
		if v.At.Before(since) || v.At.After(in.Start) {
			continue
		}
		if v.Browser && v.OrgID == in.OrgID && v.At.After(lastViewed[v.DashboardUID]) {
			lastViewed[v.DashboardUID] = v.At
		}
		k := loadKey{v.OrgID, v.DashboardUID, v.User, v.Browser}
		l := loads[k]
		if l == nil {
			l = &graph.DashboardLoad{OrgID: v.OrgID, DashboardUID: v.DashboardUID, Login: v.User, Browser: v.Browser}
			loads[k] = l
		}
		l.Loads++
		if v.At.After(l.LastAt) {
			l.LastAt = v.At
		}
	}
	for _, l := range loads {
		out.Loads = append(out.Loads, *l)
	}
	sort.Slice(out.Loads, func(i, j int) bool {
		a, b := out.Loads[i], out.Loads[j]
		if a.OrgID != b.OrgID {
			return a.OrgID < b.OrgID
		}
		if a.DashboardUID != b.DashboardUID {
			return a.DashboardUID < b.DashboardUID
		}
		if a.Login != b.Login {
			return a.Login < b.Login
		}
		return !a.Browser && b.Browser
	})

	// Dashboards: one consumer per panel query and per query variable.
	for _, d := range in.Dashboards {
		viewed := lastViewed[d.UID]
		for _, p := range d.Panels {
			for n, q := range p.Queries {
				out.Consumers = append(out.Consumers, graph.Consumer{
					ID:         ids.unique(fmt.Sprintf("panel:%s:%s:%d:%d", org, d.UID, p.ID, n)),
					Kind:       graph.ConsumerDashboardPanel,
					Name:       d.Title + " / " + p.Title,
					Expr:       q.Expr,
					LastViewed: viewed,
				})
			}
		}
		for _, v := range d.Variables {
			out.Consumers = append(out.Consumers, graph.Consumer{
				ID:         ids.unique("var:" + org + ":" + d.UID + ":" + v.Name),
				Kind:       graph.ConsumerDashboardVariable,
				Name:       d.Title + " / $" + v.Name,
				Expr:       v.Query,
				LastViewed: viewed,
			})
		}
	}

	// Coverage.
	seen := map[string]bool{}
	var sum float64
	for _, s := range in.Sources {
		if s.Coverage < 0 || s.Coverage > 1 {
			return Output{}, fmt.Errorf("scan: source %q coverage %v outside 0..1", s.Source, s.Coverage)
		}
		if seen[s.Source] {
			return Output{}, fmt.Errorf("scan: source %q listed twice", s.Source)
		}
		seen[s.Source] = true
		if !s.Configured {
			s.Coverage = 0
		}
		if s.WindowStart.IsZero() {
			s.WindowStart = since
		}
		out.Sources = append(out.Sources, s)
		sum += s.Coverage
	}
	if len(out.Sources) > 0 {
		out.LogCoverage = sum / float64(len(out.Sources))
	}
	return out, nil
}

// idSet hands out unique IDs, suffixing repeats with #2, #3, … Every ID
// handed out is reserved, so a collected name that already looks like a
// suffix ("HighLatency#2") can't collide with a generated one.
type idSet struct {
	used map[string]bool
	next map[string]int // next suffix to try for a base ID
}

func newIDSet() *idSet {
	return &idSet{used: map[string]bool{}, next: map[string]int{}}
}

func (s *idSet) unique(id string) string {
	cand := id
	for n := max(s.next[id], 2); s.used[cand]; n++ {
		cand = id + "#" + strconv.Itoa(n)
		s.next[id] = n + 1
	}
	s.used[cand] = true
	return cand
}
