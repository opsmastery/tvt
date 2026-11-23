package check

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/opsmastery/tvt/pkg/report"
	"go.yaml.in/yaml/v3"
)

// relabelListKeys are the values keys whose lists Helm replaces wholesale
// and that carry metric drops (kube-prometheus-stack's monitors).
var relabelListKeys = map[string]bool{"metricRelabelings": true, "cAdvisorMetricRelabelings": true}

// helmEffects judges a Helm values overlay (S8 item 11). Helm replaces a
// list rather than merging it, so an overlay list at a key where the
// chart has default rules must carry every one of them, or the chart's
// own drops are lost. The overlay's rules are also judged like a
// monitor's metricRelabelings.
func helmEffects(p report.PipelineDef, path string, src []byte, ix *index, files Files) ([]effect, error) {
	root, err := yamlRoot(src)
	if err != nil {
		return nil, err
	}
	if p.HelmValues.Path != "" {
		for _, k := range strings.Split(p.HelmValues.Path, ".") {
			root = field(root, k)
		}
		if root == nil {
			return nil, nil
		}
	}
	var defaults *yaml.Node
	if p.HelmValues.Defaults != "" {
		if b, err := files(p.HelmValues.Defaults); err == nil {
			defaults, _ = yamlRoot(b)
		}
	}
	blocks := tvtBlocks(src)
	var out []effect
	var walk func(n *yaml.Node, keys []string)
	walk = func(n *yaml.Node, keys []string) {
		if n == nil || n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			ks := append(append([]string{}, keys...), k)
			if relabelListKeys[k] && v.Kind == yaml.SequenceNode {
				keyPath := strings.Join(ks, ".")
				rules, err := yamlRules(v, true)
				if err != nil {
					out = append(out, effect{rule: RuleUnreadable, pipeline: p.Name, file: path, component: keyPath, reason: err.Error()})
					continue
				}
				out = append(out, relabelEffects(p, path, "values "+keyPath, rules, nil, ix, blocks)...)
				if d := nodeAt(defaults, ks); d != nil && d.Kind == yaml.SequenceNode && len(d.Content) > 0 {
					have := map[string]bool{}
					for _, it := range v.Content {
						have[canonical(it)] = true
					}
					lost := 0
					for _, it := range d.Content {
						if !have[canonical(it)] {
							lost++
						}
					}
					if lost > 0 {
						out = append(out, effect{rule: RuleHelm, pipeline: p.Name, file: path, component: "values " + keyPath,
							reason: fmt.Sprintf("the overlay's list replaces the chart's %d default rules at %s and loses %d of them (Helm replaces lists): copy the chart's rules in first", len(d.Content), keyPath, lost),
							at:     fmt.Sprintf("line %d", v.Line), block: blockAt(blocks, v.Line)})
					}
				}
				continue
			}
			walk(v, ks)
		}
	}
	walk(root, nil)
	return out, nil
}

func nodeAt(n *yaml.Node, keys []string) *yaml.Node {
	for _, k := range keys {
		n = field(n, k)
		if n == nil {
			return nil
		}
	}
	return n
}

// canonical is a YAML node's content without comments or styles, for
// comparing rules.
func canonical(n *yaml.Node) string {
	var v any
	if err := n.Decode(&v); err != nil {
		return ""
	}
	b, _ := yaml.Marshal(v)
	return string(b)
}

// unselectedEffects: a ServiceMonitor or PodMonitor no Prometheus selects
// scrapes nothing, so a change to its rules does nothing (S8 item 11).
// The effect names the rules' content, so it's new exactly when the
// change edits them.
func unselectedEffects(p report.PipelineDef, path, kind, name string, ep *yaml.Node, unselected map[string]bool) []effect {
	if !unselected[kind+" "+name] {
		return nil
	}
	mr := field(ep, "metricRelabelings")
	if mr == nil {
		return nil
	}
	sum := sha256.Sum256([]byte(canonical(mr)))
	return []effect{{rule: RuleHelm, pipeline: p.Name, file: path, component: kind + " " + name,
		reason: fmt.Sprintf("no Prometheus selects %s %s, so its metricRelabelings (%x) change nothing: label it for the server's monitor selector first", kind, name, sum[:4]),
		at:     fmt.Sprintf("line %d", mr.Line)}}
}
