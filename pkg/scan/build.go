// Package scan turns collector output into usage graph rows. It does no
// I/O, so the mapping and its ID scheme are tested without a backend or a
// database; cmd/tvt runs the collectors and writes the result to the store.
//
// IDs are stable across scans, because the store carries first_seen and
// last_viewed forward by ID:
//
//	<src>:metric:<name>                  metric signal (src = "prom" or "mimir")
//	<src>:label:<metric>:<label>         label signal
//	loki:stream:<selector>               log stream selector (canonical, see parse.CanonicalSelector)
//	loki:stream:*                        every stream: a LogQL query whose selectors couldn't be read
//	rule:<source>:<namespace>/<group>/<name>
//	rule:grafana:<folder uid>/<group>/<rule uid>   Grafana-managed rule
//	panel:<org>:<dashboard uid>:<panel id>:<query index>
//	var:<org>:<dashboard uid>:<variable name>
//	annot:<org>:<dashboard uid>:<annotation name>
//	slo:<format>:<file>:<name>           SLO definition (declared, hard keep)
//	runbook:<link>                       alert runbook or Grafana link (declared, hard keep)
//
// References (see resolver) come from every consumer's query: rules,
// panel targets, query variables and annotations. PromQL selectors resolve
// to metric signals (and collected label signals), LogQL selectors to
// stream signals; a recording rule's output is a metric signal, so readers
// of it also reference the rule's inputs, with Via set to the rule.
//
// Two consumers that would share an ID (duplicate alert names in one group,
// repeated panel IDs) get "#2", "#3"… in the order they were collected,
// skipping any ID already taken.
package scan

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// AllStreams is the selector of the signal a LogQL consumer references when
// its stream selectors can't be read, or a templated matcher left none:
// unknown means used, so it stands for every stream in Loki.
const AllStreams = "*"

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
	// Queries are query-log entries in the window, tvt's own and rule
	// evaluations already excluded.
	Queries []collect.LoggedQuery
	// External are the consumers tvt.yaml declares; every signal they
	// select is a hard keep. Owners map signals to owners and tiers.
	External []config.ExternalConsumer
	Owners   []config.Owner
	// SLOs are the SLO definitions read from tvt.yaml's slos.paths;
	// SLOsConfigured says paths were given (an empty list then means none
	// exist, not that tvt didn't look). Every signal an SLI reads is a
	// hard keep.
	SLOs           []collect.SLO
	SLOsConfigured bool
	// Runbooks are what the links in alert annotations declare. A step
	// that runs a query makes its signals hard keeps.
	Runbooks []collect.Runbook
}

// Evidence sources Build adds itself: definitions tvt reads, not usage
// logs, so they're left out of LogCoverage.
const (
	SourceSLOs     = "slo definitions"
	SourceRunbooks = "runbooks"
	// SourceDatasources: the share of queries on datasources tvt parses
	// (collect.QueryLanguage) among every dashboard panel, variable and
	// annotation query and every Grafana-managed rule query.
	SourceDatasources = "datasources"
)

// Output is one scan's graph rows.
type Output struct {
	Signals   []graph.Signal
	Consumers []graph.Consumer
	// References link signals to the consumers whose queries read them,
	// ordered by signal, consumer and via; each key appears once.
	References []graph.Reference
	Costs      []graph.Cost
	Loads      []graph.DashboardLoad
	Sources    []graph.SourceCoverage
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

	res := newResolver(in.MetricsSource, in.Metrics, in.Labels)
	var queries []query
	dsq := &dsQueries{}

	// Rules. A recording rule's output is a metric signal.
	for _, r := range in.Rules {
		kind := graph.ConsumerRecordingRule
		if r.Kind == collect.RuleAlert {
			kind = graph.ConsumerAlertRule
		}
		key := r.Name
		if r.UID != "" {
			key = r.UID // Grafana rule titles needn't be unique; UIDs are
		}
		c := graph.Consumer{
			ID:          ids.unique("rule:" + r.Source + ":" + r.Namespace + "/" + r.Group + "/" + key),
			Kind:        kind,
			Name:        r.Name,
			Expr:        r.Expr,
			EvaluatedBy: r.EvaluatedBy,
			Interval:    r.Interval,
		}
		for _, q := range r.Queries {
			if q.DatasourceUID != "" && !slices.Contains(c.Datasources, q.DatasourceUID) {
				c.Datasources = append(c.Datasources, q.DatasourceUID)
			}
		}
		slices.Sort(c.Datasources)
		if len(r.Queries) > 0 {
			// A Grafana-managed rule: each query goes to its own datasource;
			// one tvt doesn't parse is an assumed reader and a gap.
			var qs []query
			for _, q := range r.Queries {
				qs = append(qs, dsq.query(&c, q.DatasourceType, q.Expr, r.Name))
			}
			for _, q := range qs {
				q.consumer = c
				queries = append(queries, q)
			}
		}
		out.Consumers = append(out.Consumers, c)
		if kind == graph.ConsumerRecordingRule && r.Name != "" {
			res.recordingRule(c.ID, r.Name)
		}
		if len(r.Queries) > 0 {
			continue
		}
		lang := "promql"
		if r.Language == "logql" {
			lang = "logql"
		}
		queries = append(queries, query{consumer: c, expr: r.Expr, lang: lang})
	}

	// External consumers: each selector is a query that reads every label
	// of what it selects.
	for _, e := range in.External {
		c := graph.Consumer{ID: ids.unique("ext:" + e.Name), Kind: graph.ConsumerExternal, Name: e.Name,
			Expr: strings.Join(append(slices.Clone(e.Metrics), e.Streams...), "\n")}
		out.Consumers = append(out.Consumers, c)
		for _, m := range e.Metrics {
			queries = append(queries, query{consumer: c, expr: "{__name__=~" + strconv.Quote(m) + "}", lang: "promql", allLabels: true, includeRecorded: true})
		}
		for _, s := range e.Streams {
			queries = append(queries, query{consumer: c, expr: s, lang: "logql", allLabels: true})
		}
	}

	// SLOs: each SLI query is read like a rule's.
	sloGaps := sloConsumers(in.SLOs, ids, &out, &queries)

	// Runbooks: code blocks and Explore links are queries; a dashboard
	// link stands for every query on that dashboard.
	rbGaps, rbBad := runbookConsumers(in.Runbooks, in.Dashboards, res.metrics, ids, &out, &queries)

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

	// Dashboards: one consumer per panel query, query variable and
	// annotation query.
	for _, d := range in.Dashboards {
		viewed := lastViewed[d.UID]
		vars := dashboardVars(d, res.metrics)
		consume := func(c graph.Consumer, dsType string, variable bool, shows []string) {
			q := dsq.query(&c, dsType, c.Expr, c.Name)
			q.consumer, q.variable, q.vars, q.shows = c, variable, vars, shows
			out.Consumers = append(out.Consumers, c)
			if q.expr != "" {
				queries = append(queries, q)
			}
		}
		for _, p := range d.Panels {
			for n, q := range p.Queries {
				consume(graph.Consumer{
					ID:         ids.unique(fmt.Sprintf("panel:%s:%s:%d:%d", org, d.UID, p.ID, n)),
					Kind:       graph.ConsumerDashboardPanel,
					Name:       d.Title + " / " + p.Title,
					Expr:       q.Expr,
					LastViewed: viewed,
				}, q.DatasourceType, false, shownLabels([]string{q.Legend}, ""))
			}
		}
		for _, v := range d.Variables {
			if v.Type != "query" && v.Type != "" {
				continue // custom, constant, textbox: values, not a query
			}
			consume(graph.Consumer{
				ID:         ids.unique("var:" + org + ":" + d.UID + ":" + v.Name),
				Kind:       graph.ConsumerDashboardVariable,
				Name:       d.Title + " / $" + v.Name,
				Expr:       v.Query,
				LastViewed: viewed,
			}, v.DatasourceType, true, nil)
		}
		for _, a := range d.Annotations {
			consume(graph.Consumer{
				ID:         ids.unique("annot:" + org + ":" + d.UID + ":" + a.Name),
				Kind:       graph.ConsumerDashboardAnnotation,
				Name:       d.Title + " / annotation " + a.Name,
				Expr:       a.Expr,
				LastViewed: viewed,
			}, a.DatasourceType, false, shownLabels([]string{a.TitleFormat, a.TextFormat}, a.TagKeys))
		}
	}

	// Query logs: one consumer per distinct query (per panel, when the log
	// attributes it).
	logged, lq := loggedConsumers(in.Queries, in.Dashboards, in.OrgID, ids)
	out.Consumers = append(out.Consumers, logged...)
	queries = append(queries, lq...)

	// References: every query, then recording-rule chains, then the label
	// signals each metric reference reads.
	for _, q := range queries {
		res.resolve(q)
	}
	res.chains()
	res.labelRefs()
	out.Signals = append(out.Signals, res.newSignals...)
	out.References = res.sorted()
	if err := classify(out.Signals, out.Consumers, out.References, in.Owners, since, in.Start); err != nil {
		return Output{}, err
	}

	// Coverage. SLO definitions and runbooks are sources too, so what tvt
	// couldn't read shows up as a gap; they don't count toward
	// LogCoverage, which is about usage logs.
	sources := slices.Clone(in.Sources)
	sources = append(sources,
		definitionSource(SourceSLOs, in.SLOsConfigured, len(in.SLOs), len(sloGaps), sloGaps),
		definitionSource(SourceRunbooks, len(in.Runbooks) > 0, len(in.Runbooks), rbBad, rbGaps),
		definitionSource(SourceDatasources, dsq.total > 0, dsq.total, dsq.unknown, dsq.gaps()))
	seen := map[string]bool{}
	var sum float64
	var logs int
	for _, s := range sources {
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
		if s.Source == SourceSLOs || s.Source == SourceRunbooks || s.Source == SourceDatasources {
			continue
		}
		sum += s.Coverage
		logs++
	}
	if logs > 0 {
		out.LogCoverage = sum / float64(logs)
	}
	return out, nil
}

// queryLanguage is collect.QueryLanguage with an empty type (a
// datasource tvt couldn't resolve) read as PromQL: a LogQL query fails to
// parse as PromQL and then only resolves against collected metrics, so it
// can't invent signals.
func queryLanguage(dsType string) (string, bool) {
	if dsType == "" {
		return "promql", true
	}
	return collect.QueryLanguage(dsType)
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
