package kube

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

// Monitor kinds tvt resolves, and the Prometheus kinds that select them.
const (
	KindServiceMonitor = "ServiceMonitor"
	KindPodMonitor     = "PodMonitor"
)

var serverKinds = map[string]bool{"Prometheus": true, "PrometheusAgent": true}

// monitorFields maps a monitor kind to the server's selector fields and the
// scrape pool prefix prometheus-operator gives its jobs.
var monitorFields = map[string]struct{ selector, nsSelector, pool string }{
	KindServiceMonitor: {"serviceMonitorSelector", "serviceMonitorNamespaceSelector", "serviceMonitor"},
	KindPodMonitor:     {"podMonitorSelector", "podMonitorNamespaceSelector", "podMonitor"},
}

// Monitor is one ServiceMonitor or PodMonitor and what selects it.
type Monitor struct {
	Kind      string
	Namespace string
	Name      string
	File      string
	// SelectedBy lists the Prometheus (or PrometheusAgent) objects that
	// select it, as kind/namespace/name.
	SelectedBy []string
	// Unknown lists the servers whose choice tvt can't tell, each with why
	// (a namespace selector over namespaces the manifests don't define).
	Unknown []string
}

// Ref is kind/namespace/name.
func (m Monitor) Ref() string { return m.Kind + "/" + m.Namespace + "/" + m.Name }

// Selected is true when some server provably selects it.
func (m Monitor) Selected() bool { return len(m.SelectedBy) > 0 }

// Selection is which monitors the servers in a set of manifests select.
type Selection struct {
	// Servers are the Prometheus and PrometheusAgent objects, as
	// kind/namespace/name.
	Servers  []string
	Monitors []Monitor
}

// NotSelected lists the monitors no server selects or may select.
func (s Selection) NotSelected() []Monitor {
	var out []Monitor
	for _, m := range s.Monitors {
		if !m.Selected() && len(m.Unknown) == 0 {
			out = append(out, m)
		}
	}
	return out
}

// Uncertain lists the monitors no server provably selects but some may.
func (s Selection) Uncertain() []Monitor {
	var out []Monitor
	for _, m := range s.Monitors {
		if !m.Selected() && len(m.Unknown) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// ForPool returns the monitor behind a prometheus-operator scrape pool
// (serviceMonitor/<ns>/<name>/<endpoint>, podMonitor/<ns>/<name>/<endpoint>)
// when a server selects it. A monitor nothing selects never scrapes, so
// series are never attributed to it, whatever a pool name says.
func (s Selection) ForPool(pool string) (Monitor, bool) {
	parts := strings.Split(pool, "/")
	if len(parts) != 4 {
		return Monitor{}, false
	}
	for _, m := range s.Monitors {
		if monitorFields[m.Kind].pool == parts[0] && m.Namespace == parts[1] && m.Name == parts[2] && m.Selected() {
			return m, true
		}
	}
	return Monitor{}, false
}

// Resolve decides which ServiceMonitors and PodMonitors each Prometheus or
// PrometheusAgent in objs selects, with prometheus-operator's rules:
//
//   - a nil (absent or null) monitor selector selects none; {} selects all;
//   - a nil namespace selector means the server's own namespace only; {}
//     means every namespace;
//   - any other namespace selector is matched against the Namespace
//     objects in objs (plus the kubernetes.io/metadata.name label every
//     namespace carries); a namespace the manifests don't define makes the
//     answer unknown unless the selector only reads that label;
//   - matchLabels and matchExpressions (In, NotIn, Exists, DoesNotExist)
//     must all hold; an invalid selector makes the answer unknown.
//
// Objects with no namespace are in defaultNS (helm template's --namespace,
// or "default"). Resolve is I/O-free.
func Resolve(objs []Object, defaultNS string) Selection {
	if defaultNS == "" {
		defaultNS = "default"
	}
	ns := func(o Object) string {
		if o.Namespace == "" {
			return defaultNS
		}
		return o.Namespace
	}
	nsLabels := map[string]map[string]string{}
	for _, o := range objs {
		if o.Kind == "Namespace" && o.APIVersion == "v1" {
			l := maps.Clone(o.Labels)
			if l == nil {
				l = map[string]string{}
			}
			l["kubernetes.io/metadata.name"] = o.Name
			nsLabels[o.Name] = l
		}
	}
	var sel Selection
	type server struct {
		ref, ns string
		spec    map[string]any
	}
	var servers []server
	for _, o := range objs {
		if serverKinds[o.Kind] && isOperatorAPI(o.APIVersion) {
			ref := o.Kind + "/" + ns(o) + "/" + o.Name
			servers = append(servers, server{ref, ns(o), o.Spec})
			sel.Servers = append(sel.Servers, ref)
		}
	}
	for _, o := range objs {
		f, ok := monitorFields[o.Kind]
		if !ok || !isOperatorAPI(o.APIVersion) {
			continue
		}
		m := Monitor{Kind: o.Kind, Namespace: ns(o), Name: o.Name, File: o.File}
		for _, s := range servers {
			labelsOK, err := matches(s.spec[f.selector], o.Labels, false)
			if err != nil {
				m.Unknown = append(m.Unknown, fmt.Sprintf("%s: %s: %v", s.ref, f.selector, err))
				continue
			}
			if !labelsOK {
				continue
			}
			var nsOK bool
			raw, set := s.spec[f.nsSelector]
			switch {
			case !set || raw == nil:
				nsOK = m.Namespace == s.ns
			default:
				l, known := nsLabels[m.Namespace]
				if !known {
					l = map[string]string{"kubernetes.io/metadata.name": m.Namespace}
					if !onlyReads(raw, "kubernetes.io/metadata.name") {
						m.Unknown = append(m.Unknown, fmt.Sprintf("%s: %s reads labels of namespace %s, which the manifests don't define", s.ref, f.nsSelector, m.Namespace))
						continue
					}
				}
				nsOK, err = matches(raw, l, true)
				if err != nil {
					m.Unknown = append(m.Unknown, fmt.Sprintf("%s: %s: %v", s.ref, f.nsSelector, err))
					continue
				}
			}
			if nsOK {
				m.SelectedBy = append(m.SelectedBy, s.ref)
			}
		}
		sel.Monitors = append(sel.Monitors, m)
	}
	sort.Slice(sel.Monitors, func(i, j int) bool { return sel.Monitors[i].Ref() < sel.Monitors[j].Ref() })
	sort.Strings(sel.Servers)
	return sel
}

func isOperatorAPI(v string) bool { return strings.HasPrefix(v, "monitoring.coreos.com/") }

// matches evaluates a Kubernetes label selector. A nil selector matches
// nothing (prometheus-operator's monitor selectors) unless nilAll; callers
// handle a nil namespace selector before calling.
func matches(raw any, labels map[string]string, nilAll bool) (bool, error) {
	if raw == nil {
		return nilAll, nil
	}
	sel, ok := raw.(map[string]any)
	if !ok {
		return false, fmt.Errorf("not a label selector")
	}
	for k := range sel {
		if k != "matchLabels" && k != "matchExpressions" {
			return false, fmt.Errorf("unknown field %q", k)
		}
	}
	if ml, ok := sel["matchLabels"]; ok && ml != nil {
		m, ok := ml.(map[string]any)
		if !ok {
			return false, fmt.Errorf("matchLabels is not a map")
		}
		for k, v := range m {
			want, ok := scalar(v)
			if !ok {
				return false, fmt.Errorf("matchLabels %s is not a string", k)
			}
			if got, has := labels[k]; !has || got != want {
				return false, nil
			}
		}
	}
	if me, ok := sel["matchExpressions"]; ok && me != nil {
		list, ok := me.([]any)
		if !ok {
			return false, fmt.Errorf("matchExpressions is not a list")
		}
		for _, e := range list {
			ok, err := expression(e, labels)
			if err != nil || !ok {
				return false, err
			}
		}
	}
	return true, nil
}

func expression(e any, labels map[string]string) (bool, error) {
	m, ok := e.(map[string]any)
	if !ok {
		return false, fmt.Errorf("matchExpressions entry is not a map")
	}
	key, _ := scalar(m["key"])
	op, _ := scalar(m["operator"])
	if key == "" {
		return false, fmt.Errorf("matchExpressions entry without a key")
	}
	var values []string
	if raw, ok := m["values"]; ok && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return false, fmt.Errorf("matchExpressions %s: values is not a list", key)
		}
		for _, v := range list {
			s, ok := scalar(v)
			if !ok {
				return false, fmt.Errorf("matchExpressions %s: value is not a string", key)
			}
			values = append(values, s)
		}
	}
	got, has := labels[key]
	switch op {
	case "In", "NotIn":
		if len(values) == 0 {
			return false, fmt.Errorf("matchExpressions %s %s: no values", key, op)
		}
		in := has && slices.Contains(values, got)
		return in == (op == "In"), nil
	case "Exists", "DoesNotExist":
		if len(values) > 0 {
			return false, fmt.Errorf("matchExpressions %s %s: values must be empty", key, op)
		}
		return has == (op == "Exists"), nil
	}
	return false, fmt.Errorf("matchExpressions %s: unknown operator %q", key, op)
}

// onlyReads is true when a selector reads no label but key.
func onlyReads(raw any, key string) bool {
	sel, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	if ml, ok := sel["matchLabels"].(map[string]any); ok {
		for k := range ml {
			if k != key {
				return false
			}
		}
	}
	if me, ok := sel["matchExpressions"].([]any); ok {
		for _, e := range me {
			m, ok := e.(map[string]any)
			if !ok {
				return false
			}
			if k, _ := scalar(m["key"]); k != key {
				return false
			}
		}
	}
	return true
}

// scalar reads a YAML scalar as Kubernetes would a string field: label
// values like "true" or 1 decode as other types when unquoted.
func scalar(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case bool, int, int64, uint64, float64:
		return fmt.Sprint(v), true
	}
	return "", false
}
