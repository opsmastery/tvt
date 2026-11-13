package parse

import (
	"strings"
	"unicode/utf8"

	"github.com/prometheus/prometheus/promql/parser"
)

// UsesAbsence reports whether a rule query's result comes from absent()
// or absent_over_time() (Sprint 2 item 18): absent(m), absent(m) == 1,
// x > 10 or absent(m), sum(absent_over_time(m[10m])). Such a query
// returns no series by design while its metric is healthy, so Sprint 6's
// "rule with no series for 30 days" finding must not call it broken.
// Calling absent() somewhere else doesn't count: in x > 10 and absent(m)
// the result is x's, and an x that never exists is still a broken rule.
//
// The query reads its metric and the labels it matches like any other:
// those are hard keeps either way.
//
// A query the PromQL parser accepts (Grafana variables substituted) is
// read from its syntax tree. Anything else (MetricsQL, LogQL, templates)
// counts only when its outermost call is absent() or absent_over_time(),
// in any case, as MetricsQL function names are case-insensitive.
func UsesAbsence(query string) bool {
	if strings.TrimSpace(query) == "" {
		return false
	}
	if utf8.ValidString(query) {
		text, _ := substituteGrafanaVars(query)
		p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
		if expr, err := p.ParseExpr(text); err == nil {
			return absenceResult(expr)
		}
	}
	return outerAbsence(query)
}

func isAbsence(name string) bool {
	return strings.EqualFold(name, "absent") || strings.EqualFold(name, "absent_over_time")
}

// absenceResult: the series e returns come from absent().
func absenceResult(e parser.Node) bool {
	switch n := e.(type) {
	case *parser.Call:
		if isAbsence(n.Func.Name) {
			return true
		}
		// Functions that pass their vector argument's series through
		// (label_replace, sort, clamp, ...).
		for _, a := range n.Args {
			if a.Type() == parser.ValueTypeVector {
				return absenceResult(a)
			}
		}
		return false
	case *parser.ParenExpr:
		return absenceResult(n.Expr)
	case *parser.StepInvariantExpr:
		return absenceResult(n.Expr)
	case *parser.AggregateExpr:
		return absenceResult(n.Expr)
	case *parser.BinaryExpr:
		switch {
		case n.Op == parser.LOR:
			// Either side's series: absent() makes the quiet side by design.
			return absenceResult(n.LHS) || absenceResult(n.RHS)
		case n.Op == parser.LAND || n.Op == parser.LUNLESS:
			return absenceResult(n.LHS)
		case n.LHS.Type() != parser.ValueTypeVector:
			return absenceResult(n.RHS)
		case n.RHS.Type() != parser.ValueTypeVector:
			return absenceResult(n.LHS)
		default:
			// vector op vector: a series needs a match on both sides.
			return absenceResult(n.LHS) && absenceResult(n.RHS)
		}
	}
	return false
}

// outerAbsence: the first token outside comments and opening parentheses
// is absent or absent_over_time, called.
func outerAbsence(q string) bool {
	i := 0
	for {
		i = skipSpace(q, i)
		if i < len(q) && q[i] == '(' {
			i++
			continue
		}
		break
	}
	start := i
	for i < len(q) && isIdentByte(q[i]) {
		i++
	}
	if !isAbsence(q[start:i]) {
		return false
	}
	i = skipSpace(q, i)
	return i < len(q) && q[i] == '('
}
