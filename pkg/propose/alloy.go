package propose

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/grafana/alloy/syntax/ast"
	"github.com/grafana/alloy/syntax/parser"
)

// Alloy adds d as a rule block to the prometheus.relabel components that
// the prometheus.scrape components for jobs forward to, in an Alloy config
// file's bytes (S7 item 3, ADR 0013). Existing tvt blocks are removed
// first; the new block is spliced into the original bytes after the
// component's last rule block, indented like its body, so every other byte
// is kept. The result must parse.
//
// It fails rather than guess: a job with no scrape component in the file,
// a scrape component whose forward_to isn't a list of component
// references, or no prometheus.relabel component downstream.
func Alloy(src []byte, jobs []string, d Drop) ([]byte, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("propose: drop %s: no scrape job to put it in", d.ID)
	}
	out, _, err := stripBlocks(src, "//")
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseFile("config.alloy", out)
	if err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	relabels := map[string]*ast.BlockStmt{}
	scrapes := map[string]*ast.BlockStmt{} // by job_name
	for _, st := range f.Body {
		b, ok := st.(*ast.BlockStmt)
		if !ok {
			continue
		}
		switch strings.Join(b.Name, ".") {
		case "prometheus.relabel":
			relabels[b.Label] = b
		case "prometheus.scrape":
			job := b.Label // Alloy's default job_name is the component's name
			if v, ok := literalString(attr(b.Body, "job_name")); ok {
				job = v
			}
			scrapes[job] = b
		}
	}
	targets := map[string]*ast.BlockStmt{}
	for _, job := range jobs {
		sc, ok := scrapes[job]
		if !ok {
			return nil, fmt.Errorf("propose: no prometheus.scrape component for job %q", job)
		}
		refs, err := forwardTo(attr(sc.Body, "forward_to"))
		if err != nil {
			return nil, fmt.Errorf("propose: prometheus.scrape %q: %w", sc.Label, err)
		}
		found := false
		for _, r := range refs {
			if strings.HasPrefix(r, "prometheus.relabel.") && strings.HasSuffix(r, ".receiver") {
				label := strings.TrimSuffix(strings.TrimPrefix(r, "prometheus.relabel."), ".receiver")
				if rb, ok := relabels[label]; ok {
					targets[label] = rb
					found = true
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("propose: prometheus.scrape %q forwards to no prometheus.relabel component in this file", sc.Label)
		}
	}
	// Bottom up, so earlier offsets stay valid.
	type edit struct {
		after int
		text  string
	}
	var edits []edit
	for _, rb := range targets {
		after := rb.LCurlyPos.Position().Line
		indent := ""
		for _, st := range rb.Body {
			if b, ok := st.(*ast.BlockStmt); ok && len(b.Name) == 1 && b.Name[0] == "rule" {
				after = b.RCurlyPos.Position().Line
			}
		}
		if len(rb.Body) > 0 {
			indent = lineIndent(out, ast.StartPos(rb.Body[0]).Position().Line)
		} else {
			indent = lineIndent(out, rb.NamePos.Position().Line) + "  "
		}
		edits = append(edits, edit{after, alloyBlock(indent, d)})
	}
	for i := 0; i < len(edits); i++ {
		for j := i + 1; j < len(edits); j++ {
			if edits[j].after > edits[i].after {
				edits[i], edits[j] = edits[j], edits[i]
			}
		}
	}
	for _, e := range edits {
		out = insertAfterLine(out, e.after, e.text)
	}
	if _, err := parser.ParseFile("config.alloy", out); err != nil {
		return nil, fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	return out, nil
}

// alloyBlock is the marked rule block, aligned as alloy fmt aligns.
func alloyBlock(indent string, d Drop) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s// %s\n", indent, d.begin())
	if d.Reason != "" {
		fmt.Fprintf(&b, "%s// %s\n", indent, d.Reason)
	}
	unit := "  "
	if strings.HasPrefix(indent, "\t") {
		unit = "\t"
	}
	fmt.Fprintf(&b, "%srule {\n", indent)
	fmt.Fprintf(&b, "%s%ssource_labels = [\"__name__\"]\n", indent, unit)
	fmt.Fprintf(&b, "%s%sregex         = %s\n", indent, unit, strconv.Quote(d.Regex()))
	fmt.Fprintf(&b, "%s%saction        = \"drop\"\n", indent, unit)
	fmt.Fprintf(&b, "%s}\n", indent)
	fmt.Fprintf(&b, "%s// %s\n", indent, endMarker)
	return b.String()
}

func attr(body ast.Body, name string) ast.Expr {
	for _, st := range body {
		if a, ok := st.(*ast.AttributeStmt); ok && a.Name.Name == name {
			return a.Value
		}
	}
	return nil
}

// literalString is a string literal's value; anything else isn't known
// (ADR 0013: tvt never evaluates an expression).
func literalString(e ast.Expr) (string, bool) {
	l, ok := e.(*ast.LiteralExpr)
	if !ok || !strings.HasPrefix(l.Value, `"`) {
		return "", false
	}
	v, err := strconv.Unquote(l.Value)
	return v, err == nil
}

// forwardTo reads a forward_to list of component references.
func forwardTo(e ast.Expr) ([]string, error) {
	arr, ok := e.(*ast.ArrayExpr)
	if !ok {
		return nil, fmt.Errorf("forward_to isn't a list")
	}
	var out []string
	for _, el := range arr.Elements {
		ref, ok := refName(el)
		if !ok {
			return nil, fmt.Errorf("forward_to holds something other than a component reference")
		}
		out = append(out, ref)
	}
	return out, nil
}

func refName(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.IdentifierExpr:
		return x.Ident.Name, true
	case *ast.AccessExpr:
		v, ok := refName(x.Value)
		if !ok {
			return "", false
		}
		return v + "." + x.Name.Name, true
	}
	return "", false
}

// lineIndent is line n's leading whitespace.
func lineIndent(src []byte, n int) string {
	lines := strings.Split(string(src), "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	l := lines[n-1]
	return l[:len(l)-len(strings.TrimLeft(l, " \t"))]
}

// AlloyJobs lists the jobs of an Alloy config's prometheus.scrape
// components (job_name, or the component's label), in file order.
func AlloyJobs(src []byte) ([]string, error) {
	f, err := parser.ParseFile("config.alloy", src)
	if err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	var out []string
	for _, st := range f.Body {
		b, ok := st.(*ast.BlockStmt)
		if !ok || strings.Join(b.Name, ".") != "prometheus.scrape" {
			continue
		}
		job := b.Label
		if v, ok := literalString(attr(b.Body, "job_name")); ok {
			job = v
		}
		out = append(out, job)
	}
	return out, nil
}
