package grafana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
)

// rulesPath is Grafana's ruler config API for Grafana-managed rules. Unlike
// the Prometheus-compatible view (/api/prometheus/grafana/api/v1/rules),
// which flattens a rule to one query string, and the provisioning API,
// which leaves out the group interval, it carries every query with its
// datasource plus the interval in one read.
const rulesPath = "/api/ruler/grafana/api/v1/rules"

type rawRuleGroup struct {
	Name     string    `json:"name"`
	Interval string    `json:"interval"`
	Rules    []rawRule `json:"rules"`
}

type rawRule struct {
	For          string            `json:"for"`
	Annotations  map[string]string `json:"annotations"`
	Labels       map[string]string `json:"labels"`
	GrafanaAlert struct {
		UID             string `json:"uid"`
		Title           string `json:"title"`
		NamespaceUID    string `json:"namespace_uid"`
		RuleGroup       string `json:"rule_group"`
		IntervalSeconds int64  `json:"intervalSeconds"`
		Record          *struct {
			Metric string `json:"metric"`
			From   string `json:"from"`
		} `json:"record"`
		Data []struct {
			RefID         string          `json:"refId"`
			DatasourceUID string          `json:"datasourceUid"`
			Model         json.RawMessage `json:"model"`
		} `json:"data"`
	} `json:"grafana_alert"`
}

// Rules returns the Grafana-managed alert and recording rules in the
// token's organization. Grafana evaluates these itself, querying the
// datasource on every interval, so the datasource's own ruler never sees
// them: a metric only a Grafana alert reads looks unused to every other
// source. Each rule carries its folder UID as Namespace, its UID, its
// interval and one query per datasource query (expressions such as reduce
// and threshold read no signal and are left out); Expr is the queries
// joined, as evidence.
//
// A token without alert.rules:read (403), a Grafana with unified alerting
// off (404), a null listing, or a Prometheus or Loki query with no readable
// expression is an error, not zero rules: a rule tvt can't read might be
// the only thing protecting a signal.
func (c *Client) Rules(ctx context.Context) ([]collect.Rule, error) {
	var folders map[string][]rawRuleGroup
	if err := c.getJSON(ctx, rulesPath, nil, &folders); err != nil {
		var he *httpError
		if errors.As(err, &he) && (he.Status == http.StatusForbidden || he.Status == http.StatusUnauthorized) {
			return nil, fmt.Errorf("%w; tvt's service account needs alert.rules:read (the Viewer role has it)", err)
		}
		return nil, err
	}
	if folders == nil {
		return nil, fmt.Errorf("grafana: %s: response is not a rule listing (null)", rulesPath)
	}
	var dss []datasource
	listed, needList := false, false
	for _, groups := range folders {
		for _, g := range groups {
			for _, r := range g.Rules {
				if len(r.GrafanaAlert.Data) > 0 {
					needList = true
				}
			}
		}
	}
	if needList {
		var err error
		if dss, listed, err = c.datasources(ctx); err != nil {
			return nil, err
		}
	}
	res := &resolver{dss: dss, listed: listed}

	var out []collect.Rule
	for _, groups := range folders {
		for _, g := range groups {
			groupInterval, _ := time.ParseDuration(g.Interval)
			for _, raw := range g.Rules {
				a := raw.GrafanaAlert
				r := collect.Rule{
					Source:      "grafana",
					Namespace:   a.NamespaceUID,
					Group:       a.RuleGroup,
					Name:        a.Title,
					UID:         a.UID,
					Kind:        collect.RuleAlert,
					EvaluatedBy: collect.EvaluatedByGrafana,
					Interval:    time.Duration(a.IntervalSeconds) * time.Second,
				}
				if len(raw.Annotations) > 0 {
					r.Annotations = raw.Annotations
				}
				if d, err := time.ParseDuration(raw.For); err == nil {
					r.For = d
				}
				if len(raw.Labels) > 0 {
					r.Labels = raw.Labels
				}
				if r.Group == "" {
					r.Group = g.Name
				}
				if r.Interval == 0 {
					r.Interval = groupInterval
				}
				if a.Record != nil && a.Record.Metric != "" {
					r.Kind, r.Name = collect.RuleRecord, a.Record.Metric
				}
				var exprs []string
				for _, d := range a.Data {
					ds := res.lookup(d.DatasourceUID, "")
					if ds.skip || ds.mixed {
						continue
					}
					if ds.needsList {
						return nil, fmt.Errorf("rule %q (%s): datasource %q %w", a.Title, a.UID, d.DatasourceUID, errNoDatasourceList)
					}
					expr := modelExpr(d.Model)
					if strings.TrimSpace(expr) == "" {
						if _, parsed := collect.QueryLanguage(ds.typ); parsed {
							return nil, fmt.Errorf("rule %q (%s): query %q for datasource %q has an empty or unreadable model", a.Title, a.UID, d.RefID, d.DatasourceUID)
						}
						if !keepEmpty(ds.typ) {
							continue
						}
					}
					r.Queries = append(r.Queries, collect.PanelQuery{DatasourceUID: ds.uid, DatasourceType: ds.typ, Expr: expr})
					exprs = append(exprs, d.RefID+": "+expr)
				}
				if len(exprs) == 1 {
					r.Expr = r.Queries[0].Expr
				} else {
					r.Expr = strings.Join(exprs, "\n")
				}
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.UID < b.UID
	})
	return out, nil
}

// modelExpr is a rule query's text: expr for Prometheus and Loki, else a
// string query field.
func modelExpr(raw json.RawMessage) string {
	var m struct {
		Expr          string          `json:"expr"`
		Query         json.RawMessage `json:"query"`
		ProfileTypeID string          `json:"profileTypeId"`
		LabelSelector string          `json:"labelSelector"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if m.Expr != "" {
		return m.Expr
	}
	if q := stringOrQuery(m.Query); q != "" {
		return q
	}
	if m.ProfileTypeID != "" {
		return m.ProfileTypeID + m.LabelSelector // Pyroscope's own query syntax
	}
	return ""
}
