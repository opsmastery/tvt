package parse

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// PromQL returns one Ref per selector in a PromQL query, with the labels
// the query matches, groups, joins or drops on.
//
// Grafana template variables are substituted first (see
// substituteGrafanaVars). A variable in a metric name or label name makes
// that Ref Assumed, with the reason; a templated metric name becomes a
// __name__ matcher holding the name as written, with Variable set, so a
// caller that knows the variable's values can resolve it. A variable in a
// matcher value only
// sets Matcher.Variable: the metric and label are still exact, and an
// assumed Ref covers every label, which would blind label-level analysis
// on every templated dashboard. Built-in time variables ($__interval,
// $__rate_interval, $__range) and variables used as a range or offset
// change nothing about which series or labels are read.
//
// Labels an enclosing expression needs apply to every selector beneath it,
// up to the nearest aggregation that keeps only by(...) labels (sum, avg,
// count, ...): above that, only those labels exist. without(...) and
// topk/bottomk/limitk keep the input's labels, so the outer needs pass
// through. Grouping can name a label a selector doesn't have; that keeps a
// label too often but never drops one a query needs. A classic histogram
// under histogram_quantile depends on "le".
//
// A query that doesn't parse is never "references nothing": PromQL then
// returns the parse error and, with it, an assumed AllLabels Ref for every
// metric name the text could hold (or one matching every metric when it
// holds none). An empty query returns no Refs and no error.
func PromQL(query string) ([]Ref, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	text, vars := substituteGrafanaVars(query)
	var expr parser.Expr
	var err error
	if utf8.ValidString(query) {
		p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
		expr, err = p.ParseExpr(text)
	} else {
		// The parser would decode invalid bytes to U+FFFD and report names
		// that aren't in the query. Dashboards and rules are UTF-8 text.
		err = errors.New("query is not valid UTF-8")
	}
	if err != nil {
		ptext, pvars := placeholderVars(query)
		return fallbackRefs(ptext, pvars, err), fmt.Errorf("promql: %w", err)
	}
	w := promWalker{vars: vars}
	w.walk(expr, scope{})
	return w.refs, nil
}

// scope is what enclosing expressions impose on the selectors beneath.
type scope struct {
	grouping, without, created []string
	reasons                    []string
	subquery, allLabels        bool
}

func (s scope) with(f func(*scope)) scope {
	s.grouping = slices.Clone(s.grouping)
	s.without = slices.Clone(s.without)
	s.created = slices.Clone(s.created)
	s.reasons = slices.Clone(s.reasons)
	f(&s)
	return s
}

type promWalker struct {
	vars []grafanaVar
	refs []Ref
}

// labelNames resolves label names written in the query. A name built from
// a variable could be any label: it is dropped and the scope marked
// AllLabels with the reason.
func (w *promWalker) labelNames(s *scope, names []string, what string) []string {
	var out []string
	for _, n := range names {
		if restored, found := restoreVars(n, w.vars); len(found) > 0 {
			s.allLabels = true
			s.reasons = append(s.reasons, fmt.Sprintf("%s label %s comes from variable %s", what, restored, varTexts(found)))
			continue
		}
		out = append(out, n)
	}
	return out
}

func (w *promWalker) walk(node parser.Node, s scope) {
	switch n := node.(type) {
	case nil:
	case *parser.ParenExpr:
		w.walk(n.Expr, s)
	case *parser.UnaryExpr:
		w.walk(n.Expr, s)
	case *parser.StepInvariantExpr:
		w.walk(n.Expr, s)
	case *parser.MatrixSelector:
		w.walk(n.VectorSelector, s)
	case *parser.SubqueryExpr:
		w.walk(n.Expr, s.with(func(s *scope) { s.subquery = true }))
	case *parser.NumberLiteral, *parser.StringLiteral:
	case *parser.AggregateExpr:
		// A parameter (topk's k, quantile's φ) is evaluated on its own,
		// outside the grouping.
		w.walk(n.Param, s)
		inner := s
		if !n.Without && !keepsInputLabels(n.Op) {
			// Only the by(...) labels survive this aggregation, so what
			// enclosing expressions need can't reach below it.
			inner = scope{subquery: s.subquery}
		}
		w.walk(n.Expr, inner.with(func(s *scope) {
			labels := w.labelNames(s, n.Grouping, "grouping")
			if n.Without {
				s.without = append(s.without, labels...)
			} else {
				s.grouping = append(s.grouping, labels...)
			}
			if n.Op == parser.COUNT_VALUES {
				if v, ok := stringArg(n.Param); ok {
					s.created = append(s.created, w.labelNames(s, []string{v}, "created")...)
				}
			}
		}))
	case *parser.BinaryExpr:
		lhs, rhs := s, s
		vm := n.VectorMatching
		if vm != nil && n.LHS.Type() == parser.ValueTypeVector && n.RHS.Type() == parser.ValueTypeVector {
			both := s.with(func(s *scope) {
				labels := w.labelNames(s, vm.MatchingLabels, "matching")
				if vm.On {
					s.grouping = append(s.grouping, labels...)
				} else {
					// Default matching, ignoring(...) and set operators
					// without on(...) match on every other label.
					s.without = append(s.without, labels...)
					s.allLabels = true
				}
			})
			lhs, rhs = both, both
			include := func(s *scope) {
				s.grouping = append(s.grouping, w.labelNames(s, vm.Include, "group_left/group_right")...)
			}
			switch vm.Card {
			case parser.CardManyToOne: // group_left: labels come from the right
				rhs = both.with(include)
			case parser.CardOneToMany: // group_right: from the left
				lhs = both.with(include)
			}
		}
		w.walk(n.LHS, lhs)
		w.walk(n.RHS, rhs)
	case *parser.Call:
		w.walkCall(n, s)
	case *parser.VectorSelector:
		w.refs = append(w.refs, w.selectorRef(n, s))
	}
}

func (w *promWalker) walkCall(n *parser.Call, s scope) {
	name := ""
	if n.Func != nil {
		name = n.Func.Name
	}
	switch {
	case (name == "label_replace" && len(n.Args) == 5) || (name == "label_join" && len(n.Args) >= 3):
		inner := s.with(func(s *scope) {
			if dst, ok := stringArg(n.Args[1]); ok {
				s.created = append(s.created, w.labelNames(s, []string{dst}, "created")...)
			}
			srcArgs := n.Args[3:]
			if name == "label_replace" {
				srcArgs = n.Args[3:4]
			}
			for _, a := range srcArgs {
				if src, ok := stringArg(a); ok && src != "" {
					s.grouping = append(s.grouping, w.labelNames(s, []string{src}, "source")...)
				}
			}
		})
		w.walk(n.Args[0], inner)
		return
	case (name == "sort_by_label" || name == "sort_by_label_desc") && len(n.Args) >= 1:
		inner := s.with(func(s *scope) {
			for _, a := range n.Args[1:] {
				if l, ok := stringArg(a); ok {
					s.grouping = append(s.grouping, w.labelNames(s, []string{l}, "sort")...)
				}
			}
		})
		w.walk(n.Args[0], inner)
		return
	case name == "info" && len(n.Args) >= 1:
		w.walkInfo(n, s)
		return
	case name == "histogram_quantile" && len(n.Args) == 2:
		w.walk(n.Args[0], s)
		w.walk(n.Args[1], s.with(func(s *scope) { s.grouping = append(s.grouping, "le") }))
		return
	}
	for _, a := range n.Args {
		w.walk(a, s)
	}
}

// keepsInputLabels: aggregations that return input series unchanged.
func keepsInputLabels(op parser.ItemType) bool {
	switch op {
	case parser.TOPK, parser.BOTTOMK, parser.LIMITK, parser.LIMIT_RATIO:
		return true
	}
	return false
}

// walkInfo handles info(v [, data-label selector]): it joins v with an
// info metric (target_info unless the selector names another) on the
// identifying labels job and instance, and copies the info metric's data
// labels onto the result: all of them with no selector, else only the
// ones the selector names.
func (w *promWalker) walkInfo(n *parser.Call, s scope) {
	id := func(s *scope) { s.grouping = append(s.grouping, "instance", "job") }
	w.walk(n.Args[0], s.with(id))
	infoScope := s.with(id)
	if len(n.Args) < 2 {
		w.refs = append(w.refs, w.selectorRef(&parser.VectorSelector{
			Name:          "target_info",
			LabelMatchers: []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "target_info")},
		}, infoScope.with(func(s *scope) { s.allLabels = true })))
		return
	}
	arg := n.Args[1]
	for {
		if p, ok := arg.(*parser.ParenExpr); ok {
			arg = p.Expr
			continue
		}
		break
	}
	vs, ok := arg.(*parser.VectorSelector)
	if !ok {
		w.walk(n.Args[1], infoScope)
		return
	}
	r := w.selectorRef(vs, infoScope)
	if r.Metric == "" && !slices.ContainsFunc(r.Matchers, func(m Matcher) bool { return m.Label == labels.MetricName }) {
		r.Metric = "target_info"
	}
	w.refs = append(w.refs, r)
}

func stringArg(e parser.Node) (string, bool) {
	for {
		switch n := e.(type) {
		case *parser.ParenExpr:
			e = n.Expr
		case *parser.StepInvariantExpr:
			e = n.Expr
		case *parser.StringLiteral:
			return n.Val, true
		default:
			return "", false
		}
	}
}

var matchOps = map[labels.MatchType]MatchOp{
	labels.MatchEqual:     MatchEqual,
	labels.MatchNotEqual:  MatchNotEqual,
	labels.MatchRegexp:    MatchRegexp,
	labels.MatchNotRegexp: MatchNotRegex,
}

func (w *promWalker) selectorRef(vs *parser.VectorSelector, s scope) Ref {
	r := Ref{Subquery: s.subquery}
	reasons := slices.Clone(s.reasons)
	allLabels := s.allLabels
	for _, m := range vs.LabelMatchers {
		label, found := restoreVars(m.Name, w.vars)
		if len(found) > 0 {
			allLabels = true
			reasons = append(reasons, fmt.Sprintf("matcher label %s comes from variable %s", label, varTexts(found)))
			continue
		}
		if m.Name == labels.MetricName && m.Type == labels.MatchEqual {
			if strings.Contains(m.Value, placeholderPrefix) {
				restored, found := restoreVars(m.Value, w.vars)
				reasons = append(reasons, fmt.Sprintf("metric name %s comes from variable %s", restored, varTexts(found)))
				r.Matchers = append(r.Matchers, Matcher{Label: labels.MetricName, Op: MatchEqual, Value: restored, Variable: true})
				continue
			}
			if r.Metric == "" {
				r.Metric = m.Value
				continue
			}
		}
		value, found := restoreVars(m.Value, w.vars)
		r.Matchers = append(r.Matchers, Matcher{Label: m.Name, Op: matchOps[m.Type], Value: value, Variable: len(found) > 0})
	}
	r.Grouping = sortedSet(s.grouping)
	r.Without = sortedSet(s.without)
	r.Created = sortedSet(s.created)
	r.AllLabels = allLabels
	r.Reasons = sortedSet(reasons)
	r.Assumed = len(r.Reasons) > 0
	return r
}

func sortedSet(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}

func varTexts(vs []grafanaVar) string {
	var t []string
	for _, v := range vs {
		t = append(t, v.text)
	}
	return strings.Join(sortedSet(t), ", ")
}

// Words that can never name a metric in the pinned parser: modifiers that
// only appear between operands, and number literals. Other keywords (and,
// by, offset, range, start, ...) are valid metric names in some positions,
// so the fallback keeps them as candidates; a candidate that names no real
// metric matches nothing.
var neverMetric = map[string]bool{
	"on": true, "ignoring": true, "group_left": true, "group_right": true,
	"bool": true, "atan2": true, "inf": true, "nan": true,
}

// PromQL keywords. A {...} right after one of these is a selector with no
// name in front of it (see followsName).
var promqlKeywords = map[string]bool{
	"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
	"bool": true, "offset": true, "and": true, "or": true, "unless": true, "atan2": true,
	"inf": true, "nan": true, "start": true, "end": true, "step": true, "range": true,
	"smoothed": true, "anchored": true, "fill": true, "fill_left": true, "fill_right": true,
}

var labelListWords = map[string]bool{
	"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
}

// fallbackRefs is the assumed result for a query that didn't parse: every
// name outside strings, braces, brackets and label lists that isn't a
// keyword or a function call could be a metric.
func fallbackRefs(text string, vars []grafanaVar, perr error) []Ref {
	reason := "query did not parse: " + firstLine(perr.Error())
	seen := map[string]bool{}
	var refs []Ref
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		r := Ref{Metric: name, AllLabels: true, Assumed: true, Reasons: []string{reason}}
		if restored, found := restoreVars(name, vars); len(found) > 0 {
			r.Metric = ""
			r.Matchers = []Matcher{{Label: labels.MetricName, Op: MatchRegexp, Value: nameRegex(name)}}
			r.Reasons = append(r.Reasons, fmt.Sprintf("metric name %s comes from variable %s", restored, varTexts(found)))
			slices.Sort(r.Reasons)
		}
		refs = append(refs, r)
	}
	addWildcard := func(why string) {
		if seen["*"] {
			return
		}
		seen["*"] = true
		refs = append(refs, Ref{
			Matchers:  []Matcher{{Label: labels.MetricName, Op: MatchRegexp, Value: ".+"}},
			AllLabels: true, Assumed: true,
			Reasons: []string{reason + " (" + why + ")"},
		})
	}
	addRegex := func(op MatchOp, value string) {
		if op != MatchRegexp {
			addWildcard("a selector excludes metric names, so any other metric")
			return
		}
		if seen["=~"+value] {
			return
		}
		seen["=~"+value] = true
		refs = append(refs, Ref{
			Matchers:  []Matcher{{Label: labels.MetricName, Op: MatchRegexp, Value: value}},
			AllLabels: true, Assumed: true, Reasons: []string{reason},
		})
	}
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			i = stringEnd(text, i)
		case c == '{':
			next := skipOrStep(text, i, '}')
			if next > i+1 && !followsName(text, i) {
				// A selector with no name before it names its metric, if
				// at all, inside the braces.
				ms, _, err := parseSelector(text, i)
				name, op := "", MatchOp("")
				for _, m := range ms {
					if m.Label == labels.MetricName {
						name, op = m.Value, m.Op
						break
					}
				}
				switch {
				case err != nil || name == "" || !utf8.ValidString(text):
					addWildcard("a selector names no metric, so any metric")
				case op == MatchEqual:
					add(name)
				default:
					addRegex(op, name)
				}
			}
			i = next
		case c == '[':
			i = skipOrStep(text, i, ']')
		case c == '#':
			if nl := strings.IndexByte(text[i:], '\n'); nl >= 0 {
				i += nl + 1
			} else {
				i = len(text)
			}
		case c >= '0' && c <= '9' || c == '.':
			// numbers and durations (5m, 1e3, 0x1f)
			for i < len(text) && (isIdentByte(text[i]) || text[i] == '.') {
				i++
			}
		case isIdentByte(c):
			start := i
			for i < len(text) && isIdentByte(text[i]) {
				i++
			}
			word := text[start:i]
			next := i
			for next < len(text) && (text[next] == ' ' || text[next] == '\t' || text[next] == '\n') {
				next++
			}
			switch {
			case labelListWords[word] && next < len(text) && text[next] == '(':
				i = skipTo(text, next, ')')
			case neverMetric[strings.ToLower(word)]:
			case next < len(text) && text[next] == '(' && !strings.Contains(word, placeholderPrefix):
				// function or aggregator call
			default:
				add(word)
			}
		default:
			i++
		}
	}
	if len(refs) == 0 {
		addWildcard("no metric name found, so any metric")
	}
	return refs
}

// followsName: the byte at i comes right after a name (spaces allowed)
// that isn't a PromQL keyword, as in foo{...} or foo {...}.
func followsName(s string, i int) bool {
	j := i
	for j > 0 && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n') {
		j--
	}
	k := j
	for k > 0 && isIdentByte(s[k-1]) {
		k--
	}
	word := s[k:j]
	return word != "" && !(word[0] >= '0' && word[0] <= '9') && !promqlKeywords[strings.ToLower(word)]
}

// skipTo returns the index just past the first close after i that is not
// inside a string, or len(s) when there is none (an unterminated string
// counts as none).
func skipTo(s string, i int, close byte) int {
	j, _ := closeAfter(s, i, close)
	return j
}

func closeAfter(s string, i int, close byte) (int, bool) {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '"', '\'', '`':
			end := stringEnd(s, j)
			if end == len(s) && (end-j < 2 || s[end-1] != s[j]) {
				return len(s), false // unterminated
			}
			j = end - 1
		case close:
			return j + 1, true
		}
	}
	return len(s), false
}

// skipOrStep skips a {...} or [...] group; an unclosed one skips only its
// opening byte, so the names after it are still candidates.
func skipOrStep(s string, i int, close byte) int {
	if j, ok := closeAfter(s, i, close); ok {
		return j
	}
	return i + 1
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
