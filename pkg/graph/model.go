// Package graph defines the usage graph: which telemetry signals exist, which
// consumers (alerts, recording rules, dashboards, queries) depend on them, and
// what each signal costs.
package graph

import (
	_ "embed"
	"time"
)

// Schema is the SQLite DDL for the usage graph store.
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
	ConsumerAdhocQuery     ConsumerKind = "adhoc_query"
	ConsumerAgentQuery     ConsumerKind = "agent_query"
	ConsumerExternal       ConsumerKind = "external" // declared by the user in tvt.yaml
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
}

// Cost is the observed volume of a signal and what it costs per month.
type Cost struct {
	SignalID     string
	ActiveSeries int64   // metrics and labels
	BytesPerDay  int64   // log streams
	SpansPerDay  int64   // span attributes
	MonthlyCost  float64 // in the price card's currency
	ObservedAt   time.Time
}
