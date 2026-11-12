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
	"os"
	"regexp"

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

	// Read by later sprints; accepted so one tvt.yaml serves every command.
	Grafana    yaml.Node `yaml:"grafana"`
	Metrics    yaml.Node `yaml:"metrics"`
	Logs       yaml.Node `yaml:"logs"`
	Pricecard  yaml.Node `yaml:"pricecard"`
	WindowDays yaml.Node `yaml:"window_days"`
	Pipelines  yaml.Node `yaml:"pipelines"`
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
	return nil
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
