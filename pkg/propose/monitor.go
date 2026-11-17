package propose

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Prometheus Operator monitors (S7 item 3): a ServiceMonitor or PodMonitor
// takes tvt's drop as a metricRelabelings rule on each of its endpoints.

// MonitorRef names a ServiceMonitor or PodMonitor.
type MonitorRef struct {
	Kind, Namespace, Name string
}

func (m MonitorRef) String() string { return m.Kind + "/" + m.Namespace + "/" + m.Name }

// MonitorJobs works out the job label each monitor's targets get, from
// manifests (one or more YAML streams): a PodMonitor's is
// <namespace>/<name>; a ServiceMonitor's is, for each Service it selects
// in the manifests, the value of the Service's label named by
// spec.jobLabel, or else the Service's name. A monitor whose job tvt can't
// work out (a jobLabel on a PodMonitor, a Service selector with
// expressions, no selected Service in the manifests, an endpoint
// relabeling that sets job) is in unresolved, with why.
func MonitorJobs(manifests ...[]byte) (jobs map[string][]MonitorRef, unresolved map[MonitorRef]string, err error) {
	var docs []*yaml.Node
	for _, m := range manifests {
		ds, err := yamlDocs(m)
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, ds...)
	}
	type svc struct {
		namespace, name string
		labels          map[string]string
	}
	var services []svc
	for _, d := range docs {
		if str(d, "kind") == "Service" {
			md := value(d, "metadata")
			services = append(services, svc{str(md, "namespace"), str(md, "name"), stringMap(value(md, "labels"))})
		}
	}
	jobs, unresolved = map[string][]MonitorRef{}, map[MonitorRef]string{}
	add := func(job string, m MonitorRef) {
		if !slices.Contains(jobs[job], m) {
			jobs[job] = append(jobs[job], m)
		}
	}
	for _, d := range docs {
		kind := str(d, "kind")
		if kind != "ServiceMonitor" && kind != "PodMonitor" {
			continue
		}
		md, spec := value(d, "metadata"), value(d, "spec")
		ref := MonitorRef{kind, str(md, "namespace"), str(md, "name")}
		if spec == nil {
			unresolved[ref] = "no spec"
			continue
		}
		if why := setsJob(spec, kind); why != "" {
			unresolved[ref] = why
			continue
		}
		if kind == "PodMonitor" {
			if str(spec, "jobLabel") != "" {
				unresolved[ref] = "jobLabel takes the job from pod labels tvt can't see"
				continue
			}
			add(ref.Namespace+"/"+ref.Name, ref)
			continue
		}
		sel := value(spec, "selector")
		if sel == nil || value(sel, "matchExpressions") != nil {
			unresolved[ref] = "a Service selector with expressions, or none"
			continue
		}
		want := stringMap(value(sel, "matchLabels"))
		namespaces := []string{ref.Namespace}
		any := false
		if ns := value(spec, "namespaceSelector"); ns != nil {
			if v := value(ns, "any"); v != nil && v.Value == "true" {
				any = true
			}
			if names := value(ns, "matchNames"); names != nil {
				namespaces = nil
				for _, n := range names.Content {
					namespaces = append(namespaces, n.Value)
				}
			}
		}
		found := false
		for _, s := range services {
			if !any && !slices.Contains(namespaces, s.namespace) {
				continue
			}
			ok := true
			for k, v := range want {
				if s.labels[k] != v {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			found = true
			job := s.name
			if l := str(spec, "jobLabel"); l != "" && s.labels[l] != "" {
				job = s.labels[l]
			}
			add(job, ref)
		}
		if !found {
			unresolved[ref] = "selects no Service in the manifests"
		}
	}
	return jobs, unresolved, nil
}

// setsJob says why an endpoint's relabelings may set the job label.
func setsJob(spec *yaml.Node, kind string) string {
	eps := value(spec, endpointsKey(kind))
	if eps == nil {
		return "no endpoints"
	}
	for _, ep := range eps.Content {
		if rl := value(ep, "relabelings"); rl != nil {
			for _, r := range rl.Content {
				if str(r, "targetLabel") == "job" || str(r, "action") == "labelmap" {
					return "an endpoint relabeling sets job"
				}
			}
		}
	}
	return ""
}

func endpointsKey(kind string) string {
	if kind == "PodMonitor" {
		return "podMetricsEndpoints"
	}
	return "endpoints"
}

func stringMap(n *yaml.Node) map[string]string {
	out := map[string]string{}
	if n == nil || n.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		out[n.Content[i].Value] = n.Content[i+1].Value
	}
	return out
}

// yamlDocs decodes every document of a YAML stream; line numbers are the
// stream's.
func yamlDocs(src []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var out []*yaml.Node
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("propose: %w", err)
		}
		if len(n.Content) == 1 && n.Content[0].Kind == yaml.MappingNode {
			out = append(out, n.Content[0])
		}
	}
}

// Monitors adds, for each monitor in byMonitor found in src, its drop as a
// metricRelabelings rule on every endpoint. Existing tvt blocks are removed
// first, every other byte is kept, and the result is re-read. A monitor
// in byMonitor that src doesn't hold is ignored (it's in another file);
// one with a flow-style or non-list metricRelabelings is an error.
func Monitors(src []byte, byMonitor map[MonitorRef]Drop) ([]byte, int, error) {
	for _, d := range byMonitor {
		if err := d.validate(); err != nil {
			return nil, 0, err
		}
	}
	out, _, err := stripBlocks(src, "#")
	if err != nil {
		return nil, 0, err
	}
	docs, err := yamlDocs(out)
	if err != nil {
		return nil, 0, err
	}
	type edit struct {
		after int
		text  string
	}
	var edits []edit
	written := 0
	for _, doc := range docs {
		kind := str(doc, "kind")
		md := value(doc, "metadata")
		ref := MonitorRef{kind, str(md, "namespace"), str(md, "name")}
		d, ok := byMonitor[ref]
		if !ok {
			continue
		}
		eps := value(value(doc, "spec"), endpointsKey(kind))
		if eps == nil || eps.Kind != yaml.SequenceNode || len(eps.Content) == 0 {
			return nil, 0, fmt.Errorf("propose: %s has no endpoints", ref)
		}
		written++
		for _, ep := range eps.Content {
			if ep.Kind != yaml.MappingNode || len(ep.Content) == 0 {
				return nil, 0, fmt.Errorf("propose: %s: an endpoint isn't a mapping", ref)
			}
			if mrc := value(ep, "metricRelabelings"); mrc != nil {
				if mrc.Kind != yaml.SequenceNode || mrc.Style&yaml.FlowStyle != 0 || len(mrc.Content) == 0 {
					return nil, 0, fmt.Errorf("propose: %s: metricRelabelings isn't a block list tvt can append to", ref)
				}
				edits = append(edits, edit{lastLine(mrc), monitorBlock(strings.Repeat(" ", mrc.Content[0].Column-3), d, false)})
				continue
			}
			edits = append(edits, edit{lastLine(ep), monitorBlock(strings.Repeat(" ", ep.Content[0].Column-1), d, true)})
		}
	}
	slices.SortStableFunc(edits, func(a, b edit) int { return b.after - a.after })
	for _, e := range edits {
		out = insertAfterLine(out, e.after, e.text)
	}
	// Re-read: every endpoint of every written monitor carries its rule.
	docs, err = yamlDocs(out)
	if err != nil {
		return nil, 0, fmt.Errorf("propose: the edited manifests don't parse: %w", err)
	}
	for _, doc := range docs {
		kind := str(doc, "kind")
		md := value(doc, "metadata")
		ref := MonitorRef{kind, str(md, "namespace"), str(md, "name")}
		d, ok := byMonitor[ref]
		if !ok {
			continue
		}
		for _, ep := range value(value(doc, "spec"), endpointsKey(kind)).Content {
			found := false
			if mrc := value(ep, "metricRelabelings"); mrc != nil {
				for _, r := range mrc.Content {
					if str(r, "action") == "drop" && str(r, "regex") == d.Regex() {
						found = true
					}
				}
			}
			if !found {
				return nil, 0, fmt.Errorf("propose: %s: an endpoint lost the drop rule", ref)
			}
		}
	}
	return out, written, nil
}

// monitorBlock is the marked rule: a new metricRelabelings key (withKey,
// at indent) or an item for an existing list (its dash at indent).
func monitorBlock(indent string, d Drop, withKey bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s# %s\n", indent, d.begin())
	if d.Reason != "" {
		fmt.Fprintf(&b, "%s# %s\n", indent, d.Reason)
	}
	item := indent
	if withKey {
		fmt.Fprintf(&b, "%smetricRelabelings:\n", indent)
		item = indent + "  "
	}
	fmt.Fprintf(&b, "%s- action: drop\n", item)
	fmt.Fprintf(&b, "%s  sourceLabels: [__name__]\n", item)
	fmt.Fprintf(&b, "%s  regex: %q\n", item, d.Regex())
	fmt.Fprintf(&b, "%s# %s\n", indent, endMarker)
	return b.String()
}
