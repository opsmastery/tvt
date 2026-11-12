package scan

import (
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func TestNormalizeQuery(t *testing.T) {
	for in, want := range map[string]string{
		`rate(x[40s])`:                  `rate(x[_])`,
		`rate(x[1m0s]) / rate(y[ 2m ])`: `rate(x[_]) / rate(y[_])`,
		`max_over_time(x[1h:5m])`:       `max_over_time(x[_])`,
		`max_over_time(x[1h:])`:         `max_over_time(x[_])`,
		`x{le=~"[0-9]+"}`:               `x{le=~"[0-9]+"}`,
		`count_over_time({a="b"}[5m])`:  `count_over_time({a="b"}[_])`,
	} {
		if got := normalizeQuery(in); got != want {
			t.Errorf("normalizeQuery(%s) = %s, want %s", in, got, want)
		}
	}
}

// Query-log entries become consumers: one per distinct query (ranges
// folded), panel consumers when Mimir's headers attribute them, ad hoc
// otherwise. References carry the last time seen and its actor, through
// recording rules too.
func TestLoggedQueries(t *testing.T) {
	t1 := start.Add(-48 * time.Hour)
	t2 := start.Add(-2 * time.Hour)
	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "shop", Title: "Shop", Panels: []collect.Panel{{ID: 4, Title: "Cart"}}}}
	in.Queries = []collect.LoggedQuery{
		{Source: "prometheus", Language: "promql", Expr: "node_load1", At: t1, Client: "10.0.0.9"},
		{Source: "prometheus", Language: "promql", Expr: "sum(rate(shop_http_requests_total[40s]))", At: t1, Client: "10.0.0.6"},
		{Source: "prometheus", Language: "promql", Expr: "sum(rate(shop_http_requests_total[2m]))", At: t2, Client: "10.0.0.6"},
		{Source: "prometheus", Language: "promql", Expr: "route:shop_http_request_duration_seconds:p99_5m > 1", At: t2},
		{Source: "mimir", Language: "promql", Expr: "sum by (op) (rate(shop_cart_operations_total[5m]))", At: t2,
			Actor: "alice", Client: "Grafana/12.1.0", DashboardUID: "shop", PanelID: 4, OrgID: 1},
		{Source: "mimir", Language: "promql", Expr: "sum by (op) (rate(shop_cart_operations_total[1m]))", At: t1,
			Actor: "bob", Client: "Grafana/12.1.0", DashboardUID: "shop", PanelID: 4, OrgID: 1},
		{Source: "loki", Language: "logql", Expr: `{app="shop"} |= "x"`, At: t2},
		{Source: "prometheus", Language: "traceql", Expr: "{}", At: t2},
	}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var adhoc, panels []graph.Consumer
	for _, c := range out.Consumers {
		if !strings.HasPrefix(c.ID, "qlog:") {
			continue
		}
		switch c.Kind {
		case graph.ConsumerAdhocQuery:
			adhoc = append(adhoc, c)
		case graph.ConsumerDashboardPanel:
			panels = append(panels, c)
		default:
			t.Errorf("consumer %+v", c)
		}
	}
	if len(adhoc) != 4 || len(panels) != 1 {
		t.Fatalf("adhoc %v, panels %v", adhoc, panels)
	}
	p := panels[0]
	if p.Name != "Shop / Cart (mimir query log)" || !strings.HasPrefix(p.ID, "qlog:mimir:panel:1:shop:4:") || !p.LastViewed.Equal(t2) ||
		p.Expr != "sum by (op) (rate(shop_cart_operations_total[5m]))" {
		t.Errorf("panel consumer %+v", p)
	}

	byConsumerSignal := map[string]graph.Reference{}
	for _, r := range out.References {
		if strings.HasPrefix(r.ConsumerID, "qlog:") {
			byConsumerSignal[r.SignalID+" "+r.Via+" "+r.ConsumerID[:strings.LastIndex(r.ConsumerID, ":")]] = r
		}
	}
	check := func(key string, seen time.Time, actor string) {
		t.Helper()
		r, ok := byConsumerSignal[key]
		if !ok {
			t.Errorf("missing ref %q; have %v", key, keys(byConsumerSignal))
			return
		}
		if !r.LastSeen.Equal(seen) || r.Actor != actor || r.Evidence != graph.EvidenceObserved {
			t.Errorf("%s: last seen %s by %q (%s), want %s by %q", key, r.LastSeen, r.Actor, r.Evidence, seen, actor)
		}
	}
	check("prom:metric:node_load1  qlog:prometheus", t1, "")
	check("prom:metric:shop_http_requests_total  qlog:prometheus", t2, "")
	check("prom:metric:shop_cart_operations_total  qlog:mimir:panel:1:shop:4", t2, "alice")
	check("prom:label:shop_cart_operations_total:op  qlog:mimir:panel:1:shop:4", t2, "alice")
	check(bucket+" "+recRule+" qlog:prometheus", t2, "")
	check(`loki:stream:{app="shop"}  qlog:loki`, t2, "")
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
