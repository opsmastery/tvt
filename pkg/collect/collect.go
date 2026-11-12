// Package collect defines the read-only interfaces tvt uses to gather usage
// evidence and volume from observability backends. Each backend lives in its
// own subpackage (grafana/, prometheus/, mimir/, loki/) and implements the
// interfaces it can support.
//
// Implementations must be read-only, must not log credentials, and should be
// testable against recorded API responses in testdata/.
package collect

import (
	"context"
	"time"
)

// CardinalitySource reports active series per metric and per label.
// Prometheus: /api/v1/status/tsdb. Mimir: the cardinality analysis API.
type CardinalitySource interface {
	MetricCardinality(ctx context.Context) ([]MetricCardinality, error)
	LabelCardinality(ctx context.Context, metric string) ([]LabelCardinality, error)
}

// RuleSource lists alerting and recording rules (Prometheus, Mimir and Loki rules APIs).
type RuleSource interface {
	Rules(ctx context.Context) ([]Rule, error)
}

// DashboardSource lists dashboards with their panel queries and, where the
// Grafana edition exposes it, when they were last viewed.
type DashboardSource interface {
	Dashboards(ctx context.Context) ([]Dashboard, error)
}

// ViewSource reports dashboard loads, for Grafana editions without usage
// insights (OSS: the router log's GET /api/dashboards/uid/:uid lines).
type ViewSource interface {
	DashboardViews(ctx context.Context, since time.Time) ([]DashboardView, error)
	// Coverage reports how complete the log is for the window, 0..1.
	Coverage(ctx context.Context, since time.Time) (float64, error)
}

// QueryLogSource returns queries observed since a point in time
// (Prometheus query log, Mimir/Loki query-frontend logs, agent audit logs).
type QueryLogSource interface {
	Queries(ctx context.Context, since time.Time) ([]LoggedQuery, error)
	// Coverage reports how complete the log is for the window, 0..1.
	Coverage(ctx context.Context, since time.Time) (float64, error)
}

// StreamVolumeSource reports ingested bytes per log stream (Loki volume API).
type StreamVolumeSource interface {
	StreamVolumes(ctx context.Context, window time.Duration) ([]StreamVolume, error)
}

// MetricCardinality is the number of active series for a metric name.
type MetricCardinality struct {
	Metric       string
	ActiveSeries int64
}

// LabelCardinality is the number of distinct values of a label on a metric
// and how many series carry it.
type LabelCardinality struct {
	Metric       string
	Label        string
	Values       int64
	ActiveSeries int64
}

// RuleKind distinguishes alerting from recording rules.
type RuleKind string

const (
	RuleAlert  RuleKind = "alert"
	RuleRecord RuleKind = "record"
)

// Rule is one alerting or recording rule.
type Rule struct {
	Source    string // "prometheus", "mimir", "loki"
	Namespace string
	Group     string
	Name      string // alert name, or the recorded metric name
	Kind      RuleKind
	Expr      string
	Language  string // "promql" or "logql"
}

// Dashboard is a Grafana dashboard.
type Dashboard struct {
	UID       string
	Title     string
	Folder    string
	Panels    []Panel
	Variables []Variable
	// Annotations are the dashboard's annotation queries against a backend
	// (built-in Grafana annotations are left out).
	Annotations []Annotation
	LastViewed  time.Time // zero when the edition doesn't expose view stats
}

// Panel is one dashboard panel and the queries behind it.
type Panel struct {
	ID      int
	Title   string
	Queries []PanelQuery
}

// PanelQuery is one query (target) in a panel.
//
// DatasourceUID "$name" means a datasource template variable: the query can
// run against any datasource of DatasourceType, so it references all of them.
type PanelQuery struct {
	DatasourceUID  string
	DatasourceType string // "prometheus", "loki", ...
	Expr           string
	// Legend is the target's legendFormat: {{label}} templates name the
	// labels the panel shows.
	Legend string
}

// Variable is a dashboard template variable. A query variable's query can
// reference signals too; the others are collected for the values a
// matcher like {route=~"$route"} can take.
type Variable struct {
	Name           string
	Type           string // "query", "custom", "constant", "textbox", ...
	Query          string
	DatasourceUID  string
	DatasourceType string
	// Values are the values a custom or constant variable offers. Empty for
	// other types, whose values come from a backend or the viewer.
	Values []string
	// IncludeAll: the variable has an "All" option. AllValue is its custom
	// value (often ".*"), which can match values Values doesn't list.
	IncludeAll bool
	AllValue   string
	// Multi: several values can be selected at once. Grafana's Prometheus
	// and Loki datasources regex-escape the values of a Multi or IncludeAll
	// variable by default.
	Multi bool
}

// Annotation is one annotation query on a dashboard.
type Annotation struct {
	Name           string
	Expr           string
	DatasourceUID  string
	DatasourceType string
	Enabled        bool
	// TitleFormat and TextFormat are {{label}} templates; TagKeys is a
	// comma-separated list of label names shown as tags.
	TitleFormat, TextFormat, TagKeys string
}

// DashboardView is one load of a dashboard's JSON.
type DashboardView struct {
	DashboardUID string
	OrgID        int64 // dashboard UIDs are only unique within an organization
	User         string
	At           time.Time
	// Browser is true when the load came from the dashboard page in a
	// browser (Referer is the dashboard URL). False means an API client:
	// a script, a provisioning tool, or a browser with referrers disabled.
	Browser bool
}

// LoggedQuery is one query seen in a query log.
type LoggedQuery struct {
	Source       string // backend the log came from
	Expr         string
	Language     string // "promql", "logql", "traceql"
	At           time.Time
	Actor        string // user, service account or agent, when known
	Client       string // client IP or user agent, when the log records one
	DashboardUID string // when the request carried dashboard attribution
	PanelID      int
	OrgID        int64 // Grafana organization of DashboardUID, when logged
}

// StreamVolume is ingested bytes for one log stream over a window.
type StreamVolume struct {
	Labels map[string]string
	Bytes  int64
	Window time.Duration
}
