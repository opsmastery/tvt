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
}

// Reference links a signal to a consumer that depends on it.
type Reference struct {
	SignalID   string
	ConsumerID string
	LastSeen   time.Time
	Actor      string // user or agent that issued the query, when known
	Via        string // recording rule ID when the dependency is transitive
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
	// labels aren't added to Labels, and whether dropping one is safe (do
	// series merge? does the alert's identity change?) is left to the
	// analysis that proposes the drop. False when any label is only summed
	// away, as in sum by (job) (up).
	ResultLabels bool
	// Assumed means the reference is a guess rather than read exactly from
	// the query: a variable, a pattern that couldn't be resolved, a query
	// that didn't parse. Reasons says why. Unknown means used, so an
	// assumed reference still protects the signal.
	Assumed bool
	Reasons []string
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
