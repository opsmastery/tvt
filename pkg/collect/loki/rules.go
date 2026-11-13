package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"go.yaml.in/yaml/v3"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

const rulesPath = "/loki/api/v1/rules"

// promRulesPath is Loki's Prometheus-format rules view. Unlike the YAML
// listing it reports each group's effective interval, the ruler's
// evaluation_interval included.
const promRulesPath = "/prometheus/api/v1/rules"

// maxRulesBody caps the rules response. Loki's ruler config for a large
// tenant is a few MB; anything far beyond that is not a rules listing.
const maxRulesBody = 64 << 20

// ruleGroup is one group in Loki's rules listing (Prometheus rule-file
// format). Fields tvt doesn't use (for, labels) are ignored.
type ruleGroup struct {
	Name     string `yaml:"name"`
	Interval string `yaml:"interval"` // empty: the ruler's evaluation_interval
	Rules    []struct {
		Alert  string `yaml:"alert"`
		Record string `yaml:"record"`
		Expr   string `yaml:"expr"`
		// Annotations: alerting rules only (runbook_url and friends).
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"rules"`
}

// Rules returns every alerting and recording rule from /loki/api/v1/rules,
// sorted by namespace, then in listing order.
//
// Loki answers 404 "no rule groups found" when the ruler has no rules; that
// is zero rules, not an error. Any other 404 (the ruler disabled, a wrong
// base URL) is ErrNotFound. A rule that is neither or both an alert and a
// recording rule, or that has no expression, is an error rather than being
// skipped: a rule tvt can't classify might be an alert, and silently
// dropping it could leave a hard keep unprotected.
func (c *Client) Rules(ctx context.Context) ([]collect.Rule, error) {
	body, err := c.get(ctx, rulesPath, nil, "application/yaml")
	if err != nil {
		var he *httpError
		if errors.As(err, &he) && he.Status == http.StatusNotFound && strings.Contains(he.Body, "no rule groups found") {
			return nil, nil
		}
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxRulesBody+1))
	if err != nil {
		return nil, fmt.Errorf("loki: %s: read: %w", rulesPath, err)
	}
	if len(raw) > maxRulesBody {
		return nil, fmt.Errorf("loki: %s: response larger than %d bytes", rulesPath, maxRulesBody)
	}

	var byNS map[string][]ruleGroup
	if err := yaml.Unmarshal(raw, &byNS); err != nil {
		return nil, fmt.Errorf("loki: %s: decode: %w", rulesPath, err)
	}
	if byNS == nil {
		// An empty body or YAML null decodes without error. Loki says "no
		// rules" with a 404 (handled above) or {}; anything else could be a
		// broken proxy, and reading it as zero rules would drop every alert's
		// protection from the scan.
		return nil, fmt.Errorf("loki: %s: decode: response is not a rule listing (empty or null)", rulesPath)
	}
	namespaces := make([]string, 0, len(byNS))
	for ns := range byNS {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	var effective map[[2]string]time.Duration // fetched once, if a group needs it
	var out []collect.Rule
	for _, ns := range namespaces {
		for _, g := range byNS[ns] {
			if g.Name == "" {
				return nil, fmt.Errorf("loki: %s: namespace %q: group without a name", rulesPath, ns)
			}
			var interval time.Duration
			if g.Interval != "" {
				d, err := model.ParseDuration(g.Interval)
				if err != nil {
					return nil, fmt.Errorf("loki: %s: namespace %q group %q: interval: %w", rulesPath, ns, g.Name, err)
				}
				interval = time.Duration(d)
			} else {
				if effective == nil {
					effective = c.effectiveIntervals(ctx)
				}
				interval = effective[[2]string{ns, g.Name}]
			}
			for i, r := range g.Rules {
				var kind collect.RuleKind
				var name string
				switch {
				case r.Alert != "" && r.Record == "":
					kind, name = collect.RuleAlert, r.Alert
				case r.Record != "" && r.Alert == "":
					kind, name = collect.RuleRecord, r.Record
				default:
					return nil, fmt.Errorf("loki: %s: namespace %q group %q rule %d: want exactly one of alert or record", rulesPath, ns, g.Name, i+1)
				}
				if strings.TrimSpace(r.Expr) == "" {
					return nil, fmt.Errorf("loki: %s: namespace %q group %q rule %q: empty expr", rulesPath, ns, g.Name, name)
				}
				rule := collect.Rule{
					Source:    "loki",
					Namespace: ns,
					Group:     g.Name,
					Name:      name,
					Kind:      kind,
					Expr:      r.Expr,
					Language:  "logql",
					Interval:  interval,
				}
				if len(r.Annotations) > 0 {
					rule.Annotations = r.Annotations
				}
				out = append(out, rule)
			}
		}
	}
	return out, nil
}

// effectiveIntervals reads each group's effective interval, keyed by
// namespace and group, from Loki's Prometheus-format rules view. It is only
// a cost input: when the view can't be read the intervals stay unknown
// (zero), and the rules themselves still come from the YAML listing.
func (c *Client) effectiveIntervals(ctx context.Context) map[[2]string]time.Duration {
	out := map[[2]string]time.Duration{}
	body, err := c.get(ctx, promRulesPath, nil, "application/json")
	if err != nil {
		return out
	}
	defer body.Close()
	var d struct {
		Data struct {
			Groups []struct {
				Name     string  `json:"name"`
				File     string  `json:"file"`
				Interval float64 `json:"interval"`
			} `json:"groups"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(body, maxRulesBody)).Decode(&d) != nil {
		return out
	}
	for _, g := range d.Data.Groups {
		out[[2]string{g.File, g.Name}] = time.Duration(g.Interval * float64(time.Second))
	}
	return out
}
