package propose

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/grafana/alloy/syntax/ast"
	"github.com/grafana/alloy/syntax/parser"
	"go.yaml.in/yaml/v3"
)

// Rewrite is a pipeline rule that touches metric names before they're
// stored (S7 item 9): a rename tvt follows (From to To), or a rule it
// can't follow (Unfollowable says why), which is an evidence gap on every
// metric that passes it, never "no reader" and never a safe drop.
//
// Which metrics pass it: those carrying one of Jobs (scrape configs
// upstream), those named with one of Prefixes or Names (an OTel connector
// upstream whose output tvt knows), or, with All, every metric the
// pipeline carries (an upstream tvt can't enumerate).
type Rewrite struct {
	File      string
	Component string
	From, To  string
	// Unfollowable is why tvt can't follow the rule; empty for a rename.
	Unfollowable string
	Jobs         []string
	Prefixes     []string
	Names        []string
	All          bool
	// Streams: the rule touches log streams' labels (Alloy loki.process);
	// with All, every stream the pipeline carries.
	Streams bool
	// Label: the rule sets metric labels rather than names (Alloy
	// prometheus.relabel replace or labelmap, OTel attributes, transform
	// and metricstransform). Labels are the label names it sets; empty
	// means tvt can't tell which, so every label of the metrics that pass
	// it. tvt doesn't place a label removal around such a rule yet, so the
	// scan holds those labels (Unfollowable says what the rule does).
	// Prometheus metric_relabel_configs aren't listed: tvt's rules go last
	// in the list, after every rename, so they act on the stored names.
	Label  bool
	Labels []string
}

// Rewrites reads the name-touching rules of one pipeline file: Alloy
// prometheus.relabel rules and Prometheus metric_relabel_configs. Other
// kinds have none tvt reads yet (nil). It never evaluates an expression
// (ADR 0013): a non-literal value in a rule on __name__ is unfollowable.
func Rewrites(kind, file string, src []byte) ([]Rewrite, error) {
	// tvt's own blocks aren't the pipeline's rewrites.
	prefix := "#"
	if kind == "alloy" {
		prefix = "//"
	}
	if stripped, _, err := stripBlocks(src, prefix); err == nil {
		src = stripped
	}
	switch kind {
	case "alloy":
		return alloyRewrites(file, src)
	case "prometheus":
		return promRewrites(file, src)
	case "otelcol":
		return otelRewrites(file, src)
	}
	return nil, nil
}

// OTel Collector rewrites: transform statements and metricstransform
// transforms that change metric names in a metrics pipeline. tvt follows
// an exact rename (set(metric.name, "new") where metric.name == "old";
// metricstransform include: old, match_type: strict, action: update,
// new_name: new) and treats anything else that touches a name (a pattern
// replace, a templated or computed new name, a where clause that isn't
// one exact name) as a rule it can't follow. Which metrics pass it isn't
// known from the config, so an unfollowable rule covers every metric the
// pipeline carries (All).
var (
	otelNameTarget = regexp.MustCompile(`^\s*([A-Za-z_]+)\(\s*(?:metric\.)?name\s*,`)
	otelExactSet   = regexp.MustCompile(`^\s*set\(\s*(?:metric\.)?name\s*,\s*"([^"\\$]+)"\s*\)\s+where\s+(?:metric\.)?name\s*==\s*"([^"\\]+)"\s*$`)
)

func otelRewrites(file string, src []byte) ([]Rewrite, error) {
	root, err := otelRoot(src)
	if err != nil {
		return nil, err
	}
	// Processors in metrics pipelines.
	used := map[string]bool{}
	if all := value(value(root, "service"), "pipelines"); all != nil && all.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(all.Content); i += 2 {
			name := all.Content[i].Value
			if name != "metrics" && !strings.HasPrefix(name, "metrics/") {
				continue
			}
			if pr := value(all.Content[i+1], "processors"); pr != nil {
				for _, it := range pr.Content {
					used[it.Value] = true
				}
			}
		}
	}
	procs := value(root, "processors")
	if procs == nil || procs.Kind != yaml.MappingNode {
		return nil, nil
	}
	var out []Rewrite
	for i := 0; i+1 < len(procs.Content); i += 2 {
		id, cfg := procs.Content[i].Value, procs.Content[i+1]
		if !used[id] {
			continue
		}
		typ, _, _ := strings.Cut(id, "/")
		comp := "processor " + id
		if strings.Contains(id, "tvt") {
			continue // tvt's own processors
		}
		switch typ {
		case "attributes":
			out = append(out, otelAttributesRules(file, comp, cfg)...)
		case "transform":
			for _, st := range otelStatements(value(cfg, "metric_statements")) {
				if rw, ok := otelLabelStatement(file, comp, st); ok {
					out = append(out, rw)
					continue
				}
				m := otelNameTarget.FindStringSubmatch(st)
				if m == nil {
					continue
				}
				if e := otelExactSet.FindStringSubmatch(st); e != nil {
					out = append(out, Rewrite{File: file, Component: comp, From: e[2], To: e[1], All: true})
					continue
				}
				out = append(out, Rewrite{File: file, Component: comp, Unfollowable: fmt.Sprintf("%s on the metric name: %s", m[1], shorten(st)), All: true})
			}
		case "metricstransform":
			ts := value(cfg, "transforms")
			if ts == nil {
				continue
			}
			for _, t := range ts.Content {
				out = append(out, metricstransformLabelOps(file, comp, t)...)
				newName := str(t, "new_name")
				if newName == "" && value(t, "new_name") == nil {
					continue // label operations only
				}
				include, match, action := str(t, "include"), str(t, "match_type"), str(t, "action")
				if match == "" {
					match = "strict"
				}
				switch {
				case action == "insert":
					continue // a copy under the new name: the old name is still stored
				case match == "strict" && action == "update" && include != "" && !strings.Contains(newName, "$"):
					out = append(out, Rewrite{File: file, Component: comp, From: include, To: newName, All: true})
				default:
					out = append(out, Rewrite{File: file, Component: comp, Unfollowable: fmt.Sprintf("%s %s rename of %q to %q", match, action, include, newName), All: true})
				}
			}
		}
	}
	return out, nil
}

var (
	otelSetAttr   = regexp.MustCompile(`^\s*set\(\s*(?:datapoint\.|resource\.|metric\.)?attributes\[\s*"([^"\\]+)"\s*\]\s*,`)
	otelKeyRename = regexp.MustCompile(`^\s*(replace_all_patterns|replace_all_matches)\(\s*(?:datapoint\.|metric\.)?attributes\s*,\s*"key"`)
)

// otelLabelStatement reads a transform statement that sets a metric
// attribute (a label once stored): set(attributes["k"], ...) sets k; a
// pattern replace on attribute keys renames keys tvt can't list.
func otelLabelStatement(file, comp, st string) (Rewrite, bool) {
	if m := otelSetAttr.FindStringSubmatch(st); m != nil {
		return Rewrite{File: file, Component: comp, Label: true, Labels: []string{m[1]}, All: true,
			Unfollowable: fmt.Sprintf("sets label %s: %s", m[1], shorten(st))}, true
	}
	if m := otelKeyRename.FindStringSubmatch(st); m != nil {
		return Rewrite{File: file, Component: comp, Label: true, All: true,
			Unfollowable: fmt.Sprintf("%s renames label keys: %s", m[1], shorten(st))}, true
	}
	return Rewrite{}, false
}

// otelAttributesRules reads an attributes processor's actions. insert,
// update and upsert set a key; extract sets keys from a pattern's named
// groups, which tvt doesn't parse; delete, hash and convert set no name.
func otelAttributesRules(file, comp string, cfg *yaml.Node) []Rewrite {
	acts := value(cfg, "actions")
	if acts == nil || acts.Kind != yaml.SequenceNode {
		return nil
	}
	var out []Rewrite
	for _, a := range acts.Content {
		key, action := str(a, "key"), str(a, "action")
		switch action {
		case "insert", "update", "upsert":
			if key == "" {
				out = append(out, Rewrite{File: file, Component: comp, Label: true, All: true, Unfollowable: action + " with no literal key"})
				continue
			}
			out = append(out, Rewrite{File: file, Component: comp, Label: true, Labels: []string{key}, All: true, Unfollowable: fmt.Sprintf("%s sets label %s", action, key)})
		case "extract":
			out = append(out, Rewrite{File: file, Component: comp, Label: true, All: true, Unfollowable: fmt.Sprintf("extract sets labels from a pattern on %s", cmpOr(key, "an attribute"))})
		}
	}
	return out
}

// metricstransformLabelOps reads a transform's label operations:
// update_label and add_label set new_label.
func metricstransformLabelOps(file, comp string, t *yaml.Node) []Rewrite {
	ops := value(t, "operations")
	if ops == nil || ops.Kind != yaml.SequenceNode {
		return nil
	}
	var out []Rewrite
	for _, op := range ops.Content {
		action, nl := str(op, "action"), str(op, "new_label")
		if (action == "update_label" || action == "add_label") && nl != "" {
			out = append(out, Rewrite{File: file, Component: comp, Label: true, Labels: []string{nl}, All: true,
				Unfollowable: fmt.Sprintf("%s sets label %s", action, nl)})
		}
	}
	return out
}

// otelStatements flattens metric_statements: plain strings, or groups
// with a statements list.
func otelStatements(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, it := range n.Content {
		switch it.Kind {
		case yaml.ScalarNode:
			out = append(out, it.Value)
		case yaml.MappingNode:
			if sts := value(it, "statements"); sts != nil {
				for _, st := range sts.Content {
					out = append(out, st.Value)
				}
			}
		}
	}
	return out
}

func shorten(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

// rule is one relabel rule, as far as tvt reads it.
type rule struct {
	sourceLabels        []string
	target, regex, repl string
	action              string
	unknown             string // the first attribute that isn't a literal
}

// nameRule classifies a rule that touches __name__: a rename (from, to),
// unfollowable (why), or neither (a literal keep or drop, which renames
// nothing). touches is false when the rule leaves names alone.
func (r rule) nameRule() (touches bool, from, to, why string) {
	if !slices.Contains(r.sourceLabels, "__name__") && r.target != "__name__" {
		if r.unknown == "source_labels" || r.unknown == "target_label" {
			return true, "", "", r.unknown + " isn't a literal tvt can read"
		}
		return false, "", "", ""
	}
	if r.unknown != "" {
		return true, "", "", r.unknown + " isn't a literal tvt can read"
	}
	action := r.action
	if action == "" {
		action = "replace"
	}
	switch action {
	case "keep", "drop":
		return true, "", "", ""
	case "replace":
		if r.target != "__name__" {
			return false, "", "", ""
		}
		if strings.Contains(r.repl, "$") {
			return true, "", "", fmt.Sprintf("replacement %q is templated", r.repl)
		}
		return true, r.regex, r.repl, ""
	}
	return true, "", "", "action " + action + " on __name__"
}

// labelRule classifies a rule that sets a label other than __name__:
// replace (and hashmod) set target_label; labelmap sets labels its regex
// derives, which tvt can't list. labeldrop and labelkeep only remove.
// why is empty when the rule sets no label.
func (r rule) labelRule() (labels []string, why string) {
	action := r.action
	if action == "" {
		action = "replace"
	}
	switch {
	case r.unknown != "":
		return nil, r.unknown + " isn't a literal tvt can read"
	case action == "labelmap":
		return nil, fmt.Sprintf("labelmap %q sets labels tvt can't list", r.regex)
	case (action == "replace" || action == "hashmod" || action == "lowercase" || action == "uppercase") && r.target != "" && r.target != "__name__":
		return []string{r.target}, fmt.Sprintf("%s sets label %s", action, r.target)
	}
	return nil, ""
}

func alloyRewrites(file string, src []byte) ([]Rewrite, error) {
	f, err := parser.ParseFile(file, src)
	if err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	var out []Rewrite
	blocks := map[string]*ast.BlockStmt{} // by component ID: name.label
	var order []string
	// edges: component ID -> the components it sends to.
	edges := map[string][]string{}
	for _, st := range f.Body {
		b, ok := st.(*ast.BlockStmt)
		if !ok {
			continue
		}
		id := strings.Join(b.Name, ".")
		if b.Label != "" {
			id += "." + b.Label
		}
		blocks[id] = b
		order = append(order, id)
		if strings.Join(b.Name, ".") == "loki.process" {
			if why := lokiStageWhy(b.Body); why != "" {
				out = append(out, Rewrite{File: file, Component: fmt.Sprintf("loki.process %q", b.Label), Unfollowable: why, All: true, Streams: true})
			}
		}
		edges[id] = append(edges[id], alloyTargets(b.Body)...)
	}
	// upstream: component ID -> the components sending to it.
	upstream := map[string][]string{}
	for from, tos := range edges {
		for _, to := range tos {
			upstream[to] = append(upstream[to], from)
		}
	}
	for _, id := range order {
		b := blocks[id]
		if strings.Join(b.Name, ".") != "prometheus.relabel" {
			continue
		}
		for _, st := range b.Body {
			rb, ok := st.(*ast.BlockStmt)
			if !ok || len(rb.Name) != 1 || rb.Name[0] != "rule" {
				continue
			}
			r := alloyRule(rb)
			touches, from, to, why := r.nameRule()
			if !touches {
				if labels, lwhy := r.labelRule(); lwhy != "" {
					rw := Rewrite{File: file, Component: fmt.Sprintf("prometheus.relabel %q", b.Label), Label: true, Labels: labels, Unfollowable: lwhy}
					alloySources(&rw, id, blocks, upstream, map[string]bool{})
					out = append(out, rw)
				}
				continue
			}
			if why == "" && from == "" {
				continue
			}
			rw := Rewrite{File: file, Component: fmt.Sprintf("prometheus.relabel %q", b.Label), From: from, To: to, Unfollowable: why}
			alloySources(&rw, id, blocks, upstream, map[string]bool{})
			out = append(out, rw)
		}
	}
	return out, nil
}

// alloyRule reads a rule block's attributes; a non-literal value is noted
// in unknown.
func alloyRule(b *ast.BlockStmt) rule {
	var r rule
	for _, st := range b.Body {
		a, ok := st.(*ast.AttributeStmt)
		if !ok {
			continue
		}
		name := a.Name.Name
		if name == "source_labels" {
			arr, ok := a.Value.(*ast.ArrayExpr)
			if !ok {
				r.unknown = cmpOr(r.unknown, name)
				continue
			}
			for _, el := range arr.Elements {
				v, ok := literalString(el)
				if !ok {
					r.unknown = cmpOr(r.unknown, name)
					continue
				}
				r.sourceLabels = append(r.sourceLabels, v)
			}
			continue
		}
		var dst *string
		switch name {
		case "target_label":
			dst = &r.target
		case "regex":
			dst = &r.regex
		case "replacement":
			dst = &r.repl
		case "action":
			dst = &r.action
		default:
			continue
		}
		v, ok := literalString(a.Value)
		if !ok {
			r.unknown = cmpOr(r.unknown, name+" = "+exprKind(a.Value))
			continue
		}
		*dst = v
	}
	// Alloy's replacement defaults to $1, as Prometheus's does.
	if r.target == "__name__" && attr(b.Body, "replacement") == nil {
		r.repl = "$1"
	}
	return r
}

// exprKind names an expression tvt won't evaluate, for the gap's reason.
func exprKind(e ast.Expr) string {
	if c, ok := e.(*ast.CallExpr); ok {
		if n, ok := refName(c.Value); ok {
			return n + "(...)"
		}
		return "a function call"
	}
	if n, ok := refName(e); ok {
		return n
	}
	return "an expression"
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// alloyTargets are the components a block's forward_to and output lists
// reference (reference.receiver or reference.input, without the export).
func alloyTargets(body ast.Body) []string {
	var out []string
	add := func(e ast.Expr) {
		arr, ok := e.(*ast.ArrayExpr)
		if !ok {
			return
		}
		for _, el := range arr.Elements {
			if ref, ok := refName(el); ok {
				if i := strings.LastIndex(ref, "."); i > 0 {
					out = append(out, ref[:i])
				}
			}
		}
	}
	for _, st := range body {
		switch s := st.(type) {
		case *ast.AttributeStmt:
			if s.Name.Name == "forward_to" {
				add(s.Value)
			}
		case *ast.BlockStmt:
			if len(s.Name) == 1 && s.Name[0] == "output" {
				for _, o := range s.Body {
					if a, ok := o.(*ast.AttributeStmt); ok && a.Name.Name == "metrics" {
						add(a.Value)
					}
				}
			}
		}
	}
	return out
}

// alloySources fills in which metrics reach component id: scrape jobs,
// the span metrics connector's output and target_info, or All.
func alloySources(rw *Rewrite, id string, blocks map[string]*ast.BlockStmt, upstream map[string][]string, seen map[string]bool) {
	if seen[id] {
		return
	}
	seen[id] = true
	ups := upstream[id]
	if len(ups) == 0 && !strings.HasPrefix(id, "prometheus.scrape.") {
		rw.All = true // a source tvt can't see (or the file's entry point)
		return
	}
	for _, u := range ups {
		b, ok := blocks[u]
		if !ok {
			rw.All = true
			continue
		}
		switch strings.Join(b.Name, ".") {
		case "prometheus.scrape":
			job := b.Label
			if v, ok := literalString(attr(b.Body, "job_name")); ok {
				job = v
			} else if attr(b.Body, "job_name") != nil {
				rw.All = true
				continue
			}
			if !slices.Contains(rw.Jobs, job) {
				rw.Jobs = append(rw.Jobs, job)
			}
		case "prometheus.relabel":
			alloySources(rw, u, blocks, upstream, seen)
		case "otelcol.exporter.prometheus":
			// It adds target_info unless told not to, and passes what its
			// own upstream produces.
			if v := attr(b.Body, "include_target_info"); v == nil || !isFalse(v) {
				addName(&rw.Names, "target_info")
			}
			otelSources(rw, u, blocks, upstream, map[string]bool{})
		default:
			rw.All = true
		}
	}
}

// otelSources: what reaches an otelcol component. The span metrics
// connector's metrics share its namespace's prefix; any other source is
// one tvt can't enumerate.
func otelSources(rw *Rewrite, id string, blocks map[string]*ast.BlockStmt, upstream map[string][]string, seen map[string]bool) {
	if seen[id] {
		return
	}
	seen[id] = true
	ups := upstream[id]
	if len(ups) == 0 {
		rw.All = true
		return
	}
	for _, u := range ups {
		b, ok := blocks[u]
		if !ok {
			rw.All = true
			continue
		}
		name := strings.Join(b.Name, ".")
		switch {
		case name == "otelcol.connector.spanmetrics":
			ns := "traces.span.metrics"
			if v := attr(b.Body, "namespace"); v != nil {
				s, ok := literalString(v)
				if !ok {
					rw.All = true
					continue
				}
				ns = s
			}
			p := strings.NewReplacer(".", "_", "-", "_").Replace(ns)
			if p != "" {
				p += "_"
			}
			if !slices.Contains(rw.Prefixes, p) {
				rw.Prefixes = append(rw.Prefixes, p)
			}
		case strings.HasPrefix(name, "otelcol.processor."):
			otelSources(rw, u, blocks, upstream, seen)
		default:
			rw.All = true
		}
	}
}

func isFalse(e ast.Expr) bool {
	l, ok := e.(*ast.LiteralExpr)
	return ok && l.Value == "false"
}

func addName(names *[]string, n string) {
	if !slices.Contains(*names, n) {
		*names = append(*names, n)
	}
}

func promRewrites(file string, src []byte) ([]Rewrite, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("propose: not a Prometheus config (want a mapping)")
	}
	scrapes := value(doc.Content[0], "scrape_configs")
	if scrapes == nil {
		return nil, nil
	}
	var out []Rewrite
	for _, sc := range scrapes.Content {
		if sc.Kind != yaml.MappingNode {
			continue
		}
		jn := value(sc, "job_name")
		mrc := value(sc, "metric_relabel_configs")
		if jn == nil || mrc == nil || mrc.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range mrc.Content {
			var r rule
			if err := item.Decode(&struct {
				SourceLabels *[]string `yaml:"source_labels"`
				Target       *string   `yaml:"target_label"`
				Regex        *string   `yaml:"regex"`
				Repl         *string   `yaml:"replacement"`
				Action       *string   `yaml:"action"`
			}{&r.sourceLabels, &r.target, &r.regex, &r.repl, &r.action}); err != nil {
				return nil, fmt.Errorf("propose: job %s: %w", jn.Value, err)
			}
			// Prometheus's replacement defaults to $1.
			if r.target == "__name__" && item != nil && value(item, "replacement") == nil {
				r.repl = "$1"
			}
			touches, from, to, why := r.nameRule()
			if !touches || (why == "" && from == "") {
				continue
			}
			out = append(out, Rewrite{File: file, Component: "job " + jn.Value, From: from, To: to, Unfollowable: why, Jobs: []string{jn.Value}})
		}
	}
	return out, nil
}

// lokiStageWhy says why a loki.process component's stages make stored
// stream labels depend on log content (stage.labels or stage.template
// setting labels, a computed tenant), or "" when its label changes are
// literal or absent. tvt then can't tell which streams in feed which
// streams out, so a stream proposal can't be scoped to the source.
func lokiStageWhy(body ast.Body) string {
	for _, st := range body {
		b, ok := st.(*ast.BlockStmt)
		if !ok {
			continue
		}
		switch strings.Join(b.Name, ".") {
		case "stage.labels":
			return "stage.labels sets stream labels from log content"
		case "stage.template":
			return "stage.template computes values that can become labels"
		case "stage.tenant":
			if _, lit := literalString(attr(b.Body, "value")); !lit {
				return "stage.tenant picks the tenant from log content"
			}
		case "stage.match":
			if why := lokiStageWhy(b.Body); why != "" {
				return why + " (inside stage.match)"
			}
		}
	}
	return ""
}
