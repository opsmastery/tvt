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
//	qlog:<src>:<hash>                    query-log query: Explore or another API client
//	qlog:<src>:panel:<org>:<uid>:<id>:<hash>   attributed by the log's headers (Mimir)
//	qlog:<src>:match:<consumer>:<hash>   matched to a collected panel, variable, annotation or Grafana-managed rule
//	qlog:<src>:agent:<name>:<hash>       an agent's query
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
	"github.com/Opsmastery-Summit/tvt/pkg/collect/kube"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/propose"
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
	// RuleEvaluations are the datasource rulers' own evaluations from the
	// query logs, with their measured cost, for cost per rule (S6 item 1).
	RuleEvaluations []collect.RuleEvaluation
	// External are the consumers tvt.yaml declares; every signal they
	// select is a hard keep. Owners map signals to owners and tiers.
	External []config.ExternalConsumer
	Owners   []config.Owner
	// Records are tvt.yaml records: (S7 item 2).
	Records config.Records
	// Pipelines are tvt.yaml's pipelines (S6 item 4); LabelMetrics says
	// which values of each label their handles match on each metric
	// carries (nil when not read, with LabelMetricsErr saying why).
	Pipelines       []config.Pipeline
	LabelMetrics    collect.LabelMetrics
	LabelMetricsErr string
	// MetricJobs are the job label values each metric carries (S7 item
	// 3), stored with its pipeline target so tvt propose knows which
	// scrape configs a drop goes into. Nil when not read.
	MetricJobs map[string][]string
	// Rewrites are the name-touching rules read from each pipeline's
	// checkout (S7 item 9), by pipeline; RewriteErrs the files that
	// couldn't be read.
	Rewrites    map[string][]propose.Rewrite
	RewriteErrs []string
	// SelectorMetrics maps a label-only selector (SelectorKey) to the
	// metric names the backend says carry those labels. A selector that
	// names no metric resolves to these instead of every collected metric.
	// Nil or missing keys keep that conservative reading.
	SelectorMetrics map[string][]string
	// Overflow lists metrics with OpenTelemetry overflow series (S6 item
	// 5); OverflowRead says they were looked for, OverflowErr why not.
	Overflow     []collect.Overflow
	OverflowRead bool
	OverflowErr  string
	// Clients identifies query-log clients the logs can't name.
	Clients config.Clients
	// SweepShare overrides DefaultSweepShare when positive.
	SweepShare float64
	// SLOs are the SLO definitions read from tvt.yaml's slos.paths;
	// SLOsConfigured says paths were given (an empty list then means none
	// exist, not that tvt didn't look). Every signal an SLI reads is a
	// hard keep.
	SLOs           []collect.SLO
	SLOsConfigured bool
	// Runbooks are what the links in alert annotations declare. A step
	// that runs a query makes its signals hard keeps.
	Runbooks []collect.Runbook

	// PriceCard prices every metric and label signal; nil leaves
	// MonthlyCost at zero.
	PriceCard *cost.PriceCard
	// Scrape gives per-metric scrape intervals and types (Prometheus); nil
	// when the backend has no targets API (Mimir).
	Scrape *collect.Scrape
	// DefaultInterval is used for a metric no API gives an interval for,
	// recorded as assumed with DefaultIntervalReason.
	DefaultInterval       time.Duration
	DefaultIntervalReason string
	// Storage is what a Prometheus server reports about its TSDB, for a
	// self_hosted price card and the storage hygiene warnings; nil when
	// the backend doesn't (Mimir) or it couldn't be read.
	Storage *collect.Storage
	// VolumeBytes is the size of the volume under that TSDB (tvt.yaml
	// storage.volume_size); 0 when unknown.
	VolumeBytes int64
	// Monitors is which ServiceMonitors and PodMonitors the
	// prometheus-operator servers in tvt.yaml's kubernetes.manifests
	// select; nil when none are configured.
	Monitors *kube.Selection
	// StreamVolumes is every Loki stream's ingest (the volume API); nil
	// when it wasn't collected, which leaves stream signals without costs.
	StreamVolumes []collect.StreamVolume
	// VolumeTruncated: the volume API hit its series limit.
	VolumeTruncated bool
	// Patterns are Loki's line patterns per stream (its canonical full
	// label set), for the line findings; nil when not collected.
	Patterns map[string][]collect.LinePattern
	// Seasons are tvt.yaml seasons: one the window misses is a gap, and
	// its services' unread signals are dormant.
	Seasons []config.Season
	// Mentions are names incident write-ups (postmortems) give without
	// querying them; runbooks' mentions come from Runbooks. An unread
	// metric one names is dormant, not a drop candidate.
	Mentions []Mention
	// History is what earlier scans stored, for scoring; zero for the
	// first scan.
	History History
	// MinWindowDays is tvt.yaml window_days: no candidate gets confidence
	// from a shorter window. Zero means score.DefaultMinWindowDays.
	MinWindowDays int
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
	// SourceSeasonPrefix starts each season's source: "season <name>".
	// A season the window misses has coverage 0 and a gap.
	SourceSeasonPrefix = "season "
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
	// Scores rank the candidate signals, highest priority first.
	Scores []graph.Score
	// PriceCard is the card the costs use (Inputs.PriceCard), for the scan
	// row; nil when none was configured.
	PriceCard *cost.PriceCard
	// Storage is the self-hosted model's resolved inputs; nil for a vendor
	// card.
	Storage *cost.Storage
	// StreamVolumes is every Loki stream's bytes per day, by its full label
	// set, so any selector or label drop can be priced later.
	StreamVolumes   []graph.StreamVolume
	VolumeTruncated bool
	// LineFindings are line patterns worth cutting inside streams (S5
	// items 3-4), largest first.
	LineFindings []graph.LineFinding
	// TSDB is what the Prometheus server reported (Inputs.Storage), stored
	// so the scan can be repriced; nil when it wasn't read.
	TSDB *collect.Storage
	// RuleCosts price every rule's evaluations; RuleFindings are what's
	// wrong with each (S6 item 1). Report only.
	RuleCosts    []graph.RuleCost
	RuleFindings []graph.RuleFinding
	// QueryCosts are each query-log consumer's measured query cost (S6
	// item 2), for the report's query load by client.
	QueryCosts []graph.QueryCost
	// SLOFindings are the SLO coverage findings (S6 item 3). Report only.
	SLOFindings []graph.SLOFinding
	// PipelineTargets map every metric, label and stream signal, and every
	// listed stream, to its pipeline (S6 item 4); nil without pipelines.
	PipelineTargets []graph.PipelineTarget
	// OverflowFindings are the metrics with overflow series (S6 item 5).
	OverflowFindings []graph.OverflowFinding
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
		c := graph.Cost{SignalID: id, ActiveSeries: l.ActiveSeries, DistinctValues: l.Values, ObservedAt: in.Start, Summable: summable(in.Scrape, l.Metric)}
		if l.Series > 0 {
			c.Merges, c.MergesKnown = l.Series-l.SeriesWithout, true
		}
		out.Costs = append(out.Costs, c)
	}

	ids := newIDSet()

	res := newResolver(in.MetricsSource, in.Metrics, in.Labels)
	res.selectorMetrics = in.SelectorMetrics
	if in.SweepShare > 0 {
		res.sweepShare = in.SweepShare
	}
	var queries []query
	dsq := &dsQueries{}

	// Rules. A recording rule's output is a metric signal.
	ruleIDs := make([]string, 0, len(in.Rules))
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
		ruleIDs = append(ruleIDs, c.ID)
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
	sloGaps, sloIDs := sloConsumers(in.SLOs, ids, &out, &queries)
	sloBad := len(sloGaps)
	if in.SLOsConfigured && len(in.Owners) == 0 {
		// SLO coverage (S6 item 3) needs to know the services.
		sloGaps = append(sloGaps, "no ownership.owners in tvt.yaml: services without an SLO can't be listed")
	}

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
	logged, lq, qcosts := loggedConsumers(in.Queries, in.Dashboards, out.Consumers, in.Clients, in.OrgID, ids)
	out.QueryCosts = qcosts
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
	out.Signals = append(out.Signals, unreadStreams(in.StreamVolumes, out.Signals)...)
	if in.StreamVolumes != nil {
		costs, vols, err := streamCosts(in.StreamVolumes, out.Signals, in.Start)
		if err != nil {
			return Output{}, err
		}
		out.Costs, out.StreamVolumes = append(out.Costs, costs...), vols
		out.VolumeTruncated = in.VolumeTruncated
	}
	out.References = res.sorted()
	// A view an earlier scan saw counts after its log line rotates away;
	// the window check in classify and scoring still ages it out. The store
	// keeps the later of the two, so the written row agrees.
	for i := range out.Consumers {
		c := &out.Consumers[i]
		if v, ok := in.History.LastViewed[c.ID]; ok && v.After(c.LastViewed) && !v.After(in.Start) {
			c.LastViewed = v
		}
	}
	seasonSources, missed := seasons(in.Seasons, since, in.Start)
	overlaps := StreamOverlaps(out.Signals, streamLabelSets(in.StreamVolumes))
	if err := classify(out.Signals, out.Consumers, out.References, overlaps, in.Owners, mentioned(in), missed, since, in.Start); err != nil {
		return Output{}, err
	}
	if err := markRecords(out.Signals, in.Records, streamLabelSets(in.StreamVolumes)); err != nil {
		return Output{}, err
	}

	// Coverage. SLO definitions and runbooks are sources too, so what tvt
	// couldn't read shows up as a gap; they don't count toward
	// LogCoverage, which is about usage logs.
	sources := slices.Clone(in.Sources)
	sources = append(sources,
		definitionSource(SourceSLOs, in.SLOsConfigured, len(in.SLOs), sloBad, sloGaps),
		definitionSource(SourceRunbooks, len(in.Runbooks) > 0, len(in.Runbooks), rbBad, rbGaps),
		definitionSource(SourceDatasources, dsq.total > 0, dsq.total, dsq.unknown, dsq.gaps()))
	sources = append(sources, seasonSources...)
	if in.Storage != nil {
		st, _ := StorageHygiene(*in.Storage, in.VolumeBytes)
		sources = append(sources, st)
	}
	var pipeSrc graph.SourceCoverage
	out.PipelineTargets, pipeSrc = pipelineTargets(in, out.Signals, out.Consumers, out.Costs)
	if pipeSrc.Configured {
		sources = append(sources, pipeSrc)
	}
	var overSrc graph.SourceCoverage
	out.OverflowFindings, overSrc = overflowFindings(in, out.References)
	if overSrc.Configured {
		sources = append(sources, overSrc)
	}
	if in.Monitors != nil {
		var pools []string
		if in.Scrape != nil {
			pools = in.Scrape.Pools
		}
		mon, _ := MonitorSelection(*in.Monitors, pools)
		sources = append(sources, mon)
	}
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
		if s.Source == SourceSLOs || s.Source == SourceRunbooks || s.Source == SourceDatasources || s.Source == SourceStorage || s.Source == SourceMonitors || s.Source == SourcePipelines || s.Source == SourceOverflow || s.Source == SourceCloudDirectQueries || strings.HasPrefix(s.Source, SourceSeasonPrefix) {
			continue
		}
		sum += s.Coverage
		logs++
	}
	if logs > 0 {
		out.LogCoverage = sum / float64(logs)
	}
	st, err := priceCosts(in, out.Costs)
	if err != nil {
		return Output{}, err
	}
	out.PriceCard, out.Storage, out.TSDB = in.PriceCard, st, in.Storage
	out.RuleCosts, out.RuleFindings = ruleAnalysis(in, ruleIDs, out.References)
	out.SLOFindings = sloCoverage(in, ruleIDs, sloIDs, out.Signals, out.References)
	readers := newStreamReaders(out.Signals, out.Consumers, out.References)
	if out.LineFindings, err = lineFindings(in.Patterns, out.StreamVolumes, in.Owners, in.PriceCard, readers); err != nil {
		return Output{}, err
	}
	out.Scores = scoreCandidates(in, out.Signals, out.Consumers, out.References, out.Costs, out.Sources, since)
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
