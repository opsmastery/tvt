package check

import (
	"fmt"
	"strings"

	"github.com/opsmastery/tvt/pkg/parse"
	"github.com/opsmastery/tvt/pkg/report"
	"go.yaml.in/yaml/v3"
)

// yamlRoot is a document's top node.
func yamlRoot(src []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode}, nil
	}
	return doc.Content[0], nil
}

func field(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func scalar(n *yaml.Node, key string) string {
	if v := field(n, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

func strs(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, c.Value)
	}
	return out
}

// yamlRules reads a relabel list; camel is the operator's spelling
// (sourceLabels, targetLabel).
func yamlRules(list *yaml.Node, camel bool) ([]relabelRule, error) {
	if list == nil {
		return nil, nil
	}
	k := func(snake, cml string) string {
		if camel {
			return cml
		}
		return snake
	}
	var out []relabelRule
	for _, it := range list.Content {
		r, err := newRule(strs(field(it, k("source_labels", "sourceLabels"))), scalar(it, "separator"), scalar(it, "regex"),
			scalar(it, "action"), scalar(it, k("target_label", "targetLabel")), scalar(it, "replacement"), it.Line)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// scrapeConfigs are a Prometheus config's scrape configs: under
// scrape_configs, or a file that is only the list.
func scrapeConfigs(root *yaml.Node) []*yaml.Node {
	if root.Kind == yaml.SequenceNode {
		return root.Content
	}
	if sc := field(root, "scrape_configs"); sc != nil {
		return sc.Content
	}
	return nil
}

func prometheusEffects(p report.PipelineDef, path string, src []byte, ix *index) ([]effect, error) {
	root, err := yamlRoot(src)
	if err != nil {
		return nil, err
	}
	blocks := tvtBlocks(src)
	var out []effect
	for _, sc := range scrapeConfigs(root) {
		job := scalar(sc, "job_name")
		rules, err := yamlRules(field(sc, "metric_relabel_configs"), false)
		if err != nil {
			return nil, fmt.Errorf("job %s: %w", job, err)
		}
		out = append(out, relabelEffects(p, path, "job "+job, rules, []string{job}, ix, blocks)...)
	}
	rw, err := remoteWriteEffects(p, path, root, ix, blocks)
	return append(out, rw...), err
}

// remoteWriteEffects judges remote_write's write_relabel_configs: a drop
// there takes metrics out of the backend as a scrape-time drop does. A
// remote_write to the retention tier's tenant (the manifest's
// dormant_tenant, S10 item 2) only narrows the tier's copy, and a dormant
// metric it keeps may leave the other remote_writes: that's the tier,
// not a drop. Anything else it keeps is judged as dropped.
func remoteWriteEffects(p report.PipelineDef, path string, root *yaml.Node, ix *index, blocks []blockSpan) ([]effect, error) {
	rw := field(root, "remote_write")
	if rw == nil || rw.Kind != yaml.SequenceNode {
		return nil, nil
	}
	var targets []writeTarget
	for i, e := range rw.Content {
		rules, err := yamlRules(field(e, "write_relabel_configs"), false)
		if err != nil {
			return nil, fmt.Errorf("remote_write %d: %w", i, err)
		}
		targets = append(targets, writeTarget{name: "remote_write " + cmpOr(scalar(e, "name"), scalar(e, "url")), rules: rules,
			tier: ix.dormantTenant != "" && scalar(field(e, "headers"), "X-Scope-OrgID") == ix.dormantTenant})
	}
	return writeTargetEffects(p, path, targets, ix, blocks), nil
}

// writeTarget is one remote write destination and its write relabeling;
// tier marks the retention tier's tenant.
type writeTarget struct {
	name  string
	rules []relabelRule
	tier  bool
}

// writeTargetEffects judges each non-tier target's write relabeling as a
// drop, except that a dormant metric the tier keeps may leave the others.
func writeTargetEffects(p report.PipelineDef, path string, targets []writeTarget, ix *index, blocks []blockSpan) []effect {
	tiered := func(metric string) bool {
		for _, t := range targets {
			if t.tier && evalRelabel(t.rules, metric, "", "").dropped == parse.False {
				return true
			}
		}
		return false
	}
	byID := map[string]report.ProtectedSignal{}
	for _, s := range ix.of(p.Name, "metric") {
		byID[s.ID] = s
	}
	var out []effect
	for _, t := range targets {
		if t.tier {
			continue
		}
		for _, e := range relabelEffects(p, path, t.name, t.rules, nil, ix, blocks) {
			if s, ok := byID[e.signal]; ok && s.Class == "dormant" && s.Record == "" && tiered(s.Name) {
				continue // the retention tier keeps it, in its own tenant
			}
			out = append(out, e)
		}
	}
	return out
}

// operatorEffects reads ServiceMonitor and PodMonitor metricRelabelings
// (prometheus-operator pipelines). The jobs a monitor makes depend on its
// Service, so every protected metric of the pipeline is judged against
// every monitor.
func operatorEffects(p report.PipelineDef, path string, src []byte, ix *index, unselectedList []string) ([]effect, error) {
	unselected := map[string]bool{}
	for _, u := range unselectedList {
		unselected[u] = true
	}
	blocks := tvtBlocks(src)
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	var out []effect
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		kind := scalar(root, "kind")
		if kind != "ServiceMonitor" && kind != "PodMonitor" {
			continue
		}
		name := scalar(field(root, "metadata"), "namespace") + "/" + scalar(field(root, "metadata"), "name")
		eps := field(field(root, "spec"), "endpoints")
		if kind == "PodMonitor" {
			eps = field(field(root, "spec"), "podMetricsEndpoints")
		}
		if eps == nil {
			continue
		}
		for i, ep := range eps.Content {
			rules, err := yamlRules(field(ep, "metricRelabelings"), true)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", kind, name, err)
			}
			out = append(out, relabelEffects(p, path, fmt.Sprintf("%s %s endpoint %d", kind, name, i), rules, nil, ix, blocks)...)
			out = append(out, unselectedEffects(p, path, kind, name, ep, unselected)...)
		}
	}
	return out, nil
}

// fluentbitEffects reads a Fluent Bit YAML pipeline: grep filters that
// exclude, and rewrite_tag that moves records to another tag (tvt's cold
// reroute). Either takes lines from Loki for the streams its match tag
// carries; tvt can't map tags to streams, so a step whose match covers a
// tag judged against every protected stream of the pipeline.
func fluentbitEffects(p report.PipelineDef, path string, src []byte, ix *index) ([]effect, error) {
	if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
		return nil, fmt.Errorf("check reads Fluent Bit's YAML config only (classic .conf and Helm values come later)")
	}
	root, err := yamlRoot(src)
	if err != nil {
		return nil, err
	}
	blocks := tvtBlocks(src)
	var out []effect
	filters := field(field(root, "pipeline"), "filters")
	if filters == nil {
		return nil, nil
	}
	for _, f := range filters.Content {
		name := scalar(f, "name")
		var what string
		switch name {
		case "grep":
			if field(f, "exclude") == nil && field(f, "Exclude") == nil && field(f, "regex") == nil && field(f, "Regex") == nil {
				continue
			}
			what = "drops lines with grep"
		case "rewrite_tag":
			what = "moves lines to another output with rewrite_tag"
		case "lua", "wasm":
			out = append(out, effect{rule: RuleFailClosed, pipeline: p.Name, file: path, component: "filter " + name,
				reason: "a " + name + " filter: check can't tell what it drops", at: fmt.Sprintf("line %d", f.Line)})
			continue
		default:
			continue
		}
		comp := "filter " + name + " match " + scalar(f, "match")
		out = append(out, effect{rule: ruleDropMarker, pipeline: p.Name, file: path, component: comp, reason: what,
			at: fmt.Sprintf("line %d", f.Line), block: blockAt(blocks, f.Line)})
		if name == "grep" && p.Cold != "" {
			out = append(out, effect{rule: RuleLeastLossy, pipeline: p.Name, file: path, component: comp,
				reason: "grep drops lines outright: reroute them to the " + p.Cold + " output with rewrite_tag first (ADR 0010: drops are staged)", at: fmt.Sprintf("line %d", f.Line), block: blockAt(blocks, f.Line)})
		}
		ruleText := strings.Join(append(strs(field(f, "rule")), append(strs(field(f, "exclude")), strs(field(f, "regex"))...)...), " ")
		if !textGuard(ruleText) {
			out = append(out, guardEffect(p, path, comp, what+" with no negative lookahead keeping the error pattern", fmt.Sprintf("line %d", f.Line), blockAt(blocks, f.Line)))
		}
		belowWarn := name == "rewrite_tag" && lookaheadLevel(ruleText)
		for _, s := range ix.of(p.Name, "stream") {
			if belowWarn && s.WarnPlus && s.Record == "" {
				// A level tier (S10 item 3): every reader keeps to warn and
				// above, and only lines naming a lower level move.
				continue
			}
			rule := ruleFor(s)
			reason := fmt.Sprintf("may take lines of %s from Loki, %s", s.Name, why(s))
			out = append(out, effect{rule: rule, pipeline: p.Name, file: path, component: comp, signal: s.ID, reason: reason,
				at: fmt.Sprintf("line %d", f.Line), block: blockAt(blocks, f.Line)})
		}
	}
	return out, nil
}
