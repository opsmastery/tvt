package parse

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// Lookback is how far back a PromQL query reads from its evaluation time:
// the longest range plus offset over its selectors, with a subquery's
// range and offset added to everything beneath it (max_over_time(rate(x[5m])[1d:])
// reads a day and five minutes back). A selector with neither reads the
// lookback delta, which is the server's, not the query's: it counts as 0.
// An @ modifier pins a selector to a fixed time, which no retention keeps
// forever, so it is an error, as are a query that doesn't parse and a
// negative offset (reads ahead, never back) that tvt can't place.
//
// A dashboard's time picker and a range query's span aren't in the query:
// callers add them when they know them (S10 item 2).
func Lookback(query string) (time.Duration, error) {
	if strings.TrimSpace(query) == "" {
		return 0, nil
	}
	// A variable as a range or offset ($__range, $window) is the
	// dashboard's choice, not the query's: substituteGrafanaVars would
	// make it 5m, which understates it.
	if m := durationVarRe.FindString(query); m != "" {
		return 0, fmt.Errorf("promql: %q takes its window from a variable", m)
	}
	text, _ := substituteGrafanaVars(query)
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(text)
	if err != nil {
		return 0, fmt.Errorf("promql: %w", err)
	}
	return lookback(expr, 0)
}

var durationVarRe = regexp.MustCompile(`\[[^\]]*\$[^\]]*\]|\boffset\s+-?\$\{?\w+`)

func lookback(node parser.Node, outer time.Duration) (time.Duration, error) {
	var best time.Duration
	var walkErr error
	visit := func(n parser.Node) bool {
		if walkErr != nil {
			return false
		}
		switch n := n.(type) {
		case *parser.SubqueryExpr:
			if n.Timestamp != nil || n.StartOrEnd != 0 {
				walkErr = fmt.Errorf("promql: a subquery pinned with @ reads a fixed time")
				return false
			}
			d, err := lookback(n.Expr, outer+n.Range+n.OriginalOffset)
			if err != nil {
				walkErr = err
				return false
			}
			best = max(best, d)
			return false // its children are done
		case *parser.MatrixSelector:
			vs, ok := n.VectorSelector.(*parser.VectorSelector)
			if !ok {
				return true
			}
			d, err := selectorLookback(vs, outer+n.Range)
			if err != nil {
				walkErr = err
				return false
			}
			best = max(best, d)
			return false
		case *parser.VectorSelector:
			d, err := selectorLookback(n, outer)
			if err != nil {
				walkErr = err
				return false
			}
			best = max(best, d)
		}
		return true
	}
	inspect(node, visit)
	return best, walkErr
}

func selectorLookback(vs *parser.VectorSelector, d time.Duration) (time.Duration, error) {
	if vs.Timestamp != nil || vs.StartOrEnd != 0 {
		return 0, fmt.Errorf("promql: %s is pinned with @ to a fixed time", vs.String())
	}
	if vs.OriginalOffset < 0 {
		return 0, fmt.Errorf("promql: %s has a negative offset", vs.String())
	}
	return d + vs.OriginalOffset, nil
}

// inspect walks node depth first, like parser.Inspect, but lets visit
// stop descent into a node's children by returning false.
func inspect(node parser.Node, visit func(parser.Node) bool) {
	if node == nil || !visit(node) {
		return
	}
	for _, c := range parser.Children(node) {
		inspect(c, visit)
	}
}

// Windows is what a scrape interval change must respect in a PromQL
// query (S10 item 5): Shortest is the shortest range over raw samples
// (rate(x[2m]) needs at least four samples in 2m), zero when there's
// none; Instant is true when a selector is read without a range, which
// sees a series only within the server's lookback delta (5m by default)
// of its last sample. A range or offset from a variable, an @ modifier or
// a parse error is an error, as for Lookback.
type Windows struct {
	Shortest time.Duration
	Instant  bool
}

// QueryWindows reads a query's Windows.
func QueryWindows(query string) (Windows, error) {
	var w Windows
	if strings.TrimSpace(query) == "" {
		return w, nil
	}
	if _, err := Lookback(query); err != nil {
		return w, err
	}
	text, _ := substituteGrafanaVars(query)
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(text)
	if err != nil {
		return w, fmt.Errorf("promql: %w", err)
	}
	inspect(expr, func(n parser.Node) bool {
		switch n := n.(type) {
		case *parser.MatrixSelector:
			if w.Shortest == 0 || n.Range < w.Shortest {
				w.Shortest = n.Range
			}
			return false
		case *parser.VectorSelector:
			w.Instant = true
		}
		return true
	})
	return w, nil
}
