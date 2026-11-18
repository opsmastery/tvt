// Package config reads tvt.yaml.
//
// Decoding is strict: an unknown key is an error, so a typo can't silently
// drop an external consumer and with it a hard keep. Sections that later
// sprints read (backends) are accepted but not yet interpreted; the CLI still takes backends from flags.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/prometheus/common/model"
)

// Config is tvt.yaml.
type Config struct {
	// ExternalConsumers read signals in ways no log shows (remote-write
	// targets, federation, outside tools). Every signal they match is a
	// hard keep.
	ExternalConsumers []ExternalConsumer `yaml:"external_consumers"`
	Ownership         Ownership          `yaml:"ownership"`
	// Queue caps what tvt puts in front of each owner (S9 item 6).
	Queue Queue `yaml:"queue"`
	// Detection sets detection objectives (S8 item 5).
	Detection Detection `yaml:"detection"`
	// Records are signals tvt never drops, samples or collapses (S7 item 2,
	// ADR 0011), on top of the ones named like a record (audit, deploy and
	// change events).
	Records Records `yaml:"records"`
	// SLOs are read from files; every signal an SLI reads is a hard keep.
	SLOs SLOs `yaml:"slos"`
	// Runbooks says how to read the runbooks alerts link to.
	Runbooks Runbooks `yaml:"runbooks"`
	// Seasons are readers that only run in some months (a quarter-end
	// close, a holiday peak). A window that misses every month of a
	// season can't show its readers: that is a gap, and the unread signals
	// of the season's services are dormant, not drop candidates.
	Seasons []Season `yaml:"seasons"`
	// Gaps acknowledges evidence gaps: an acknowledged gap leaves the
	// report's rows (it stays in the header with its reason); an
	// unacknowledged one shows on every row it affects.
	Gaps Gaps `yaml:"gaps"`
	// Postmortems are incident write-ups: a metric one names is expected
	// in incidents, so if nothing reads it, it is dormant (ask the owner),
	// never a drop candidate.
	Postmortems Postmortems `yaml:"postmortems"`
	// PriceCard is a price card file (docs/pricecard.md), relative to
	// tvt.yaml, or the name of a shipped preset such as grafana-cloud.
	PriceCard string `yaml:"pricecard"`
	// Clients says who sent logged queries where the log can't (the
	// Prometheus query log records only an IP).
	Clients Clients `yaml:"clients"`
	// SweepShare: an observed query that selects more than this share of
	// the series, naming no metric and restricting no label, is a sweep
	// (weak evidence). 0 means the default, 0.5.
	SweepShare float64 `yaml:"sweep_share"`
	// Storage describes the metrics backend's disk, for storage hygiene
	// warnings (S5 item 6).
	Storage Storage `yaml:"storage"`
	// Kubernetes points at manifests (helm template output, a GitOps
	// checkout) so tvt knows which ServiceMonitors and PodMonitors a
	// prometheus-operator Prometheus selects (S5 item 8).
	Kubernetes Kubernetes `yaml:"kubernetes"`
	// WindowDays is the shortest observation window anything is
	// recommended from: a scan with a shorter --window scores every
	// candidate with confidence 0. 0 means the default, 30.
	WindowDays int `yaml:"window_days"`

	// Read by later sprints; accepted so one tvt.yaml serves every command.
	Grafana yaml.Node `yaml:"grafana"`
	Metrics yaml.Node `yaml:"metrics"`
	Logs    yaml.Node `yaml:"logs"`
	// Pipelines are where signals are shaped (ADR 0007, S6 item 4): each
	// signal maps to the one pipeline whose handles match its labels.
	Pipelines []Pipeline `yaml:"pipelines"`
}

// Pipeline kinds (ADR 0007).
var PipelineKinds = []string{"prometheus", "alloy", "otelcol", "fluentbit", "prometheus-operator", "exporter"}

// Pipeline is one place signals are ingested and shaped (ADR 0007).
type Pipeline struct {
	Name   string `yaml:"name"`
	Kind   string `yaml:"kind"`
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
	// Files to change, or HelmValues when the config is rendered from Helm.
	Files      []string    `yaml:"files"`
	HelmValues *HelmValues `yaml:"helm_values"`
	// Handles are label selectors ({job=~"shop|node"}) for the metric
	// series and log streams this pipeline ingests.
	Handles   []string `yaml:"handles"`
	Reviewers []string `yaml:"reviewers"`
	// Checkout is a local checkout of Repo (relative to tvt.yaml): tvt
	// scan reads the pipeline's rewrites from Files there (S7 item 9).
	Checkout string `yaml:"checkout"`
	// Build is the ocb builder manifest of a custom Collector (otelcol
	// pipelines), a path in the repo: tvt propose adds the components it
	// writes to it (S7 item 4).
	Build string `yaml:"build"`
	// Version is the Collector version the pipeline runs (otelcol
	// pipelines, "0.165.0"): it picks the OTTL form tvt writes (S7 item 7).
	Version string `yaml:"version"`
	// Cold is the alias of the output that takes streams tvt reroutes to
	// cold storage (fluentbit pipelines): one the config already defines.
	Cold string `yaml:"cold"`
	// MonitorJobs names the monitor (Kind/namespace/name) for a job tvt
	// can't work out from the manifests (prometheus-operator pipelines: a
	// ServiceMonitor whose Service isn't in them, such as the apiserver's).
	MonitorJobs map[string]string `yaml:"monitor_jobs"`
	// Replicas is how many Collector replicas run this config (otelcol
	// pipelines; default 1), and LoadBalanced says a load-balancing
	// exporter in front routes each series to one replica. A delta-counter
	// label removal needs one Collector to see every session (S8 item
	// 12): with several replicas and no load balancing, tvt doesn't
	// propose it and check refuses it.
	Replicas     int  `yaml:"replicas"`
	LoadBalanced bool `yaml:"load_balanced"`
}

// Detection: worst-case detection delay objectives by alert severity
// ("critical": "15m"), and Alertmanager's group_wait (default 30s).
type Detection struct {
	Objectives map[string]string `yaml:"objectives"`
	GroupWait  string            `yaml:"group_wait"`
}

// Records declares records: metrics entries are anchored regexes, streams
// entries stream selectors (as ownership's).
type Records struct {
	Metrics []string `yaml:"metrics"`
	Streams []string `yaml:"streams"`
	// Spans and LogEvents are OTTL conditions over a span or a log record
	// (S8 item 13): span.attributes["gen_ai.operation.name"] ==
	// "deploy". tvt check never lets a pipeline drop, sample or strip the
	// outcome fields of one. Agent tool calls are built in.
	Spans     []string `yaml:"spans"`
	LogEvents []string `yaml:"log_events"`
}

// HelmValues points at the Helm values key a pipeline's config renders from.
type HelmValues struct {
	File string `yaml:"file"`
	Path string `yaml:"path"`
	// Defaults is the chart's own values.yaml (helm show values) at the
	// deployed version, a path in the repo: Helm replaces lists, so a new
	// list key in File starts with the chart's default rules from it.
	Defaults string `yaml:"defaults"`
}

// Clients identifies query-log clients.
type Clients struct {
	// Grafana is the addresses (IPs or CIDRs) Grafana's queries come from,
	// as the backends see them. Without it, an address that sent a query
	// matching a panel or Grafana-managed rule is taken to be Grafana's.
	Grafana []string `yaml:"grafana"`
	// Agents are AI agents and other automated clients, by name.
	Agents []Agent `yaml:"agents"`
}

// Agent is one automated client. A logged query is the agent's when any
// of its identifiers matches.
type Agent struct {
	Name string `yaml:"name"`
	// UserAgent is a regular expression over the logged user agent
	// (Mimir), unanchored.
	UserAgent string `yaml:"user_agent"`
	// Addresses are IPs or CIDRs (the Prometheus query log's client IP).
	Addresses []string `yaml:"addresses"`
	// Actors are logins or service-account names its token belongs to, as
	// forwarded in X-Grafana-User.
	Actors []string `yaml:"actors"`
}

// ExternalConsumer is one consumer declared by the user.
type ExternalConsumer struct {
	Name string `yaml:"name"`
	// Metrics are regular expressions over metric names, anchored at both
	// ends as in PromQL: "billing_.*" matches billing_total, not x_billing.
	Metrics []string `yaml:"metrics"`
	// Streams are LogQL stream selectors, such as {component="audit"}.
	Streams []string `yaml:"streams"`
}

// SLOs lists where SLO definitions live.
type SLOs struct {
	// Paths are files or directories (walked recursively) holding Sloth,
	// Pyrra or OpenSLO YAML. A repository is a path to its checkout.
	// Relative paths are relative to tvt.yaml.
	Paths []string `yaml:"paths"`
}

// Season is a recurring period with its own readers.
type Season struct {
	Name string `yaml:"name"`
	// Months are the months of the year it runs in, 1-12.
	Months []int `yaml:"months"`
	// Services are ownership.owners names whose signals it reads.
	Services []string `yaml:"services"`
}

// Gaps lists acknowledged evidence gaps.
type Gaps struct {
	Acknowledge []GapAck `yaml:"acknowledge"`
}

// GapAck accepts the gaps of one evidence source (as the report names it:
// "loki query log", "runbooks", "datasources", "assumed references"…)
// whose text contains Match (all of them when Match is empty). Reason is
// required: it is shown wherever the gap would have been.
type GapAck struct {
	Source string `yaml:"source"`
	Match  string `yaml:"match"`
	Reason string `yaml:"reason"`
}

// Kubernetes lists manifests tvt reads from disk; it never talks to a
// cluster.
type Kubernetes struct {
	// Manifests are files or directories (.yaml, .yml, .json), relative to
	// tvt.yaml.
	Manifests []string `yaml:"manifests"`
	// Namespace is the namespace of objects that don't name one (helm
	// template's --namespace); empty means "default".
	Namespace string `yaml:"namespace"`
}

// Storage describes the disk under the Prometheus TSDB.
type Storage struct {
	// VolumeSize is the size of the volume holding the TSDB (a PVC's
	// requested storage, or what df reports): bytes, or a number with a
	// unit. Ki, Mi, Gi, Ti, Pi and KiB…PiB are powers of 1024; K, M, G,
	// T, P and KB…PB powers of 1000. Empty: unknown.
	VolumeSize string `yaml:"volume_size"`
}

// VolumeBytes is VolumeSize in bytes; 0 when it isn't set.
func (s Storage) VolumeBytes() (int64, error) {
	if strings.TrimSpace(s.VolumeSize) == "" {
		return 0, nil
	}
	return ParseSize(s.VolumeSize)
}

var sizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMGTP]?)(i?)(B?)$`)

// ParseSize reads a byte size: "53687091200", "50Gi", "50GiB", "500G",
// "1.5TB". A bare B is bytes; a unit with i is binary, without it decimal.
func ParseSize(v string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil || (m[2] == "" && m[3] != "") {
		return 0, fmt.Errorf("size %q: want bytes or a number with a unit such as 50Gi or 500GB", v)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", v, err)
	}
	base := 1000.0
	if m[3] == "i" {
		base = 1024
	}
	if m[2] != "" {
		n *= math.Pow(base, float64(strings.Index("KMGTP", m[2])+1))
	}
	if n < 1 || n > math.MaxInt64/2 {
		return 0, fmt.Errorf("size %q: out of range", v)
	}
	return int64(n), nil
}

// Postmortems lists where incident write-ups live.
type Postmortems struct {
	// Paths are files or directories (walked recursively; .md, .markdown,
	// .txt, .html and .htm files are read). Relative paths are relative
	// to tvt.yaml.
	Paths []string `yaml:"paths"`
}

// Runbooks says how tvt reads alert runbooks. A link is read from the
// first mirror whose url_prefix it starts with, else fetched over HTTP GET
// when Fetch is true; anything else is a recorded gap.
type Runbooks struct {
	Fetch   bool            `yaml:"fetch"`
	Mirrors []RunbookMirror `yaml:"mirrors"`
}

// RunbookMirror maps links under URLPrefix to files under Path (a
// checked-out runbook repo). Relative paths are relative to tvt.yaml.
type RunbookMirror struct {
	URLPrefix string `yaml:"url_prefix"`
	Path      string `yaml:"path"`
}

// Ownership says who owns which signals.
type Ownership struct {
	// Labels whose values name a team (team, service, namespace). Read when
	// series label values are collected; Sprint 2 records them only.
	Labels []string `yaml:"labels"`
	// Owners map signals to an owner, first match wins.
	Owners []Owner `yaml:"owners"`
	// The other owner sources (S9 item 1), tried in this order after
	// Owners: CODEOWNERS files, the Labels above on the signal's series,
	// and Backstage catalog files (catalog-info.yaml, or directories
	// searched for it). A component is named by a path's first segment
	// (CODEOWNERS) or metadata.name (Backstage), and a signal belongs to
	// it through its job, app or service_name label, or a metric name
	// prefix. Paths are relative to tvt.yaml.
	CODEOWNERS []string `yaml:"codeowners"`
	Backstage  []string `yaml:"backstage"`
	// Fallbacks when no source names an owner, marked inferred: Services
	// maps an OTel service.name (or service.namespace/service.name) to an
	// owner, and Kubernetes manifests give a workload's namespace team
	// label (any of Labels) or its app.kubernetes.io/part-of through
	// Services.
	Services   map[string]string `yaml:"services"`
	Kubernetes []string          `yaml:"kubernetes"`
	// GitHub maps an owner to the GitHub user or team (@user,
	// @org/team) tvt propose requests a review from (S9 item 2). An owner
	// that is already a handle (@name) needs no entry.
	GitHub map[string]string `yaml:"github"`
}

// Owner owns the signals its selectors match.
type Owner struct {
	Name string `yaml:"name"`
	// Tier is the service tier (0 most critical); nil when not given.
	// Unread signals of tier 0 and 1 are dormant, never drop candidates.
	Tier    *int     `yaml:"tier"`
	Metrics []string `yaml:"metrics"` // anchored regular expressions
	Streams []string `yaml:"streams"` // LogQL stream selectors
}

// Load reads and validates tvt.yaml at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	for i, p := range c.SLOs.Paths {
		c.SLOs.Paths[i] = relTo(dir, p)
	}
	for i, m := range c.Runbooks.Mirrors {
		c.Runbooks.Mirrors[i].Path = relTo(dir, m.Path)
	}
	for i, p := range c.Postmortems.Paths {
		c.Postmortems.Paths[i] = relTo(dir, p)
	}
	for i, p := range c.Kubernetes.Manifests {
		c.Kubernetes.Manifests[i] = relTo(dir, p)
	}
	for _, l := range []*[]string{&c.Ownership.CODEOWNERS, &c.Ownership.Backstage, &c.Ownership.Kubernetes} {
		for i, p := range *l {
			(*l)[i] = relTo(dir, p)
		}
	}
	for i, p := range c.Pipelines {
		if p.Checkout != "" {
			c.Pipelines[i].Checkout = relTo(dir, p.Checkout)
		}
	}
	if isPath(c.PriceCard) {
		c.PriceCard = relTo(dir, c.PriceCard)
	}
	return c, nil
}

// Parse decodes and validates a tvt.yaml document.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("expected a single YAML document")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	for sev, o := range c.Detection.Objectives {
		if _, err := model.ParseDuration(o); err != nil {
			return fmt.Errorf("detection.objectives.%s: %q isn't a duration (15m, 1h)", sev, o)
		}
	}
	if g := c.Detection.GroupWait; g != "" {
		if _, err := model.ParseDuration(g); err != nil {
			return fmt.Errorf("detection.group_wait: %q isn't a duration", g)
		}
	}
	for _, list := range []struct {
		key   string
		conds []string
	}{{"records.spans", c.Records.Spans}, {"records.log_events", c.Records.LogEvents}} {
		for i, s := range list.conds {
			if _, err := parse.OTTLCondition(s); err != nil {
				return fmt.Errorf("%s[%d]: %v", list.key, i, err)
			}
		}
	}
	seen := map[string]bool{}
	for i, e := range c.ExternalConsumers {
		if e.Name == "" {
			return fmt.Errorf("external_consumers[%d]: name is required", i)
		}
		if seen[e.Name] {
			return fmt.Errorf("external_consumers: %q appears twice", e.Name)
		}
		seen[e.Name] = true
		if len(e.Metrics) == 0 && len(e.Streams) == 0 {
			return fmt.Errorf("external consumer %q: needs metrics or streams", e.Name)
		}
		if err := checkSelectors("external consumer "+e.Name, e.Metrics, e.Streams); err != nil {
			return err
		}
	}
	for i, o := range c.Ownership.Owners {
		if o.Name == "" {
			return fmt.Errorf("ownership.owners[%d]: name is required", i)
		}
		if o.Tier != nil && *o.Tier < 0 {
			return fmt.Errorf("owner %q: tier %d, want 0 or more", o.Name, *o.Tier)
		}
		if err := checkSelectors("owner "+o.Name, o.Metrics, o.Streams); err != nil {
			return err
		}
	}
	if c.Queue.PerOwner < 0 {
		return fmt.Errorf("queue.per_owner %d: want 0 (the default) or more", c.Queue.PerOwner)
	}
	for svc, owner := range c.Ownership.Services {
		if svc == "" || owner == "" {
			return fmt.Errorf("ownership.services: %q: service and owner are both required", svc)
		}
	}
	for _, l := range [][]string{c.Ownership.CODEOWNERS, c.Ownership.Backstage, c.Ownership.Kubernetes} {
		for _, p := range l {
			if p == "" {
				return fmt.Errorf("ownership: an empty path")
			}
		}
	}
	for i, p := range c.SLOs.Paths {
		if p == "" {
			return fmt.Errorf("slos.paths[%d]: empty path", i)
		}
	}
	pipelines := map[string]bool{}
	for i, p := range c.Pipelines {
		if p.Name == "" {
			return fmt.Errorf("pipelines[%d]: name is required", i)
		}
		if pipelines[p.Name] {
			return fmt.Errorf("pipelines: %q appears twice", p.Name)
		}
		pipelines[p.Name] = true
		if !slices.Contains(PipelineKinds, p.Kind) {
			return fmt.Errorf("pipeline %q: kind %q, want one of %s", p.Name, p.Kind, strings.Join(PipelineKinds, ", "))
		}
		if len(p.Handles) == 0 {
			return fmt.Errorf("pipeline %q: handles is required (the label selectors of the signals it ingests)", p.Name)
		}
		for _, h := range p.Handles {
			if _, err := StreamMatchers(h); err != nil {
				return fmt.Errorf("pipeline %q: handles %q: %w", p.Name, h, err)
			}
		}
		// helm_values.path is where the config sits in the values: required
		// for a collector (its config key); for a chart whose own keys tvt
		// writes (kube-prometheus-stack, fluent/fluent-bit) it's empty, or
		// the subchart's key in an umbrella chart.
		if p.HelmValues != nil && (p.HelmValues.File == "" || (p.Kind == "otelcol" && p.HelmValues.Path == "")) {
			return fmt.Errorf("pipeline %q: helm_values needs file (and path, the collector config's key)", p.Name)
		}
		// A prometheus-operator pipeline may list both: files are the
		// chart's rendered monitors, which say each job's monitor, and
		// helm_values is where tvt writes.
		if p.HelmValues != nil && len(p.Files) > 0 && p.Kind != "prometheus-operator" {
			return fmt.Errorf("pipeline %q: files or helm_values, not both (only a prometheus-operator pipeline lists both: the rendered monitors and the values)", p.Name)
		}
	}
	for i, p := range c.Postmortems.Paths {
		if p == "" {
			return fmt.Errorf("postmortems.paths[%d]: empty path", i)
		}
	}
	for i, expr := range c.Records.Metrics {
		if _, err := MetricRegexp(expr); err != nil {
			return fmt.Errorf("records.metrics[%d]: %w", i, err)
		}
	}
	for i, sel := range c.Records.Streams {
		if _, err := StreamMatchers(sel); err != nil {
			return fmt.Errorf("records.streams[%d] %q: %w", i, sel, err)
		}
	}
	owners := map[string]bool{}
	for _, o := range c.Ownership.Owners {
		owners[o.Name] = true
	}
	seasons := map[string]bool{}
	for i, se := range c.Seasons {
		if strings.TrimSpace(se.Name) == "" {
			return fmt.Errorf("seasons[%d]: name is required", i)
		}
		if seasons[se.Name] {
			return fmt.Errorf("seasons[%d]: %q appears twice", i, se.Name)
		}
		seasons[se.Name] = true
		if len(se.Months) == 0 || len(se.Services) == 0 {
			return fmt.Errorf("season %q: months and services are required", se.Name)
		}
		for _, m := range se.Months {
			if m < 1 || m > 12 {
				return fmt.Errorf("season %q: month %d, want 1-12", se.Name, m)
			}
		}
		for _, svc := range se.Services {
			// A typo would silently protect nothing: services must be owners.
			if !owners[svc] {
				return fmt.Errorf("season %q: service %q is not an ownership.owners name", se.Name, svc)
			}
		}
	}
	for i, a := range c.Gaps.Acknowledge {
		if strings.TrimSpace(a.Source) == "" || strings.TrimSpace(a.Reason) == "" {
			return fmt.Errorf("gaps.acknowledge[%d]: source and reason are required", i)
		}
	}
	for i, m := range c.Runbooks.Mirrors {
		if m.URLPrefix == "" || m.Path == "" {
			return fmt.Errorf("runbooks.mirrors[%d]: url_prefix and path are required", i)
		}
	}
	if _, err := c.Storage.VolumeBytes(); err != nil {
		return fmt.Errorf("storage.volume_size: %w", err)
	}
	if c.WindowDays < 0 {
		return fmt.Errorf("window_days %d: want a number of days", c.WindowDays)
	}
	if c.SweepShare < 0 || c.SweepShare > 1 || c.SweepShare != c.SweepShare {
		return fmt.Errorf("sweep_share %v: want a share between 0 and 1", c.SweepShare)
	}
	for _, a := range c.Clients.Grafana {
		if _, err := ParseAddress(a); err != nil {
			return fmt.Errorf("clients.grafana: %w", err)
		}
	}
	agents := map[string]bool{}
	for i, a := range c.Clients.Agents {
		if a.Name == "" {
			return fmt.Errorf("clients.agents[%d]: name is required", i)
		}
		if agents[a.Name] {
			return fmt.Errorf("clients.agents: %q appears twice", a.Name)
		}
		agents[a.Name] = true
		if a.UserAgent == "" && len(a.Addresses) == 0 && len(a.Actors) == 0 {
			return fmt.Errorf("agent %q: needs user_agent, addresses or actors", a.Name)
		}
		if a.UserAgent != "" {
			if _, err := regexp.Compile(a.UserAgent); err != nil {
				return fmt.Errorf("agent %q: user_agent: %w", a.Name, err)
			}
		}
		for _, ad := range a.Addresses {
			if _, err := ParseAddress(ad); err != nil {
				return fmt.Errorf("agent %q: %w", a.Name, err)
			}
		}
	}
	return nil
}

// ParseAddress reads an IP or a CIDR as a prefix (an IP is a /32 or /128).
func ParseAddress(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("address %q: not an IP or CIDR", s)
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// isPath reports whether a pricecard value names a file rather than a
// preset: it has a directory separator or a YAML extension.
func isPath(v string) bool {
	return strings.ContainsAny(v, `/\`) || strings.HasSuffix(v, ".yaml") || strings.HasSuffix(v, ".yml")
}

func relTo(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

func checkSelectors(who string, metrics, streams []string) error {
	for _, m := range metrics {
		if _, err := MetricRegexp(m); err != nil {
			return fmt.Errorf("%s: metrics %q: %w", who, m, err)
		}
	}
	for _, s := range streams {
		if _, err := StreamMatchers(s); err != nil {
			return fmt.Errorf("%s: streams %q: %w", who, s, err)
		}
	}
	return nil
}

// MetricRegexp compiles a metrics entry, anchored as PromQL anchors regex
// matchers.
func MetricRegexp(expr string) (*regexp.Regexp, error) {
	return regexp.Compile("^(?:" + expr + ")$")
}

// StreamMatchers parses a streams entry: exactly one stream selector, with
// at least one matcher.
func StreamMatchers(sel string) ([]parse.Matcher, error) {
	refs, err := parse.LogQL(sel)
	if err != nil {
		return nil, err
	}
	if len(refs) != 1 || len(refs[0].Matchers) == 0 {
		return nil, fmt.Errorf("want one stream selector with at least one matcher")
	}
	for _, m := range refs[0].Matchers {
		if m.Op == parse.MatchRegexp || m.Op == parse.MatchNotRegex {
			if _, err := regexp.Compile("^(?:" + m.Value + ")$"); err != nil {
				return nil, fmt.Errorf("label %s: %w", m.Label, err)
			}
		}
	}
	return refs[0].Matchers, nil
}

// Queue caps the recommendations in front of one owner at a time (S9 item
// 6): tvt propose writes at most PerOwner of an owner's open
// recommendations per run, highest savings per minute of checking first,
// and the weekly digest lists the same. Zero: propose writes them all and
// the digest lists report.DefaultQueue.
type Queue struct {
	PerOwner int `yaml:"per_owner"`
}
