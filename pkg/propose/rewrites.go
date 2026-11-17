package propose

import (
	"fmt"
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
}

// Rewrites reads the name-touching rules of one pipeline file: Alloy
// prometheus.relabel rules and Prometheus metric_relabel_configs. Other
// kinds have none tvt reads yet (nil). It never evaluates an expression
// (ADR 0013): a non-literal value in a rule on __name__ is unfollowable.
func Rewrites(kind, file string, src []byte) ([]Rewrite, error) {
	switch kind {
	case "alloy":
		return alloyRewrites(file, src)
	case "prometheus":
		return promRewrites(file, src)
	}
	return nil, nil
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

func alloyRewrites(file string, src []byte) ([]Rewrite, error) {
	f, err := parser.ParseFile(file, src)
	if err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
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
		edges[id] = append(edges[id], alloyTargets(b.Body)...)
	}
	// upstream: component ID -> the components sending to it.
	upstream := map[string][]string{}
	for from, tos := range edges {
		for _, to := range tos {
			upstream[to] = append(upstream[to], from)
		}
	}
	var out []Rewrite
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
			touches, from, to, why := alloyRule(rb).nameRule()
			if !touches || (why == "" && from == "") {
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
