package check

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/grafana/alloy/syntax/ast"
	"github.com/grafana/alloy/syntax/parser"
)

// alloyEffects reads an Alloy pipeline: prometheus.relabel rules (as
// Prometheus' metric relabeling), loki.process stages that drop lines or
// send them elsewhere, and otelcol processors that drop or sample spans.
func alloyEffects(p report.PipelineDef, path string, src []byte, ix *index) ([]effect, error) {
	f, err := parser.ParseFile(path, src)
	if err != nil {
		return nil, err
	}
	blocks := tvtBlocks(src)
	cold := coldKeeps(f, p.Cold)
	var out []effect
	for _, st := range f.Body {
		b, ok := st.(*ast.BlockStmt)
		if !ok {
			continue
		}
		name := strings.Join(b.Name, ".")
		comp := fmt.Sprintf("%s %q", name, b.Label)
		switch {
		case name == "prometheus.relabel":
			var rules []relabelRule
			for _, rs := range b.Body {
				rb, ok := rs.(*ast.BlockStmt)
				if !ok || len(rb.Name) != 1 || rb.Name[0] != "rule" {
					continue
				}
				r, err := alloyRelabelRule(rb)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", comp, err)
				}
				rules = append(rules, r)
			}
			out = append(out, relabelEffects(p, path, comp, rules, nil, ix, blocks)...)
		case name == "prometheus.remote_write":
			// write_relabel_config in each endpoint drops metrics on the
			// way to that backend (S10 item 2 debug pass: check never read
			// them, so an Alloy remote-write drop passed unseen).
			var targets []writeTarget
			for i, es := range b.Body {
				eb, ok := es.(*ast.BlockStmt)
				if !ok || len(eb.Name) != 1 || eb.Name[0] != "endpoint" {
					continue
				}
				var rules []relabelRule
				name := fmt.Sprintf("%s endpoint %d", comp, i)
				for _, rs := range eb.Body {
					rb, ok := rs.(*ast.BlockStmt)
					if !ok || len(rb.Name) != 1 || rb.Name[0] != "write_relabel_config" {
						continue
					}
					r, err := alloyRelabelRule(rb)
					if err != nil {
						return nil, fmt.Errorf("%s: %w", name, err)
					}
					rules = append(rules, r)
				}
				if v, ok := attrOf(eb.Body, "name"); ok {
					if s, ok := literal(v); ok {
						name = fmt.Sprintf("%s endpoint %q", comp, s)
					}
				}
				targets = append(targets, writeTarget{name: name, rules: rules, tier: ix.dormantTenant != "" && alloyTenant(src, eb) == ix.dormantTenant})
			}
			out = append(out, writeTargetEffects(p, path, targets, ix, blocks)...)
		case name == "loki.relabel":
			out = append(out, lokiRelabelEffects(p, path, comp, b, ix, blocks)...)
		case name == "loki.process":
			out = append(out, lokiProcessEffects(p, path, comp, b.Body, nil, ix, blocks, cold)...)
		case strings.HasPrefix(name, "otelcol.processor."):
			typ := strings.TrimPrefix(name, "otelcol.processor.")
			line := ast.StartPos(b).Position().Line
			switch typ {
			case "probabilistic_sampler", "tail_sampling", "filter":
				// Alloy's components take any signal; tvt can't see from
				// the block alone which pipeline carries spans, so any
				// span-dropping component may drop record spans.
				out = append(out, effect{rule: ruleDropMarker, pipeline: p.Name, file: path, component: comp,
					reason: typ, at: fmt.Sprintf("line %d", line), block: blockAt(blocks, line)})
				if len(ix.spans) > 0 {
					out = append(out, effect{rule: RuleRecord, pipeline: p.Name, file: path, component: comp,
						reason: fmt.Sprintf("%s in an Alloy pipeline may drop record spans (%s): check doesn't evaluate Alloy's otelcol conditions yet", typ, recordSpanText(ix)),
						at:     fmt.Sprintf("line %d", line), block: blockAt(blocks, line)})
				}
			}
		}
	}
	return out, nil
}

// alloyTenantRe finds X-Scope-OrgID in an endpoint's headers object.
var alloyTenantRe = regexp.MustCompile(`"X-Scope-OrgID"\s*=\s*"([^"]*)"`)

// alloyTenant is the tenant an Alloy remote_write endpoint sends to, from
// a literal X-Scope-OrgID header; empty when it has none.
func alloyTenant(src []byte, eb *ast.BlockStmt) string {
	v, ok := attrOf(eb.Body, "headers")
	if !ok {
		return ""
	}
	start, end := ast.StartPos(v).Position().Offset, ast.EndPos(v).Position().Offset
	if start < 0 || end >= len(src) || start > end {
		return ""
	}
	if m := alloyTenantRe.FindSubmatch(src[start : end+1]); m != nil {
		return string(m[1])
	}
	return ""
}

func recordSpanText(ix *index) string {
	var ss []string
	for _, o := range ix.spans {
		ss = append(ss, o.Where)
	}
	return strings.Join(ss, " or ")
}

func alloyRelabelRule(b *ast.BlockStmt) (relabelRule, error) {
	var source []string
	vals := map[string]string{}
	unreadable := ""
	for _, st := range b.Body {
		a, ok := st.(*ast.AttributeStmt)
		if !ok {
			continue
		}
		n := a.Name.Name
		if n == "source_labels" {
			arr, ok := a.Value.(*ast.ArrayExpr)
			if !ok {
				unreadable = n
				continue
			}
			for _, el := range arr.Elements {
				v, ok := literal(el)
				if !ok {
					unreadable = n
					continue
				}
				source = append(source, v)
			}
			continue
		}
		v, ok := literal(a.Value)
		if !ok {
			unreadable = n + " isn't a literal"
			continue
		}
		vals[n] = v
	}
	line := ast.StartPos(b).Position().Line
	if unreadable != "" {
		return relabelRule{Action: strings.ToLower(vals["action"]), Target: vals["target_label"], Line: line, Unreadable: unreadable}, nil
	}
	return newRule(source, vals["separator"], vals["regex"], vals["action"], vals["target_label"], vals["replacement"], line)
}

func literal(e ast.Expr) (string, bool) {
	l, ok := e.(*ast.LiteralExpr)
	if !ok {
		return "", false
	}
	if strings.HasPrefix(l.Value, `"`) || strings.HasPrefix(l.Value, "`") {
		v, err := strconv.Unquote(l.Value)
		return v, err == nil
	}
	return l.Value, true
}

func attrOf(body ast.Body, name string) (ast.Expr, bool) {
	for _, st := range body {
		if a, ok := st.(*ast.AttributeStmt); ok && a.Name.Name == name {
			return a.Value, true
		}
	}
	return nil, false
}

// lokiProcessEffects: stage.drop drops lines; stage.match with action
// "drop" drops the lines its selector picks; a stage.match whose stages
// drop is scoped to its selector. scope is the enclosing selector.
func lokiProcessEffects(p report.PipelineDef, path, comp string, body ast.Body, scope []matcher, ix *index, blocks []blockSpan, cold []relabelRule) []effect {
	var out []effect
	for _, st := range body {
		b, ok := st.(*ast.BlockStmt)
		if !ok {
			continue
		}
		name := strings.Join(b.Name, ".")
		line := ast.StartPos(b).Position().Line
		at := fmt.Sprintf("line %d", line)
		switch name {
		case "stage.match":
			sel := scope
			text := "every stream"
			if e, ok := attrOf(b.Body, "selector"); ok {
				s, ok := literal(e)
				if !ok {
					out = append(out, effect{rule: RuleFailClosed, pipeline: p.Name, file: path, component: comp, reason: "a stage.match selector that isn't a literal", at: at})
					continue
				}
				ms, ok := selector(s)
				if !ok {
					out = append(out, effect{rule: RuleFailClosed, pipeline: p.Name, file: path, component: comp, reason: "a stage.match selector check can't read: " + s, at: at})
					continue
				}
				sel, text = append(append([]matcher{}, scope...), ms...), s
			}
			action := "keep"
			if e, ok := attrOf(b.Body, "action"); ok {
				action, _ = literal(e)
			}
			if action == "drop" {
				out = append(out, streamDropEffects(p, path, comp, "drops the lines of "+text, sel, at, blockAt(blocks, line), ix, lineFilterLevel(text))...)
				if !lineFilterGuard(text) {
					out = append(out, guardEffect(p, path, comp, "stage.match drops "+text+" with no !~ line filter keeping the error pattern", at, blockAt(blocks, line)))
				}
				if p.Cold != "" && !reroutedTo(cold, sel) {
					out = append(out, effect{rule: RuleLeastLossy, pipeline: p.Name, file: path, component: comp,
						reason: "drops the lines of " + text + " outright: reroute them to " + p.Cold + " first, and drop after the quiet period (ADR 0010: drops are staged)", at: at, block: blockAt(blocks, line)})
				}
				continue
			}
			out = append(out, lokiProcessEffects(p, path, comp, b.Body, sel, ix, blocks, cold)...)
		case "stage.drop":
			text := "drops lines (stage.drop)"
			out = append(out, streamDropEffects(p, path, comp, text, scope, at, blockAt(blocks, line), ix, false)...)
			out = append(out, guardEffect(p, path, comp, "stage.drop can't keep the error pattern: drop with stage.match and a !~ line filter instead", at, blockAt(blocks, line)))
			if p.Cold != "" {
				out = append(out, effect{rule: RuleLeastLossy, pipeline: p.Name, file: path, component: comp,
					reason: "stage.drop drops lines outright: reroute them to " + p.Cold + " first (ADR 0010: drops are staged)", at: at, block: blockAt(blocks, line)})
			}
		case "stage.sampling":
			out = append(out, streamDropEffects(p, path, comp, "samples lines (stage.sampling)", scope, at, blockAt(blocks, line), ix, false)...)
			out = append(out, guardEffect(p, path, comp, "stage.sampling can't keep the error pattern: sample inside a stage.match with a !~ line filter", at, blockAt(blocks, line)))
		}
	}
	return out
}

// belowWarn says the step takes only lines naming a level below warn (a
// level tier's line filter, S10 item 3): readers that keep to warn and
// above lose nothing.
func streamDropEffects(p report.PipelineDef, path, comp, what string, sel []matcher, at, block string, ix *index, belowWarn bool) []effect {
	out := []effect{{rule: ruleDropMarker, pipeline: p.Name, file: path, component: comp, reason: what, at: at, block: block}}
	for _, s := range ix.read[p.Name] {
		if s.Kind != "stream" {
			continue
		}
		if ms, ok := selector(s.Name); ok && !mayOverlap(ms, sel) {
			continue
		}
		if belowWarn && s.WarnPlus {
			continue
		}
		out = append(out, effect{rule: RuleLeastLossy, pipeline: p.Name, file: path, component: comp, signal: s.ID,
			reason: what + ": " + lessLossy(s), at: at, block: block})
	}
	if selectsWarnPlus(sel) && !belowWarn {
		out = append(out, effect{rule: RuleWarnLogs, pipeline: p.Name, file: path, component: comp,
			reason: what + ", and its selector doesn't rule out warn-and-above lines", at: at, block: block})
	}
	for _, s := range ix.of(p.Name, "stream") {
		ms, ok := selector(s.Name)
		if ok && !mayOverlap(ms, sel) || belowWarn && s.WarnPlus && s.Record == "" {
			continue
		}
		out = append(out, effect{rule: ruleFor(s), pipeline: p.Name, file: path, component: comp, signal: s.ID,
			reason: fmt.Sprintf("%s, which may include %s, %s", what, s.Name, why(s)), at: at, block: block})
	}
	return out
}

// lokiRelabelEffects: a loki.relabel component's drop rules take streams
// out of whatever it forwards to; its keep rules do too when it forwards
// to a loki.write (a keep in front of a cold tier only narrows the cold
// copy).
func lokiRelabelEffects(p report.PipelineDef, path, comp string, b *ast.BlockStmt, ix *index, blocks []blockSpan) []effect {
	toLoki := false
	if e, ok := attrOf(b.Body, "forward_to"); ok {
		if arr, ok := e.(*ast.ArrayExpr); ok {
			for _, el := range arr.Elements {
				// The cold tier (pipelines[].cold, such as loki.write.cold)
				// isn't Loki's hot side: a keep in front of it only narrows
				// the cold copy.
				if n, ok := refText(el); ok && strings.HasPrefix(n, "loki.write.") && strings.TrimSuffix(n, ".receiver") != p.Cold {
					toLoki = true
				}
			}
		}
	}
	var out []effect
	for _, rs := range b.Body {
		rb, ok := rs.(*ast.BlockStmt)
		if !ok || len(rb.Name) != 1 || rb.Name[0] != "rule" {
			continue
		}
		r, err := alloyRelabelRule(rb)
		if err != nil {
			out = append(out, effect{rule: RuleFailClosed, pipeline: p.Name, file: path, component: comp, reason: err.Error()})
			continue
		}
		if r.Action != "drop" && !(r.Action == "keep" && toLoki) {
			continue
		}
		at := fmt.Sprintf("line %d", r.Line)
		block := blockAt(blocks, r.Line)
		what := "drops streams (" + r.text() + ")"
		out = append(out, effect{rule: ruleDropMarker, pipeline: p.Name, file: path, component: comp, reason: what, at: at, block: block})
		out = append(out, guardEffect(p, path, comp, what+": a loki.relabel rule drops whole streams and can't keep the error pattern (use loki.process stage.match with a !~ line filter)", at, block))
		for _, s := range ix.of(p.Name, "stream") {
			if streamRuleHits(r, s.Name) == 0 {
				continue
			}
			out = append(out, effect{rule: ruleFor(s), pipeline: p.Name, file: path, component: comp, signal: s.ID,
				reason: fmt.Sprintf("%s, which may take %s out of Loki, %s", what, s.Name, why(s)), at: at, block: block})
		}
		if r.Unreadable == "" && r.Action == "drop" {
			if streamRuleWarn(r) {
				out = append(out, effect{rule: RuleWarnLogs, pipeline: p.Name, file: path, component: comp,
					reason: what + ", which may match warn-and-above streams", at: at, block: block})
			}
		}
	}
	return out
}

// streamRuleHits: 0 when the rule surely doesn't act on the stream, 1
// when it may.
func streamRuleHits(r relabelRule, sel string) int {
	if r.Unreadable != "" {
		return 1
	}
	ms, ok := selector(sel)
	if !ok {
		return 1
	}
	// A literal regex over the source labels (tvt's own stream rules):
	// each part must be a value the selector's matchers on that label
	// allow, or the rule can't act on the stream.
	if lit, ok := literalRegex(r.RegexText); ok && r.Action == "drop" {
		parts := strings.Split(lit, r.Separator)
		if len(parts) == len(r.Source) {
			for i, l := range r.Source {
				for _, m := range ms {
					if m.Label == l && !m.matches(parts[i]) {
						return 0
					}
				}
			}
			return 1
		}
	}
	known := map[string]string{}
	for _, m := range ms {
		if m.Op == "=" {
			known[m.Label] = m.Value
		}
	}
	src := ""
	for i, l := range r.Source {
		v, ok := known[l]
		if !ok {
			return 1
		}
		if i > 0 {
			src += r.Separator
		}
		src += v
	}
	if r.Regex.MatchString(src) == (r.Action == "drop") {
		return 1
	}
	return 0
}

// streamRuleWarn: a drop rule that reads a level label and whose regex
// could let a warn-and-above value through: it names one, or has a
// wildcard. (tvt's own stream rules pin the level exactly.)
func streamRuleWarn(r relabelRule) bool {
	reads := false
	for _, l := range r.Source {
		switch l {
		case "level", "severity", "detected_level":
			reads = true
		}
	}
	if !reads {
		return false
	}
	re := strings.ToLower(r.RegexText)
	if strings.Contains(re, ".*") || strings.Contains(re, ".+") {
		return true
	}
	for _, lv := range []string{"warn", "error", "fatal", "critical"} {
		if strings.Contains(re, lv) {
			return true
		}
	}
	return false
}

func refText(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.IdentifierExpr:
		return x.Ident.Name, true
	case *ast.AccessExpr:
		v, ok := refText(x.Value)
		if !ok {
			return "", false
		}
		return v + "." + x.Name.Name, true
	}
	return "", false
}

// literalRegex: the regex matches exactly one string, which it returns.
func literalRegex(re string) (string, bool) {
	r, err := syntax.Parse(re, syntax.Perl)
	if err != nil {
		return "", false
	}
	r = r.Simplify()
	if r.Op == syntax.OpLiteral && r.Flags&syntax.FoldCase == 0 {
		return string(r.Rune), true
	}
	if r.Op == syntax.OpEmptyMatch {
		return "", true
	}
	return "", false
}

func guardEffect(p report.PipelineDef, path, comp, reason, at, block string) effect {
	return effect{rule: RuleErrorGuard, pipeline: p.Name, file: path, component: comp,
		reason: reason + " (S8 item 2: every log drop, sample or reroute keeps lines matching " + guardText + ")", at: at, block: block}
}

const guardText = "error|exception|traceback|critical|fatal"

// coldKeeps are the keep rules of loki.relabel components that forward
// to the pipeline's cold tier: the streams tvt's reroute copies there.
func coldKeeps(f *ast.File, cold string) []relabelRule {
	if cold == "" {
		return nil
	}
	var out []relabelRule
	for _, st := range f.Body {
		b, ok := st.(*ast.BlockStmt)
		if !ok || strings.Join(b.Name, ".") != "loki.relabel" {
			continue
		}
		toCold := false
		if e, ok := attrOf(b.Body, "forward_to"); ok {
			if arr, ok := e.(*ast.ArrayExpr); ok {
				for _, el := range arr.Elements {
					if n, ok := refText(el); ok && strings.TrimSuffix(n, ".receiver") == cold {
						toCold = true
					}
				}
			}
		}
		if !toCold {
			continue
		}
		for _, rs := range b.Body {
			rb, ok := rs.(*ast.BlockStmt)
			if !ok || len(rb.Name) != 1 || rb.Name[0] != "rule" {
				continue
			}
			if r, err := alloyRelabelRule(rb); err == nil && r.Unreadable == "" && r.Action == "keep" {
				out = append(out, r)
			}
		}
	}
	return out
}

// reroutedTo: a cold keep rule copies the stream a drop selector names.
func reroutedTo(keeps []relabelRule, sel []matcher) bool {
	known := map[string]string{}
	for _, m := range sel {
		if m.Op == "=" {
			known[m.Label] = m.Value
		}
	}
	for _, r := range keeps {
		v := ""
		for i, l := range r.Source {
			if i > 0 {
				v += r.Separator
			}
			v += known[l]
		}
		if r.Regex.MatchString(v) {
			return true
		}
	}
	return false
}
