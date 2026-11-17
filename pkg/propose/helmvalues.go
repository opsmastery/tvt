package propose

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// kube-prometheus-stack Helm values (S7 item 3). The chart renders each
// ServiceMonitor's metricRelabelings from a values key, and Helm replaces
// a list rather than merging it, so a values overlay that sets the key
// must repeat the chart's default rules (the apiserver's bucket drops, the
// kubelet's cAdvisor drops) or it silently deletes them.

// kpsMonitorKeys maps a kube-prometheus-stack ServiceMonitor, by the
// component its name ends in, to the values keys that render its
// endpoints' metricRelabelings. Checked with helm template at chart
// 93.3.0, each key set to a sentinel rule: every rendered monitor's
// endpoints took exactly its keys' rules (the kubelet's three default
// endpoints: metrics, cAdvisor, probes).
var kpsMonitorKeys = []struct {
	suffix string
	keys   []string
}{
	// Longest suffixes first: -prometheus-node-exporter before -prometheus.
	{"-prometheus-node-exporter", []string{"prometheus-node-exporter.prometheus.monitor.metricRelabelings"}},
	{"-kube-controller-manager", []string{"kubeControllerManager.serviceMonitor.metricRelabelings"}},
	{"-kube-state-metrics", []string{"kube-state-metrics.prometheus.monitor.metricRelabelings"}},
	{"-kube-scheduler", []string{"kubeScheduler.serviceMonitor.metricRelabelings"}},
	{"-alertmanager", []string{"alertmanager.serviceMonitor.metricRelabelings"}},
	{"-prometheus", []string{"prometheus.serviceMonitor.metricRelabelings"}},
	{"-kube-proxy", []string{"kubeProxy.serviceMonitor.metricRelabelings"}},
	{"-apiserver", []string{"kubeApiServer.serviceMonitor.metricRelabelings"}},
	{"-kube-etcd", []string{"kubeEtcd.serviceMonitor.metricRelabelings"}},
	{"-operator", []string{"prometheusOperator.serviceMonitor.metricRelabelings"}},
	{"-coredns", []string{"coreDns.serviceMonitor.metricRelabelings"}},
	{"-kubelet", []string{"kubelet.serviceMonitor.metricRelabelings", "kubelet.serviceMonitor.cAdvisorMetricRelabelings", "kubelet.serviceMonitor.probesMetricRelabelings"}},
	{"-grafana", []string{"grafana.serviceMonitor.metricRelabelings"}},
}

// KPSValuesKeys are the kube-prometheus-stack values keys for a
// ServiceMonitor's metricRelabelings, by its name; nil for a monitor the
// chart doesn't render.
func KPSValuesKeys(monitor string) []string {
	for _, k := range kpsMonitorKeys {
		if strings.HasSuffix(monitor, k.suffix) {
			return k.keys
		}
	}
	return nil
}

// HelmValues adds each drop, keyed by a dotted values path (under root,
// the chart's place in an umbrella chart's values, or ""), as a
// metricRelabelings rule in a values file's bytes. A path the file
// already sets as a block list gets the rule appended. A path it doesn't
// set is created inside the marked block, its list starting with the
// chart's default rules for that path, from defaults (the chart's own
// values.yaml), so the overlay keeps them. Every other byte is kept;
// existing tvt blocks are removed first.
func HelmValues(values, defaults []byte, root string, byPath map[string]Drop) ([]byte, error) {
	for _, d := range byPath {
		if err := d.validate(); err != nil {
			return nil, err
		}
	}
	var defs yaml.Node
	if err := yaml.Unmarshal(defaults, &defs); err != nil {
		return nil, fmt.Errorf("propose: chart defaults: %w", err)
	}
	out, _, err := stripBlocks(values, "#")
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		full := p
		if root != "" {
			full = root + "." + p
		}
		if out, err = helmValueDrop(out, &defs, strings.Split(full, "."), strings.Split(p, "."), byPath[p]); err != nil {
			return nil, fmt.Errorf("propose: %s: %w", full, err)
		}
	}
	for _, p := range paths {
		full := p
		if root != "" {
			full = root + "." + p
		}
		if err := checkHelmValue(out, strings.Split(full, "."), byPath[p]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func helmValueDrop(src []byte, defs *yaml.Node, path, defPath []string, d Drop) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	var cur *yaml.Node // the deepest mapping the file has on the path
	if len(doc.Content) == 1 {
		cur = doc.Content[0]
		if cur.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("the values file isn't a mapping")
		}
	}
	depth := 0
	for cur != nil && depth < len(path) {
		next := value(cur, path[depth])
		if next == nil {
			break
		}
		if depth == len(path)-1 {
			// The file sets the list: append to it.
			if next.Kind != yaml.SequenceNode || next.Style&yaml.FlowStyle != 0 || len(next.Content) == 0 {
				return nil, fmt.Errorf("set as something other than a block list tvt can append to (write it as a block list)")
			}
			item := strings.Repeat(" ", next.Content[0].Column-3)
			return insertAfterLine(src, pastBlockEnd(src, lastLine(next)), helmRuleBlock(item, d, nil)), nil
		}
		if next.Kind != yaml.MappingNode || next.Style&yaml.FlowStyle != 0 || len(next.Content) == 0 {
			return nil, fmt.Errorf("%s isn't a block mapping tvt can add to", strings.Join(path[:depth+1], "."))
		}
		cur = next
		depth++
	}
	// Create the rest of the path under cur, the list starting with the
	// chart's defaults.
	indent, after := "", strings.Count(string(src), "\n")
	if cur != nil && depth > 0 {
		indent = strings.Repeat(" ", cur.Content[0].Column-1)
		after = pastBlockEnd(src, lastLine(cur))
	}
	var keys strings.Builder
	in := indent
	for _, k := range path[depth : len(path)-1] {
		fmt.Fprintf(&keys, "%s%s:\n", in, k)
		in += "  "
	}
	defaultsList := lookup(defs, defPath)
	text := helmRuleBlock(in+"  ", d, defaultsList)
	// Keys go inside the block, after its begin line.
	lines := strings.SplitAfterN(text, "\n", 2)
	text = lines[0] + keys.String() + fmt.Sprintf("%s%s:\n", in, path[len(path)-1]) + lines[1]
	// Re-indent the begin line to the first key's indent.
	text = strings.Replace(text, in+"  # tvt:begin", indent+"# tvt:begin", 1)
	return insertAfterLine(src, after, text), nil
}

// helmRuleBlock is the marked block: the chart's default rules (when the
// key is new), then tvt's rule; item is the list items' indent.
func helmRuleBlock(item string, d Drop, defaults *yaml.Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s# %s\n", item, d.begin())
	if defaults != nil && defaults.Kind == yaml.SequenceNode && len(defaults.Content) > 0 {
		fmt.Fprintf(&b, "%s# the chart's default rules: Helm replaces the list, so the overlay repeats them\n", item)
		for _, r := range defaults.Content {
			stripComments(r)
			var buf bytes.Buffer
			enc := yaml.NewEncoder(&buf)
			enc.SetIndent(2)
			_ = enc.Encode(r)
			_ = enc.Close()
			for i, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
				prefix := item + "  "
				if i == 0 {
					prefix = item + "- "
				}
				fmt.Fprintf(&b, "%s%s\n", prefix, l)
			}
		}
	}
	if d.Reason != "" {
		fmt.Fprintf(&b, "%s# %s\n", item, d.Reason)
	}
	fmt.Fprintf(&b, "%s- action: drop\n", item)
	fmt.Fprintf(&b, "%s  sourceLabels: [__name__]\n", item)
	fmt.Fprintf(&b, "%s  regex: %q\n", item, d.Regex())
	fmt.Fprintf(&b, "%s# %s\n", item, endMarker)
	return b.String()
}

func stripComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		stripComments(c)
	}
}

func lookup(doc *yaml.Node, path []string) *yaml.Node {
	n := doc
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	for _, k := range path {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		n = value(n, k)
	}
	return n
}

// checkHelmValue re-reads the file: the path is a list holding the rule.
func checkHelmValue(src []byte, path []string, d Drop) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return fmt.Errorf("propose: the edited values don't parse: %w", err)
	}
	n := lookup(&doc, path)
	if n == nil || n.Kind != yaml.SequenceNode {
		return fmt.Errorf("propose: %s isn't a list after the edit", strings.Join(path, "."))
	}
	for _, r := range n.Content {
		if str(r, "action") == "drop" && str(r, "regex") == d.Regex() {
			return nil
		}
	}
	return fmt.Errorf("propose: %s lost the drop rule", strings.Join(path, "."))
}
