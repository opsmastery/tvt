// Package config reads tvt.yaml.
//
// Decoding is strict: an unknown key is an error, so a typo can't silently
// drop an external consumer and with it a hard keep. Sections that later
// sprints read (backends, price card, pipelines) are accepted but not yet
// interpreted; the CLI still takes backends from flags.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// Config is tvt.yaml.
type Config struct {
	// ExternalConsumers read signals in ways no log shows (remote-write
	// targets, federation, outside tools). Every signal they match is a
	// hard keep.
	ExternalConsumers []ExternalConsumer `yaml:"external_consumers"`
	Ownership         Ownership          `yaml:"ownership"`
	// SLOs are read from files; every signal an SLI reads is a hard keep.
	SLOs SLOs `yaml:"slos"`
	// Runbooks says how to read the runbooks alerts link to.
	Runbooks Runbooks `yaml:"runbooks"`
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
	// WindowDays is the shortest observation window anything is
	// recommended from: a scan with a shorter --window scores every
	// candidate with confidence 0. 0 means the default, 30.
	WindowDays int `yaml:"window_days"`

	// Read by later sprints; accepted so one tvt.yaml serves every command.
	Grafana   yaml.Node `yaml:"grafana"`
	Metrics   yaml.Node `yaml:"metrics"`
	Logs      yaml.Node `yaml:"logs"`
	Pipelines yaml.Node `yaml:"pipelines"`
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
	for i, p := range c.SLOs.Paths {
		if p == "" {
			return fmt.Errorf("slos.paths[%d]: empty path", i)
		}
	}
	for i, p := range c.Postmortems.Paths {
		if p == "" {
			return fmt.Errorf("postmortems.paths[%d]: empty path", i)
		}
	}
	for i, m := range c.Runbooks.Mirrors {
		if m.URLPrefix == "" || m.Path == "" {
			return fmt.Errorf("runbooks.mirrors[%d]: url_prefix and path are required", i)
		}
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
