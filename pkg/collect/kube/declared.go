package kube

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/grafana"
)

// Labels kube-prometheus-stack's Grafana sidecars look for (the chart's
// sidecar.dashboards.label and sidecar.datasources.label defaults).
const (
	DashboardLabel  = "grafana_dashboard"
	DatasourceLabel = "grafana_datasource"
)

// Rules turns every prometheus-operator PrometheusRule into rules, as the
// Prometheus rules API would list them once loaded: Source "prometheus",
// Namespace the object's namespace/name, the group's interval when set
// (zero: the server's evaluation_interval, unknown here). A rule without
// alert or record, or without expr, or a group interval tvt can't parse,
// is an error: a declared reader tvt can't read must not vanish.
//
// Every PrometheusRule counts, selected by a server's ruleSelector or
// not: counting one too many over-protects, which is the safe side.
func Rules(objs []Object) ([]collect.Rule, error) {
	var out []collect.Rule
	for _, o := range objs {
		if o.Kind != "PrometheusRule" || !isOperatorAPI(o.APIVersion) {
			continue
		}
		groups, _ := o.Spec["groups"].([]any)
		for gi, g := range groups {
			gm, _ := g.(map[string]any)
			name, _ := scalar(gm["name"])
			if name == "" {
				return nil, fmt.Errorf("%s (%s): group %d has no name", o.Ref(), o.File, gi)
			}
			var interval time.Duration
			if s, ok := scalar(gm["interval"]); ok && s != "" {
				d, err := model.ParseDuration(s)
				if err != nil {
					return nil, fmt.Errorf("%s: group %s: interval %q: %w", o.Ref(), name, s, err)
				}
				interval = time.Duration(d)
			}
			rules, _ := gm["rules"].([]any)
			for ri, r := range rules {
				rm, _ := r.(map[string]any)
				alert, _ := scalar(rm["alert"])
				record, _ := scalar(rm["record"])
				expr, _ := scalar(rm["expr"])
				rule := collect.Rule{Source: "prometheus", Namespace: o.Ref(), Group: name, Expr: strings.TrimSpace(expr), Language: "promql",
					Interval: interval, Labels: stringMap(rm["labels"]), Annotations: stringMap(rm["annotations"])}
				switch {
				case alert != "" && record == "":
					rule.Kind, rule.Name = collect.RuleAlert, alert
				case record != "" && alert == "":
					rule.Kind, rule.Name = collect.RuleRecord, record
				default:
					return nil, fmt.Errorf("%s: group %s: rule %d needs exactly one of alert and record", o.Ref(), name, ri)
				}
				if rule.Expr == "" {
					return nil, fmt.Errorf("%s: group %s: rule %s has no expr", o.Ref(), name, rule.Name)
				}
				out = append(out, rule)
			}
		}
	}
	return out, nil
}

// Dashboards reads the dashboards Grafana's sidecar would load: every
// .json key of a ConfigMap labelled DashboardLabel, with datasources
// resolved against the provisioning files in ConfigMaps labelled
// DatasourceLabel (none: datasources a dashboard leaves to the default
// are an error, as with the API). A dashboard tvt can't read is an error.
func Dashboards(objs []Object) ([]collect.Dashboard, error) {
	var dss []grafana.Datasource
	for _, o := range objs {
		if o.Kind != "ConfigMap" || o.Labels[DatasourceLabel] == "" {
			continue
		}
		for _, k := range sortedKeys(o.Data) {
			d, err := grafana.ParseDatasources([]byte(o.Data[k]))
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", o.Ref(), k, err)
			}
			dss = append(dss, d...)
		}
	}
	var out []collect.Dashboard
	for _, o := range objs {
		if o.Kind != "ConfigMap" || o.Labels[DashboardLabel] == "" {
			continue
		}
		for _, k := range sortedKeys(o.Data) {
			if !strings.HasSuffix(k, ".json") {
				continue
			}
			d, err := grafana.ParseDashboard([]byte(o.Data[k]), "", dss)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", o.Ref(), k, err)
			}
			if d.UID == "" {
				return nil, fmt.Errorf("%s %s: dashboard has no uid", o.Ref(), k)
			}
			out = append(out, d)
		}
	}
	return out, nil
}

// Drop is one metricRelabelings rule with action drop on a ServiceMonitor
// or PodMonitor endpoint: series it matches are dropped at scrape and
// never stored.
type Drop struct {
	Monitor  string // kind/namespace/name
	Endpoint string // port and path, as written
	// Labels are the source labels, joined with Separator before matching.
	Labels    []string
	Separator string
	Regex     string
	re        *regexp.Regexp
}

// Whole is true when the drop removes whole metrics: it matches on
// __name__ alone, so a name it matches has no series left at all.
func (d Drop) Whole() bool { return len(d.Labels) == 1 && d.Labels[0] == "__name__" }

// DropsMetric reports whether a whole-metric drop removes name.
func (d Drop) DropsMetric(name string) bool { return d.Whole() && d.re.MatchString(name) }

// Drops lists the drop rules of every selected monitor's endpoints (only
// selected ones: an unselected monitor scrapes nothing). Prometheus
// anchors relabel regexes; an invalid one is an error.
func Drops(objs []Object, sel Selection) ([]Drop, error) {
	selected := map[string]bool{}
	for _, m := range sel.Monitors {
		if m.Selected() {
			selected[m.Ref()] = true
		}
	}
	var out []Drop
	for _, o := range objs {
		field := map[string]string{KindServiceMonitor: "endpoints", KindPodMonitor: "podMetricsEndpoints"}[o.Kind]
		if field == "" || !isOperatorAPI(o.APIVersion) {
			continue
		}
		ref := Monitor{Kind: o.Kind, Namespace: o.Namespace, Name: o.Name}.Ref()
		if !selected[ref] {
			continue
		}
		eps, _ := o.Spec[field].([]any)
		for _, ep := range eps {
			em, _ := ep.(map[string]any)
			port, _ := scalar(em["port"])
			path, _ := scalar(em["path"])
			rels, _ := em["metricRelabelings"].([]any)
			for _, r := range rels {
				rm, _ := r.(map[string]any)
				action, _ := scalar(rm["action"])
				if !strings.EqualFold(action, "drop") {
					continue
				}
				d := Drop{Monitor: ref, Endpoint: strings.TrimSpace(port + " " + path), Separator: ";", Regex: "(.*)"}
				if s, ok := scalar(rm["separator"]); ok {
					d.Separator = s
				}
				if s, ok := scalar(rm["regex"]); ok {
					d.Regex = s
				}
				srcs, _ := rm["sourceLabels"].([]any)
				for _, l := range srcs {
					s, _ := scalar(l)
					d.Labels = append(d.Labels, s)
				}
				re, err := regexp.Compile("^(?s:" + d.Regex + ")$")
				if err != nil {
					return nil, fmt.Errorf("%s endpoint %s: metricRelabelings regex %q: %w", ref, d.Endpoint, d.Regex, err)
				}
				d.re = re
				out = append(out, d)
			}
		}
	}
	return out, nil
}

func stringMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		s, _ := scalar(x)
		out[k] = s
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
