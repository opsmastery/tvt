// Package corpus pulls PromQL expressions out of Grafana dashboards and
// rule files for the parser corpus test and its generator.
package corpus

import "fmt"

// Expr is one expression and where it came from.
type Expr struct {
	Source string `json:"source"`
	Expr   string `json:"expr"`
}

// DashboardExprs returns every "expr" in a decoded dashboard whose
// datasource (its own, or the nearest enclosing one) isn't Loki or another
// non-Prometheus type.
func DashboardExprs(source string, v any) []Expr {
	return DashboardExprsOfType(source, v, "prometheus")
}

// DashboardExprsOfType is DashboardExprs for another datasource type. An
// expression with no datasource type anywhere above it counts as
// Prometheus.
func DashboardExprsOfType(source string, v any, want string) []Expr {
	var out []Expr
	var walk func(v any, path string, dsType string)
	walk = func(v any, path string, dsType string) {
		switch n := v.(type) {
		case map[string]any:
			if ds, ok := n["datasource"].(map[string]any); ok {
				if t, ok := ds["type"].(string); ok && t != "" && t != "datasource" {
					dsType = t
				}
			}
			if e, ok := n["expr"].(string); ok && e != "" && (dsType == want || (dsType == "" && want == "prometheus")) {
				out = append(out, Expr{Source: source + path, Expr: e})
			}
			for k, c := range n {
				walk(c, path+"/"+k, dsType)
			}
		case []any:
			for i, c := range n {
				walk(c, fmt.Sprintf("%s/%d", path, i), dsType)
			}
		}
	}
	walk(v, "", "")
	return out
}
