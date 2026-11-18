package propose

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/prometheus/prometheus/promql/parser"
)

// Replaying protected queries before a label removal (S8 item 4). tvt
// runs each protected query that touches the metric twice: as written,
// and as it reads once the label is gone, which is the query with every
// use of the metric aggregated over the label (sum without(label), around
// the function call when the metric sits in a range). The results are
// compared: series counts, label sets, empty results, and values beyond
// a tolerance. A query that goes empty or changes its grouping blocks the
// removal until the owner accepts it.

// ReplayQueries returns a protected query as written and without the
// label, each starting with collect.ReplayComment so query log readers
// drop them from usage. ok is false when the query doesn't read the
// metric.
func ReplayQueries(expr, metric, label string) (asWritten, without string, ok bool, err error) {
	e, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(expr)
	if err != nil {
		return "", "", false, err
	}
	found := false
	wrap := func(n parser.Expr) parser.Expr {
		return &parser.AggregateExpr{Op: parser.SUM, Expr: n, Grouping: []string{label}, Without: true}
	}
	var rewrite func(n parser.Expr) parser.Expr
	rewrite = func(n parser.Expr) parser.Expr {
		switch x := n.(type) {
		case *parser.VectorSelector:
			if x.Name == metric {
				found = true
				return wrap(x)
			}
		case *parser.Call:
			// A range function over the metric: aggregate its result.
			for _, a := range x.Args {
				if ms, ok := a.(*parser.MatrixSelector); ok {
					if vs, ok := ms.VectorSelector.(*parser.VectorSelector); ok && vs.Name == metric {
						found = true
						return wrap(x)
					}
				}
				if sq, ok := a.(*parser.SubqueryExpr); ok {
					sq.Expr = rewrite(sq.Expr)
				}
			}
			for i, a := range x.Args {
				x.Args[i] = rewrite(a)
			}
		case *parser.AggregateExpr:
			x.Expr = rewrite(x.Expr)
		case *parser.BinaryExpr:
			x.LHS, x.RHS = rewrite(x.LHS), rewrite(x.RHS)
		case *parser.ParenExpr:
			x.Expr = rewrite(x.Expr)
		case *parser.UnaryExpr:
			x.Expr = rewrite(x.Expr)
		case *parser.SubqueryExpr:
			x.Expr = rewrite(x.Expr)
		case *parser.StepInvariantExpr:
			x.Expr = rewrite(x.Expr)
		}
		return n
	}
	out := rewrite(e)
	if !found {
		return "", "", false, nil
	}
	return collect.ReplayComment + "\n" + expr, collect.ReplayComment + "\n" + out.String(), true, nil
}

// ReplaySample is one series of a replayed result.
type ReplaySample struct {
	Labels map[string]string
	Value  float64
}

// ReplayDiff is what removing the label does to one query's result.
type ReplayDiff struct {
	// Blocked says why the removal must wait for the owner: the result
	// goes empty, or its series or label sets change.
	Blocked string
	// Notes are differences that don't block: values beyond the
	// tolerance, a gauge whose series collide.
	Notes []string
}

// CompareReplay compares a query's result as written (a) and without the
// label (b). tolerance is relative (0.01: 1%). gauge says the metric is a
// gauge, where summing over the label may not be what readers mean.
func CompareReplay(a, b []ReplaySample, label string, tolerance float64, gauge bool) ReplayDiff {
	var d ReplayDiff
	switch {
	case len(a) > 0 && len(b) == 0:
		d.Blocked = "the query goes empty without " + label
		return d
	case len(a) != len(b):
		d.Blocked = fmt.Sprintf("the query returns %d series without %s, %d as written: its grouping changes", len(b), label, len(a))
		return d
	}
	key := func(ls map[string]string) string {
		ks := make([]string, 0, len(ls))
		for k, v := range ls {
			ks = append(ks, k+"="+v)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	byKey := map[string]float64{}
	for _, s := range a {
		byKey[key(s.Labels)] = s.Value
	}
	for _, s := range b {
		v, ok := byKey[key(s.Labels)]
		if !ok {
			d.Blocked = "the query's result label sets change without " + label + " (it groups or filters on it)"
			return d
		}
		if diff := math.Abs(s.Value - v); diff > tolerance*math.Max(math.Abs(v), 1e-9) {
			d.Notes = append(d.Notes, fmt.Sprintf("%s: %g without %s, %g as written", key(s.Labels), s.Value, label, v))
		}
	}
	if gauge && len(d.Notes) > 0 {
		d.Notes = append(d.Notes, "a gauge: series that differed only in "+label+" are now summed, which may not be what its readers mean")
	}
	return d
}

// Gauge: by name, a metric that isn't a counter, histogram or summary
// series (no _total, _bucket, _count or _sum).
func Gauge(metric string) bool {
	for _, s := range []string{"_total", "_bucket", "_count", "_sum"} {
		if strings.HasSuffix(metric, s) {
			return false
		}
	}
	return true
}
