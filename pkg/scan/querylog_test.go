package scan

import (
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
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
		// Strings and comments keep their brackets: these select
		// different things and must stay different consumers.
		`rate({__name__=~"metric_[12]"}[5m])`:        `rate({__name__=~"metric_[12]"}[_])`,
		`rate({__name__=~"metric_[34]"}[5m])`:        `rate({__name__=~"metric_[34]"}[_])`,
		`rate({__name__=~'m_[12]'}[1m])`:             `rate({__name__=~'m_[12]'}[_])`,
		"count_over_time({a=\"b\"} |~ `x[12]` [5m])": "count_over_time({a=\"b\"} |~ `x[12]` [_])",
		`count_over_time({a="b"} |~ "q\"[12]" [5m])`: `count_over_time({a="b"} |~ "q\"[12]" [_])`,
		"rate(x[5m]) # was [10m]\n+ rate(y[1m])":     "rate(x[_]) # was [10m]\n+ rate(y[_])",
		`x{a="unterminated [12]`:                     `x{a="unterminated [12]`,
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

// FuzzNormalizeQuery: normalizing never panics, is idempotent, never
// lengthens a query (a range folds to "[_]"), and keeps quoted text.
func FuzzNormalizeQuery(f *testing.F) {
	for _, s := range []string{
		`rate(x[5m])`, `{__name__=~"m_[12]"}`, "sum(rate(x[1h:5m])) # [5m]\n+ y[2d]",
		`'unterminated [5m]`, "`raw [1m]` [3m]", `"esc\"[5m]" [5m]`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		n := normalizeQuery(q)
		if len(n) > len(q) {
			t.Fatalf("normalizeQuery(%q) = %q is longer", q, n)
		}
		if nn := normalizeQuery(n); nn != n {
			t.Fatalf("not idempotent: %q -> %q -> %q", q, n, nn)
		}
	})
}

// Item 5: every logged query is tagged with its client, and Grafana's
// unattributed queries that match a panel or a Grafana-managed rule become
// evidence for it.
func TestLoggedClients(t *testing.T) {
	at := start.Add(-time.Hour)
	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "shop", Title: "Shop", Panels: []collect.Panel{
		{ID: 1, Title: "Requests", Queries: []collect.PanelQuery{{DatasourceType: "prometheus",
			Expr: `sum by (route) (rate(shop_http_requests_total{route=~"$route"}[$__rate_interval]))`}}},
		{ID: 2, Title: "Requests again", Queries: []collect.PanelQuery{{DatasourceType: "prometheus",
			Expr: `sum by (route) (rate(shop_http_requests_total{route=~"$route"}[$__rate_interval]))`}}},
	}}}
	in.Rules = append(in.Rules, collect.Rule{Source: "grafana", Namespace: "f", Group: "g", Name: "Lag", UID: "lag",
		Kind: collect.RuleAlert, Language: "promql", EvaluatedBy: collect.EvaluatedByGrafana,
		Expr:    "max(node_load1)\nsum(node_cpu_seconds_total)",
		Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: "max(node_load1)"}, {DatasourceType: "prometheus", Expr: "sum(node_cpu_seconds_total)"}}})
	in.Clients = config.Clients{Agents: []config.Agent{{Name: "finance", Addresses: []string{"10.0.0.23"}}}}
	in.Queries = []collect.LoggedQuery{
		// Grafana (10.0.0.6) running both panels' query and the rule's.
		{Source: "prometheus", Language: "promql", Expr: `sum by (route) (rate(shop_http_requests_total{route=~".*"}[1m]))`, At: at, Client: "10.0.0.6"},
		{Source: "prometheus", Language: "promql", Expr: "sum(node_cpu_seconds_total)", At: at, Client: "10.0.0.6"},
		// ... and something else from Grafana: Explore.
		{Source: "prometheus", Language: "promql", Expr: "node_load1 > 2", At: at, Client: "10.0.0.6"},
		// Grafana's address a day later, after a restart gave it to
		// someone else: not Grafana any more.
		{Source: "prometheus", Language: "promql", Expr: "node_load1 < 0", At: at.Add(24 * time.Hour), Client: "10.0.0.6"},
		// Another address, no match: another API client.
		{Source: "prometheus", Language: "promql", Expr: "node_load1", At: at, Client: "10.0.0.9"},
		// The configured agent, even when its query matches a panel.
		{Source: "prometheus", Language: "promql", Expr: "sum(node_cpu_seconds_total)", At: at, Client: "10.0.0.23"},
		// Mimir: an MCP user agent, and Grafana's user agent unmatched.
		{Source: "mimir", Language: "promql", Expr: `count by (__name__) ({__name__=~".+"})`, At: at, Client: "sweep-agent/1.0 (mcp)"},
		{Source: "mimir", Language: "promql", Expr: "node_load1 * 2", At: at, Client: "Grafana/12.1.0", Actor: "alice"},
	}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]graph.Consumer{}
	for _, c := range out.Consumers {
		if strings.HasPrefix(c.ID, "qlog:") {
			got[c.ID[:strings.LastIndex(c.ID, ":")]+" "+c.Client] = c
		}
	}
	for _, want := range []struct{ key, name string }{
		{"qlog:prometheus:match:panel:1:shop:1:0 dashboard", "Shop / Requests (prometheus query log)"},
		{"qlog:prometheus:match:panel:1:shop:2:0 dashboard", "Shop / Requests again (prometheus query log)"},
		{"qlog:prometheus:match:rule:grafana:f/g/lag rule", "Lag (evaluations in the prometheus query log)"},
		{"qlog:prometheus explore", "Explore prometheus query"},
		{"qlog:prometheus api", ""}, // two: 10.0.0.9's and 10.0.0.6's a day later
		{"qlog:prometheus:agent:finance agent", "agent finance (prometheus query log)"},
		{"qlog:mimir:agent:sweep-agent agent", "agent sweep-agent (mimir query log)"},
		{"qlog:mimir explore", "Explore mimir query by alice"},
	} {
		c, ok := got[want.key]
		if !ok || (want.name != "" && c.Name != want.name) {
			t.Errorf("%s: %+v (have %v)", want.key, c, keys(got))
		}
	}
	apis := 0
	for _, c := range out.Consumers {
		if c.Client == ClientAPI {
			apis++
		}
	}
	if len(got) != 8 || apis != 2 {
		t.Errorf("%d query-log consumers: %v", len(got), keys(got))
	}
	if c := got["qlog:prometheus:match:panel:1:shop:1:0 dashboard"]; c.Kind != graph.ConsumerDashboardPanel || !c.LastViewed.Equal(at) {
		t.Errorf("matched panel %+v", c)
	}
	if c := got["qlog:prometheus:match:rule:grafana:f/g/lag rule"]; c.Kind != graph.ConsumerAlertRule || c.EvaluatedBy != "grafana" || !c.LastViewed.IsZero() {
		t.Errorf("matched rule %+v", c)
	}
	if c := got["qlog:prometheus:agent:finance agent"]; c.Kind != graph.ConsumerAgentQuery {
		t.Errorf("agent %+v", c)
	}
	// The agent's reference names it as the actor.
	for _, r := range out.References {
		if strings.HasPrefix(r.ConsumerID, "qlog:prometheus:agent:finance:") && r.Actor != "finance" {
			t.Errorf("agent ref actor %q", r.Actor)
		}
	}

	// With Grafana's addresses configured, another address's matching
	// query isn't attributed to the panel.
	in.Clients.Grafana = []string{"10.0.0.6"}
	in.Queries = []collect.LoggedQuery{
		{Source: "prometheus", Language: "promql", Expr: "sum(node_cpu_seconds_total)", At: at, Client: "10.0.0.7"},
	}
	out, err = Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range out.Consumers {
		if strings.HasPrefix(c.ID, "qlog:") && c.Client != ClientAPI {
			t.Errorf("unconfigured address: %+v", c)
		}
	}
}

func TestTemplates(t *testing.T) {
	for _, c := range []struct {
		tmpl, logged string
		want         bool
	}{
		{`rate(x{a=~"$a"}[$__rate_interval])`, `rate(x{a=~"b|c"}[2m])`, true},
		{`rate(x{a=~"$a"}[$__rate_interval])`, `rate(y{a=~"b"}[2m])`, false},
		{`sum(rate({app="x"}[$__auto]))`, `sum(rate({app="x"}[1m]))`, true},
		{`x{a="${a:regex}"} offset [[off]]`, `x{a="q"} offset 1h`, true},
		{`up`, `up`, true},
		{`up`, `up == 1`, false},
		{`a.b+(c)`, `aXb+(c)`, false}, // literal text is quoted
	} {
		tm, ok := newTemplate(graph.Consumer{Expr: c.tmpl})
		if !ok {
			t.Fatalf("%s: no template", c.tmpl)
		}
		if got := tm.match(normalizeQuery(c.logged)); got != c.want {
			t.Errorf("%s ~ %s: %v, want %v", c.tmpl, c.logged, got, c.want)
		}
	}
}
