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
	ConsumerAdhocQuery        ConsumerKind = "adhoc_query"
	ConsumerAgentQuery        ConsumerKind = "agent_query"
	ConsumerExternal          ConsumerKind = "external" // declared by the user in tvt.yaml
)

// HardKeep reports whether a dependency of this kind always protects a
// signal, regardless of cost. Recording rules are only hard keeps when they
// transitively feed an alert or SLO; that is resolved in the graph, not here.
func (k ConsumerKind) HardKeep() bool {
	switch k {
	case ConsumerAlertRule, ConsumerSLO, ConsumerExternal:
		return true
	}
	return false
}

// Consumer is something that reads telemetry.
type Consumer struct {
	ID         string
	Kind       ConsumerKind
	Name       string    // rule name, "dashboard / panel", query source
	Expr       string    // the query or rule expression, shown as evidence
	Team       string    // owning team, when known
	LastViewed time.Time // dashboards only; zero if unknown
}

// Reference links a signal to a consumer that depends on it.
type Reference struct {
	SignalID   string
	ConsumerID string
	LastSeen   time.Time
	Actor      string // user or agent that issued the query, when known
	Via        string // recording rule ID when the dependency is transitive
	// Assumed means the reference was inferred without fully parsing the
	// consumer's query (for example a LogQL rule before the LogQL extractor
	// exists). It covers every label of the signal: unknown means used.
	Assumed bool
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
}
