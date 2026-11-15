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
	"errors"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/parse"
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

// SLOSource lists service level objectives (Sloth, Pyrra and OpenSLO
// files; Grafana SLO).
type SLOSource interface {
	SLOs(ctx context.Context) ([]SLO, error)
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
// When a response hits the source's series limit it returns what it has and
// an error wrapping ErrTruncated: totals are then low.
type StreamVolumeSource interface {
	StreamVolumes(ctx context.Context, window time.Duration) ([]StreamVolume, error)
}

// ErrTruncated means a collector hit a server-side limit and its result is
// incomplete.
var ErrTruncated = errors.New("result truncated at the server's limit")

// ScrapeSource reports how metrics are scraped: per-metric intervals
// (samples per month are a cost input), metric types (a histogram's buckets
// multiply its series) and whether exemplars are stored (Prometheus targets,
// targets/metadata and flags APIs).
type ScrapeSource interface {
	Scrape(ctx context.Context) (Scrape, error)
}

// StorageSource reports what a self-hosted TSDB stores, from the server's
// own metrics (no query: tvt never runs PromQL against a backend).
type StorageSource interface {
	Storage(ctx context.Context) (Storage, error)
}

// Storage is a TSDB's on-disk footprint and limits.
type Storage struct {
	// BytesPerSample is compacted chunk bytes over samples since the
	// server started (prometheus_tsdb_compaction_chunk_{size_bytes,
	// samples}_sum); 0 before its first compaction.
	BytesPerSample float64
	HeadSeries     int64
	// HeadBytes is the WAL plus the head chunks on disk.
	HeadBytes   int64
	BlocksBytes int64
	// RetentionTime and RetentionBytes are the effective limits; zero when
	// unset.
	RetentionTime  time.Duration
	RetentionBytes int64
}

// Scrape is how a backend's metrics are scraped.
type Scrape struct {
	// Intervals maps a metric family name, as target metadata reports it
	// (shop_http_requests, node_cpu_seconds_total), to the shortest scrape
	// interval of the jobs exposing it. Look up series names with Family.
	Intervals map[string]time.Duration
	// Types maps a family name to counter, gauge, histogram, summary,
	// unknown.
	Types map[string]string
	// JobIntervals is each job's scrape interval.
	JobIntervals map[string]time.Duration
	// Pools are the scrape pools of the active targets, sorted.
	// prometheus-operator names a monitor's pools
	// serviceMonitor/<ns>/<name>/<endpoint> (podMonitor/... likewise).
	Pools []string
	// Exemplars is true when exemplar storage is on: histogram series then
	// carry exemplars, which cost extra on most backends.
	Exemplars bool
}

// Family returns the metadata family a series name belongs to and whether
// one was found: the name itself, else the name without a suffix that
// exposition adds (_total, _bucket, _sum, _count, _created, _info).
func (s Scrape) Family(series string) (string, bool) {
	if _, ok := s.Types[series]; ok {
		return series, true
	}
	for _, suf := range []string{"_total", "_bucket", "_sum", "_count", "_created", "_info"} {
		if f, ok := strings.CutSuffix(series, suf); ok {
			if _, ok := s.Types[f]; ok {
				return f, true
			}
		}
	}
	return "", false
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
	Source    string // "prometheus", "mimir", "loki", "grafana"
	Namespace string // Grafana: the folder UID
	Group     string
	Name      string // alert name, or the recorded metric name
	Kind      RuleKind
	Expr      string
	Language  string // "promql" or "logql"
	// UID identifies a Grafana-managed rule; titles needn't be unique.
	UID string
	// Interval is how often the rule is evaluated, when known.
	Interval time.Duration
	// EvaluatedBy is EvaluatedByGrafana when Grafana evaluates the rule and
	// sends its queries to the datasource on every interval; empty when
	// the datasource's own ruler does.
	EvaluatedBy string
	// Queries are a Grafana-managed rule's datasource queries, each with
	// its datasource (expressions left out). When set, they are what the
	// rule reads, and Expr and Language are evidence only.
	Queries []PanelQuery
	// Annotations are an alert's annotations (runbook_url, summary, …):
	// a runbook or Grafana link there can declare more dependencies.
	Annotations map[string]string
}

// EvaluatedByGrafana marks a rule Grafana evaluates itself.
const EvaluatedByGrafana = "grafana"

// SLO is one service level objective. Whatever its SLI reads is a hard
// keep: the SLO and its burn-rate alerts are evaluated from it.
type SLO struct {
	Format string // "sloth", "pyrra", "openslo", "grafana"
	Name   string // service/name where the format has a service
	// Origin is where it was read: a file path, or an API.
	Origin string
	// Queries are the SLI's queries. Empty with Unreadable set when tvt
	// can't tell what the SLI reads (a Sloth plugin SLI, an OpenSLO metric
	// source other than Prometheus or Loki): an evidence gap.
	Queries    []SLOQuery
	Unreadable string
}

// SLOQuery is one query an SLI evaluates.
type SLOQuery struct {
	Expr     string
	Language string // "promql" or "logql"
}

// Runbook is what one link in alert annotations declares: the query steps
// in the page it points to, or the Grafana Explore or dashboard link
// itself. Every step that runs a query is a declared dependency.
type Runbook struct {
	URL string
	// Alerts names the rules that link to it, as "source/name".
	Alerts []string
	Steps  []parse.RunbookStep
	// Mentions are the metric-shaped names anywhere in its text
	// (parse.Mentions); empty for a Grafana link, which has no text.
	Mentions []string
	// Err says why it couldn't be read; Steps is then empty.
	Err string
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

// StreamVolume is ingested bytes for one log stream (its full label set)
// over a window.
type StreamVolume struct {
	Labels map[string]string
	Bytes  int64
	Window time.Duration
}

// PatternSource lists the line patterns in one log stream (Loki's pattern
// ingester, /loki/api/v1/patterns). Loki doesn't log these calls as queries,
// so reading them is not usage.
type PatternSource interface {
	Patterns(ctx context.Context, selector string, window time.Duration) ([]LinePattern, error)
}

// LinePattern is one pattern the pattern ingester found in a stream: its
// text, with <_> for each variable field, and how many lines matched it
// over the window.
type LinePattern struct {
	Pattern string
	Lines   int64
	// Window is the time the counts cover: from the first sample to the
	// last plus one step. Zero when there were no samples.
	Window time.Duration
}
