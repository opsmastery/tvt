// Package graph defines the usage graph: which telemetry signals exist, which
// consumers (alerts, recording rules, dashboards, queries) depend on them, and
// what each signal costs.
package graph

import (
	_ "embed"
	"time"
)

// Schema is the DDL for the usage graph store (pkg/store). It lives here so
// the model and its storage stay in step; this package does no I/O.
//
//go:embed schema.sql
var Schema string

// SignalKind is the type of telemetry a Signal represents.
type SignalKind string

const (
	KindMetric   SignalKind = "metric"         // a metric name
	KindLabel    SignalKind = "label"          // a label on one metric (Parent = metric name)
	KindStream   SignalKind = "stream"         // a log stream, identified by its label set
	KindSpanAttr SignalKind = "span_attribute" // a span attribute on one service (Phase 3)
)

// Signal is a unit of telemetry that can be kept, dropped, downsampled or rerouted.
type Signal struct {
	ID        string
	Kind      SignalKind
	Name      string // metric name, label name, or canonical stream selector
	Parent    string // for KindLabel: the metric the label belongs to
	Source    string // backend the signal was observed in, e.g. "prom", "loki"
	FirstSeen time.Time
	// Class is the scan's classification (score.Class: hard_keep,
	// keep_high, dormant or candidate); empty for rows from before schema
	// version 6. Dormant says why an unread signal looks incident-critical.
	Class   string
	Dormant string
	// Owner decides about the signal (tvt.yaml ownership), empty when
	// unknown; Tier is its service tier, nil when unknown.
	Owner string
	Tier  *int
}

// ConsumerKind is the type of thing that depends on a signal.
type ConsumerKind string

const (
	ConsumerAlertRule      ConsumerKind = "alert_rule"
	ConsumerRecordingRule  ConsumerKind = "recording_rule"
	ConsumerSLO            ConsumerKind = "slo"
	ConsumerDashboardPanel ConsumerKind = "dashboard_panel"
	// ConsumerDashboardVariable is a dashboard's query variable
	// (label_values() and friends). It weighs like its dashboard's panels:
	// a variable can be the only thing that reads a label.
	ConsumerDashboardVariable ConsumerKind = "dashboard_variable"
	// ConsumerDashboardAnnotation is a dashboard annotation query. Viewers
	// can toggle a disabled one on, so it counts whether enabled or not.
	ConsumerDashboardAnnotation ConsumerKind = "dashboard_annotation"
	ConsumerAdhocQuery          ConsumerKind = "adhoc_query"
	ConsumerAgentQuery          ConsumerKind = "agent_query"
	ConsumerExternal            ConsumerKind = "external" // declared by the user in tvt.yaml
	// ConsumerRunbook is an alert's runbook (or a Grafana link in its
	// annotations) whose steps run queries: the responder depends on them.
	ConsumerRunbook ConsumerKind = "runbook"
)

// HardKeep reports whether a dependency of this kind always protects a
// signal, regardless of cost. Recording rules are only hard keeps when they
// transitively feed an alert or SLO; that is resolved in the graph, not here.
func (k ConsumerKind) HardKeep() bool {
	switch k {
	case ConsumerAlertRule, ConsumerSLO, ConsumerExternal, ConsumerRunbook:
		return true
	}
	return false
}

// Evidence is the class of evidence a consumer's references carry. It
// decides how a reference weighs over time: declared and incident evidence
// never decays, observed evidence does (Sprint 3's scoring).
func (k ConsumerKind) Evidence() EvidenceClass {
	switch k {
	case ConsumerAlertRule, ConsumerRecordingRule, ConsumerSLO, ConsumerExternal, ConsumerRunbook:
		return EvidenceDeclared
	}
	return EvidenceObserved
}

// EvidenceClass is how a reference was established.
type EvidenceClass string

const (
	// EvidenceDeclared: something evaluates the signal by definition (an
	// alert, a recording rule, an SLO, a runbook step, a declared external
	// consumer).
	EvidenceDeclared EvidenceClass = "declared"
	// EvidenceIncident: the signal was read during an incident (reserved
	// for Phase 2's incident timelines).
	EvidenceIncident EvidenceClass = "incident"
	// EvidenceObserved: someone was seen using it (dashboard views, query
	// logs). The only class that decays.
	EvidenceObserved EvidenceClass = "observed"
)

// Valid reports whether e is one of the three classes.
func (e EvidenceClass) Valid() bool {
	return e == EvidenceDeclared || e == EvidenceIncident || e == EvidenceObserved
}

// Decays reports whether evidence of this class loses weight with age.
func (e EvidenceClass) Decays() bool { return e == EvidenceObserved }

// LabelUse is how a query uses a label.
type LabelUse string

const (
	LabelMatch   LabelUse = "match"   // in a selector's matchers
	LabelGroup   LabelUse = "group"   // by(...), on(...), group_left/right, or read by a variable
	LabelWithout LabelUse = "without" // without(...) or ignoring(...): dropping it changes the result silently
	LabelCreated LabelUse = "created" // written by label_replace, label_join or count_values
	// LogQL only: LabelFilter is read by the pipeline and can only be a
	// stream label; LabelExtracted is read by the pipeline and may come
	// from a parser stage (a stream label only if the stream has it).
	LabelFilter    LabelUse = "filter"
	LabelExtracted LabelUse = "extracted"
	// LabelLegend is shown by a panel's legend ({{label}}): dropping it
	// leaves the query working but changes what the viewer sees.
	LabelLegend LabelUse = "legend"
)

// LabelRef is one label a reference reads, and how.
type LabelRef struct {
	Name string
	Use  LabelUse
}

// Consumer is something that reads telemetry.
type Consumer struct {
	ID         string
	Kind       ConsumerKind
	Name       string    // rule name, "dashboard / panel", query source
	Expr       string    // the query or rule expression, shown as evidence
	Team       string    // owning team, when known
	LastViewed time.Time // dashboards only; zero if unknown
	// Rules only: EvaluatedBy is "grafana" when Grafana evaluates the rule
	// (and sends its query to the datasource every Interval), empty when
	// the datasource's ruler does. Interval is zero when unknown.
	// Datasources are the UIDs a Grafana-managed rule queries, sorted.
	EvaluatedBy string
	Interval    time.Duration
	Datasources []string
	// Unparsed says why tvt couldn't read the consumer's query (a panel
	// on a datasource type it doesn't parse, such as Pyroscope); empty
	// when it could. Its references are assumed, and a consumer with
	// Unparsed set reads unknown signals: it never makes its dashboard
	// look unused.
	Unparsed string
	// Client is who sent a query-log consumer's query: dashboard, rule,
	// explore, agent or api (scan.Client*). Empty for collected consumers.
	Client string
}

// Reference links a signal to a consumer that depends on it.
type Reference struct {
	SignalID   string
	ConsumerID string
	LastSeen   time.Time
	// FirstSeen is when tvt first saw the reference: the start of the
	// first scan that stored it. Set by the store when reading; ignored
	// when writing.
	FirstSeen time.Time
	Actor     string // user or agent that issued the query, when known
	Via       string // recording rule ID when the dependency is transitive
	// Evidence is the class of the consumer's evidence (see
	// ConsumerKind.Evidence). Required.
	Evidence EvidenceClass
	// Labels are the labels the consumer reads on the signal, sorted by
	// name and use. For a reference through a recording rule (Via) they
	// include the labels the rule reads and those its reader reads on the
	// rule's output, since those can come from the input.
	Labels []LabelRef
	// AllLabels: the consumer may depend on every label of the signal, so
	// dropping any label can change its result (a vector match without
	// on(...), a label name from a variable, a query that didn't parse).
	AllLabels bool
	// ResultLabels: the signal's labels survive into the consumer's result
	// (no aggregation or join removes them), so the panel, alert or caller
	// receives every one, as in up{job="shop"}. It is not a read: the
	// labels aren't added to Labels. Classification protects every label
	// of the metric when an alert (any hard-keep consumer) or a dashboard
	// viewed in the window keeps them in its result, since removing one
	// merges series and changes what fires or shows (scan.classify).
	// False when any label is only summed away, as in sum by (job) (up).
	ResultLabels bool
	// Assumed means the reference is a guess rather than read exactly from
	// the query: a variable, a pattern that couldn't be resolved, a query
	// that didn't parse. Reasons says why. Unknown means used, so an
	// assumed reference still protects the signal.
	Assumed bool
	Reasons []string
	// Sweep: the consumer read the signal only as part of a query over
	// every metric name or most of the series ({__name__=~".+"}, an
	// agent's "what's here"). Weak evidence: it never keeps a signal on
	// its own. The same consumer naming the signal makes it a read.
	Sweep bool
}

// Cost is the observed volume of a signal and what it costs per month.
type Cost struct {
	SignalID     string
	ScanID       int64 // the scan that observed it; set by the store
	ActiveSeries int64 // metrics and labels
	// DistinctValues is a label's number of distinct values: what dropping
	// it collapses. Zero for other kinds.
	DistinctValues int64
	BytesPerDay    int64   // log streams
	SpansPerDay    int64   // span attributes
	MonthlyCost    float64 // in the price card's currency
	ObservedAt     time.Time
	// Interval is the scrape (or rule evaluation) interval a metric's
	// samples are counted at; IntervalAssumed says why when no API gave it.
	Interval        time.Duration
	IntervalAssumed string
	// Buckets is a histogram _bucket metric's bucket count (its le values);
	// Exemplars is true when exemplar storage is on for it.
	Buckets   int64
	Exemplars bool
	// DiskBytes is what a metric (or a label's share) holds on disk over
	// retention, replicas included; set only by a self_hosted price card.
	DiskBytes int64
}

// Score is a candidate signal's rank in a scan (score.Score): its monthly
// cost divided by one plus its usage weight, times the confidence that the
// evidence would have shown every reader.
type Score struct {
	SignalID    string
	MonthlyCost float64
	UsageWeight float64
	// Strongest names the evidence the weight came from (score.Usage's
	// String: none, sweep, adhoc_query…).
	Strongest  string
	Confidence float64
	Priority   float64
	// Held says why Confidence is 0, when it is: "window" (the scan's
	// window is shorter than window_days), "coverage" (no usage log could
	// have shown the signal read) or "new" (it first appeared in this scan).
	Held string
}

// StreamVolume is one Loki stream's ingest in a scan: its full label set as
// a canonical selector, and its bytes per day.
type StreamVolume struct {
	Selector    string
	BytesPerDay int64
}

// Line finding kinds (S5 items 3-4). tvt reports both with an estimated
// saving and never proposes a line-level change itself (ADR 0006).
const (
	// LineRepeated: one identical line (no variable fields) is a large
	// share of a stream's bytes.
	LineRepeated = "repeated_line"
	// LineHandoff: the dominant unread pattern of a kept stream, handed to
	// zeroreads for the proven line-level cut.
	LineHandoff = "line_handoff"
)

// LineFinding is a line pattern inside one stream that is worth cutting:
// its stream's full label set, the pattern (Loki's, <_> for each variable
// field), how many lines matched it over Window, its estimated share of the
// stream's bytes, and those bytes per day and their monthly cost.
type LineFinding struct {
	Stream      string
	Pattern     string
	Kind        string
	Lines       int64
	Window      time.Duration
	Share       float64
	BytesPerDay int64
	MonthlyCost float64
}

// PerSecond is the pattern's average line rate over its window.
func (f LineFinding) PerSecond() float64 {
	if f.Window <= 0 {
		return 0
	}
	return float64(f.Lines) / f.Window.Seconds()
}

// RuleCost is what one rule's evaluations cost (S6 item 1): evaluations
// per day from its interval, and the samples (PromQL) or bytes (LogQL)
// each evaluation read, measured from the query logs. EvalsPerDay is 0
// when the interval is unknown; the per-evaluation figures are 0 when no
// evaluation was logged. Unknown says why the daily cost is unknown, never
// zero.
type RuleCost struct {
	ConsumerID     string
	EvalsPerDay    float64
	SamplesPerEval float64
	BytesPerEval   float64
	// Evaluations is how many logged evaluations the means come from.
	Evaluations int64
	Unknown     string
}

// SamplesPerDay and BytesPerDay are the daily cost; 0 when unknown.
func (c RuleCost) SamplesPerDay() float64 { return c.EvalsPerDay * c.SamplesPerEval }
func (c RuleCost) BytesPerDay() float64   { return c.EvalsPerDay * c.BytesPerEval }

// QueryCost is the measured cost of one query-log consumer's queries over
// a scan's window (S6 item 2): how many were logged, and the samples
// (PromQL) and bytes (LogQL) they read, as the backends report them. A
// query matching several collected panels or rules is split evenly among
// them, so costs add up to the log's total.
type QueryCost struct {
	ConsumerID string
	Queries    float64
	Samples    float64
	Bytes      float64
}

// Rule finding kinds (S6 item 1). Report only: tvt never edits rules.
const (
	RuleGrafanaManaged     = "grafana_managed_alert"        // Grafana evaluates an alert on Loki or Prometheus
	RuleParsesRawLogs      = "logql_parses_raw_logs"        // a LogQL alert parses raw lines every interval
	RuleNoLineFilter       = "no_line_filter_before_parser" // a parser runs on every line of the stream
	RuleLongRange          = "long_range_window"            // a range far longer than the interval
	RuleIntervalBelowScrap = "interval_below_scrape"        // evaluated more often than its metrics are scraped
	RuleDuplicate          = "duplicate_rule"               // same expression as another rule of the ruler
	RuleUnreadRecording    = "unread_recording_rule"        // a recording rule nothing reads
	RuleNoSeries           = "no_series"                    // reads only metrics with no series: broken, not unused
	RuleGroupSlow          = "group_slower_than_interval"   // its group takes longer than its interval
)

// RuleFinding is one inefficiency or fault of a rule, with its fix.
type RuleFinding struct {
	ConsumerID string
	Kind       string
	Detail     string
	Fix        string
}

// SLO coverage finding kinds (S6 item 3).
const (
	SLOServiceWithout    = "service_without_slo"
	SLOPagingNotBurnRate = "paging_not_burn_rate"
	SLOWithoutAlert      = "slo_without_alert"
)

// SLOFinding is one SLO coverage finding (S6 item 3). Subject is the
// alert's or SLO's consumer ID, or "service:<owner>" for a service.
type SLOFinding struct {
	Kind    string
	Subject string
	Name    string
	Detail  string
	Fix     string
}

// DashboardLoad aggregates loads of one dashboard by one login in a scan's
// window. Browser loads (from the dashboard page) are views; API loads by
// anyone but tvt are recorded as evidence of use but not counted as views.
type DashboardLoad struct {
	OrgID        int64
	DashboardUID string
	Login        string
	Browser      bool
	Loads        int64
	LastAt       time.Time
}

// SourceCoverage is how complete one usage-evidence source was for a scan's
// window. A source that wasn't configured has Configured false and
// Coverage 0, so a missing log shows up in the report rather than hiding in
// an average.
type SourceCoverage struct {
	Source      string // e.g. "prometheus query log", "grafana router log"
	Configured  bool
	Coverage    float64 // 0..1
	WindowStart time.Time
	// Gap says why the source can't be trusted for the window, when it
	// can't: a query log whose probe marker never appeared (Coverage is
	// then 0). Empty when there is no gap or the source isn't configured.
	Gap string
}
