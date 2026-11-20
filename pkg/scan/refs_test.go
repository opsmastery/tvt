package scan

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// refIndex keys references by "signal <- consumer [via]".
func refIndex(out Output) map[string]graph.Reference {
	m := map[string]graph.Reference{}
	for _, r := range out.References {
		k := r.SignalID + " <- " + r.ConsumerID
		if r.Via != "" {
			k += " via " + r.Via
		}
		m[k] = r
	}
	return m
}

func signalIDs(out Output) map[string]graph.Signal {
	m := map[string]graph.Signal{}
	for _, s := range out.Signals {
		m[s.ID] = s
	}
	return m
}

// baseInputs is a minimal scan: the lab's histogram, its recording rule and
// the alert on it, plus whatever a test adds.
func baseInputs() Inputs {
	return Inputs{
		Start: start, Window: 30 * 24 * time.Hour, MetricsSource: "prom", OrgID: 1,
		Metrics: []collect.MetricCardinality{
			{Metric: "shop_http_request_duration_seconds_bucket", ActiveSeries: 600},
			{Metric: "shop_http_requests_total", ActiveSeries: 40},
			{Metric: "shop_cart_operations_total", ActiveSeries: 5091},
			{Metric: "route:shop_http_request_duration_seconds:p99_5m", ActiveSeries: 8},
			{Metric: "node_cpu_seconds_total", ActiveSeries: 16},
			{Metric: "node_load1", ActiveSeries: 1},
		},
		Labels: []collect.LabelCardinality{
			{Metric: "shop_cart_operations_total", Label: "user_id", Values: 2000, ActiveSeries: 5091},
			{Metric: "shop_cart_operations_total", Label: "op", Values: 3, ActiveSeries: 5091},
			{Metric: "shop_http_request_duration_seconds_bucket", Label: "le", Values: 12, ActiveSeries: 600},
			{Metric: "shop_http_request_duration_seconds_bucket", Label: "pod", Values: 4, ActiveSeries: 600},
		},
		Rules: []collect.Rule{
			{Source: "prometheus", Namespace: "shop.yml", Group: "shop-recording", Name: "route:shop_http_request_duration_seconds:p99_5m",
				Kind: collect.RuleRecord, Language: "promql",
				Expr: "histogram_quantile(0.99, sum by (le, route) (rate(shop_http_request_duration_seconds_bucket[5m])))"},
			{Source: "prometheus", Namespace: "shop.yml", Group: "shop-alerts", Name: "ShopHighLatency",
				Kind: collect.RuleAlert, Language: "promql", Expr: "route:shop_http_request_duration_seconds:p99_5m > 0.5"},
		},
	}
}

const (
	recRule   = "rule:prometheus:shop.yml/shop-recording/route:shop_http_request_duration_seconds:p99_5m"
	alertRule = "rule:prometheus:shop.yml/shop-alerts/ShopHighLatency"
	bucket    = "prom:metric:shop_http_request_duration_seconds_bucket"
	p99       = "prom:metric:route:shop_http_request_duration_seconds:p99_5m"
)

// metric -> recording rule -> alert resolves end to end, and so does a
// panel on the recorded series.
func TestRecordingRuleChain(t *testing.T) {
	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "shop", Title: "Shop", Panels: []collect.Panel{{ID: 3, Title: "p99",
		Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: `route:shop_http_request_duration_seconds:p99_5m{route=~"$route"}`}}}}}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	refs := refIndex(out)
	panel := "panel:1:shop:3:0"

	direct := refs[bucket+" <- "+recRule]
	wantDirect := []graph.LabelRef{{Name: "le", Use: graph.LabelGroup}, {Name: "route", Use: graph.LabelGroup}}
	if direct.Evidence != graph.EvidenceDeclared || !reflect.DeepEqual(direct.Labels, wantDirect) {
		t.Errorf("bucket <- recording rule = %+v", direct)
	}
	if r, ok := refs[p99+" <- "+alertRule]; !ok || r.Evidence != graph.EvidenceDeclared {
		t.Errorf("p99 <- alert = %+v, %v", r, ok)
	}
	viaAlert, ok := refs[bucket+" <- "+alertRule+" via "+recRule]
	if !ok || viaAlert.Evidence != graph.EvidenceDeclared || !reflect.DeepEqual(viaAlert.Labels, wantDirect) {
		t.Errorf("bucket <- alert via rule = %+v, %v", viaAlert, ok)
	}
	// The panel's route matcher is on the output; route comes from the
	// input, so the transitive reference carries it too.
	viaPanel, ok := refs[bucket+" <- "+panel+" via "+recRule]
	wantPanel := []graph.LabelRef{{Name: "le", Use: graph.LabelGroup}, {Name: "route", Use: graph.LabelGroup}, {Name: "route", Use: graph.LabelMatch}}
	if !ok || viaPanel.Evidence != graph.EvidenceObserved || !reflect.DeepEqual(viaPanel.Labels, wantPanel) {
		t.Errorf("bucket <- panel via rule = %+v, %v", viaPanel, ok)
	}
	// Label signals: le is read along every path, pod on none.
	if _, ok := refs["prom:label:shop_http_request_duration_seconds_bucket:le <- "+alertRule+" via "+recRule]; !ok {
		t.Error("le label signal not referenced by the alert through the rule")
	}
	for k := range refs {
		if strings.HasPrefix(k, "prom:label:shop_http_request_duration_seconds_bucket:pod ") {
			t.Errorf("pod is read by nothing, but %s", k)
		}
	}
}

// Chains of any depth resolve, and a rule that reads its own output (or a
// cycle) terminates.
func TestRecordingRuleChainDepthAndCycles(t *testing.T) {
	in := baseInputs()
	in.Rules = append(in.Rules,
		collect.Rule{Source: "prometheus", Namespace: "shop.yml", Group: "g", Name: "a:rec", Kind: collect.RuleRecord, Language: "promql",
			Expr: "max(route:shop_http_request_duration_seconds:p99_5m) + b:rec"},
		collect.Rule{Source: "prometheus", Namespace: "shop.yml", Group: "g", Name: "b:rec", Kind: collect.RuleRecord, Language: "promql",
			Expr: "a:rec * 2"},
		collect.Rule{Source: "prometheus", Namespace: "shop.yml", Group: "g", Name: "Deep", Kind: collect.RuleAlert, Language: "promql",
			Expr: "b:rec > 1"},
	)
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	refs := refIndex(out)
	deep := "rule:prometheus:shop.yml/g/Deep"
	for _, k := range []string{
		"prom:metric:b:rec <- " + deep,
		"prom:metric:a:rec <- " + deep + " via rule:prometheus:shop.yml/g/b:rec",
		p99 + " <- " + deep + " via rule:prometheus:shop.yml/g/a:rec",
		bucket + " <- " + deep + " via " + recRule,
	} {
		if _, ok := refs[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	// Recorded outputs nobody collected yet are still signals, so the
	// chain has somewhere to point, but they carry no cost.
	sigs := signalIDs(out)
	if s, ok := sigs["prom:metric:a:rec"]; !ok || s.Kind != graph.KindMetric {
		t.Errorf("a:rec signal = %+v, %v", s, ok)
	}
	for _, c := range out.Costs {
		if c.SignalID == "prom:metric:a:rec" {
			t.Errorf("uncollected a:rec has a cost row %+v", c)
		}
	}
}

func TestExternalPatternMatchesRecordingRuleOutput(t *testing.T) {
	in := baseInputs()
	in.Metrics = append(in.Metrics, collect.MetricCardinality{Metric: "raw_total"})
	in.Rules = append(in.Rules, collect.Rule{
		Source: "prometheus", Namespace: "billing.yml", Group: "billing", Name: "billing:total",
		Kind: collect.RuleRecord, Language: "promql", Expr: "sum(raw_total)",
	})
	in.Rules = append(in.Rules, collect.Rule{
		Source: "prometheus", Namespace: "billing.yml", Group: "billing", Name: "Broken",
		Kind: collect.RuleAlert, Language: "promql", Expr: "billing:total + (",
	})
	in.External = []config.ExternalConsumer{{Name: "billing", Metrics: []string{"billing:total"}}}

	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	refs := refIndex(out)
	rule := "rule:prometheus:billing.yml/billing/billing:total"
	if _, ok := refs["prom:metric:billing:total <- ext:billing"]; !ok {
		t.Fatal("external consumer does not reference the recording-rule output")
	}
	if _, ok := refs["prom:metric:raw_total <- ext:billing via "+rule]; !ok {
		t.Fatal("external consumer does not reference the recording-rule input")
	}
	if _, ok := refs["prom:metric:billing:total <- rule:prometheus:billing.yml/billing/Broken"]; ok {
		t.Fatal("parse failure referenced an uncollected recording-rule output")
	}
	for _, s := range out.Signals {
		if s.ID == "prom:metric:raw_total" && s.Class != "hard_keep" {
			t.Errorf("recording-rule input class = %q, want hard_keep", s.Class)
		}
	}
}

// Label-level refs: a panel that groups by op references the op label
// signal but not user_id; an AllLabels reference covers every label.
func TestLabelSignalRefs(t *testing.T) {
	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "shop", Title: "Shop", Panels: []collect.Panel{
		{ID: 4, Title: "cart", Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: "sum by (op) (rate(shop_cart_operations_total[$__rate_interval]))"}}},
		{ID: 9, Title: "join", Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: "shop_cart_operations_total / shop_cart_operations_total offset 1d"}}},
	}}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	refs := refIndex(out)
	if r, ok := refs["prom:label:shop_cart_operations_total:op <- panel:1:shop:4:0"]; !ok ||
		!reflect.DeepEqual(r.Labels, []graph.LabelRef{{Name: "op", Use: graph.LabelGroup}}) || r.Evidence != graph.EvidenceObserved {
		t.Errorf("op label ref = %+v, %v", r, ok)
	}
	if _, ok := refs["prom:label:shop_cart_operations_total:user_id <- panel:1:shop:4:0"]; ok {
		t.Error("panel 4 aggregates user_id away but references it")
	}
	m := refs["prom:metric:shop_cart_operations_total <- panel:1:shop:9:0"]
	if !m.AllLabels {
		t.Errorf("vector match without on(): %+v, want AllLabels", m)
	}
	for _, l := range []string{"op", "user_id"} {
		if r, ok := refs["prom:label:shop_cart_operations_total:"+l+" <- panel:1:shop:9:0"]; !ok || !r.AllLabels {
			t.Errorf("AllLabels panel does not reference label %s: %+v", l, r)
		}
	}
}

// The memory note for item 3: a query that doesn't parse resolves only
// against collected signals. The fallback names keyword-like words ("and",
// "by", "range"); none may become a signal.
func TestBrokenQueryCreatesNoPhantomSignals(t *testing.T) {
	in := baseInputs()
	in.Rules = append(in.Rules, collect.Rule{Source: "prometheus", Namespace: "x.yml", Group: "x", Name: "Broken",
		Kind: collect.RuleAlert, Language: "promql", Expr: "sum by (route) (rate(shop_http_requests_total[5m]) and range nope_total"})
	in.Dashboards = []collect.Dashboard{{UID: "d", Title: "D", Panels: []collect.Panel{{ID: 1, Title: "broken",
		Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: "rate(node_cpu_seconds_total[5m]"}}}}}}
	before := len(baseInputs().Metrics)
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	sigs := signalIDs(out)
	for _, phantom := range []string{"and", "by", "range", "sum", "rate", "route", "nope_total"} {
		if _, ok := sigs["prom:metric:"+phantom]; ok {
			t.Errorf("broken query created signal prom:metric:%s", phantom)
		}
	}
	var metrics int
	for _, s := range out.Signals {
		if s.Kind == graph.KindMetric {
			metrics++
		}
	}
	if metrics != before {
		t.Errorf("%d metric signals, want the %d collected", metrics, before)
	}
	refs := refIndex(out)
	r, ok := refs["prom:metric:shop_http_requests_total <- rule:prometheus:x.yml/x/Broken"]
	if !ok || !r.Assumed || !r.AllLabels || len(r.Reasons) == 0 || r.Evidence != graph.EvidenceDeclared {
		t.Errorf("broken alert's reference to a collected metric = %+v, %v", r, ok)
	}
	if r, ok := refs["prom:metric:node_cpu_seconds_total <- panel:1:d:1:0"]; !ok || !r.Assumed {
		t.Errorf("broken panel's reference = %+v, %v", r, ok)
	}
}

// An exact name that wasn't collected (a metric that stopped existing) is
// still a signal: the consumer reading it is evidence, and it costs nothing.
func TestExactUncollectedNameIsASignal(t *testing.T) {
	in := baseInputs()
	in.Rules = append(in.Rules, collect.Rule{Source: "prometheus", Namespace: "x.yml", Group: "x", Name: "Gone",
		Kind: collect.RuleAlert, Language: "promql", Expr: "shop_removed_metric > 0"})
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := signalIDs(out)["prom:metric:shop_removed_metric"]; !ok {
		t.Error("exact uncollected name is not a signal")
	}
	if r, ok := refIndex(out)["prom:metric:shop_removed_metric <- rule:prometheus:x.yml/x/Gone"]; !ok || r.Assumed {
		t.Errorf("reference = %+v, %v", r, ok)
	}
}

// __name__ patterns and names built from variables expand to every
// collected metric they match.
func TestNamePatterns(t *testing.T) {
	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "d", Title: "D",
		Variables: []collect.Variable{
			{Name: "metric", Type: "custom", Values: []string{"node_load1", "node_load5"}},
			{Name: "free", Type: "textbox"},
			{Name: "names", Type: "query", DatasourceType: "prometheus", Query: "metrics(^shop_http)"},
		},
		Panels: []collect.Panel{
			{ID: 1, Title: "regex", Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: `{__name__=~"node_.+"}`}}},
			{ID: 2, Title: "custom var", Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: `{__name__="$metric"}`}}},
			{ID: 3, Title: "textbox var", Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: `{__name__=~"$free"}`}}},
			{ID: 4, Title: "name from var", Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: `${free}_seconds_total`}}},
			{ID: 5, Title: "metrics() var", Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: `{__name__=~"$names"}`}}},
			{ID: 6, Title: "no name", Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: `{job="shop"}`}}},
		}}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	byPanel := map[string][]string{}
	assumed := map[string]bool{}
	for _, r := range out.References {
		if strings.HasPrefix(r.SignalID, "prom:metric:") && strings.HasPrefix(r.ConsumerID, "panel:") && r.Via == "" {
			byPanel[r.ConsumerID] = append(byPanel[r.ConsumerID], strings.TrimPrefix(r.SignalID, "prom:metric:"))
			assumed[r.ConsumerID] = r.Assumed
		}
	}
	all := []string{}
	for _, m := range in.Metrics {
		all = append(all, m.Metric)
	}
	sort.Strings(all)
	for panel, want := range map[string]struct {
		names   []string
		assumed bool
	}{
		"panel:1:d:1:0": {[]string{"node_cpu_seconds_total", "node_load1"}, false},
		// node_load5 isn't collected: a variable value never creates a signal.
		// (Assumed: a metric name from a variable is, even when resolved.)
		"panel:1:d:2:0": {[]string{"node_load1"}, true},
		"panel:1:d:3:0": {all, true},
		"panel:1:d:4:0": {[]string{"node_cpu_seconds_total"}, true},
		"panel:1:d:5:0": {[]string{"shop_http_request_duration_seconds_bucket", "shop_http_requests_total"}, false},
		"panel:1:d:6:0": {all, true},
	} {
		got := byPanel[panel]
		sort.Strings(got)
		if !reflect.DeepEqual(got, want.names) || assumed[panel] != want.assumed {
			t.Errorf("%s: %v (assumed %v), want %v (assumed %v)", panel, got, assumed[panel], want.names, want.assumed)
		}
	}
}

// Templated LogQL matchers: known values expand to one stream signal per
// value; unknown values drop the matcher, assumed; none left is every
// stream.
func TestTemplatedStreams(t *testing.T) {
	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "logs", Title: "Logs",
		Variables: []collect.Variable{
			{Name: "level", Type: "custom", Values: []string{"warn", "error"}},
			{Name: "app", Type: "query", DatasourceType: "loki", Query: "label_values(app)"},
			{Name: "env", Type: "custom", Values: []string{"prod"}, IncludeAll: true, AllValue: ".*"},
		},
		Panels: []collect.Panel{
			{ID: 1, Title: "levels", Queries: []collect.PanelQuery{{DatasourceType: "loki", Expr: `{app="shop", level=~"$level"} | logfmt | status >= 500`}}},
			{ID: 2, Title: "app", Queries: []collect.PanelQuery{{DatasourceType: "loki", Expr: `{app="$app", component="api"}`}}},
			{ID: 3, Title: "all", Queries: []collect.PanelQuery{{DatasourceType: "loki", Expr: `{env=~"$env"}`}}},
		},
		Annotations: []collect.Annotation{{Name: "Panics", DatasourceType: "loki", Expr: `{app="shop"} |= "panic"`}},
	}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	refs := refIndex(out)
	for _, lvl := range []string{"warn", "error"} {
		k := `loki:stream:{app="shop", level=~"` + lvl + `"} <- panel:1:logs:1:0`
		r, ok := refs[k]
		if !ok || r.Assumed {
			t.Errorf("%s = %+v, %v", k, r, ok)
			continue
		}
		want := []graph.LabelRef{{Name: "app", Use: graph.LabelMatch}, {Name: "level", Use: graph.LabelMatch}, {Name: "status", Use: graph.LabelExtracted}}
		if !reflect.DeepEqual(r.Labels, want) {
			t.Errorf("%s labels = %+v, want %+v", k, r.Labels, want)
		}
	}
	if r, ok := refs[`loki:stream:{component="api"} <- panel:1:logs:2:0`]; !ok || !r.Assumed || len(r.Reasons) != 1 {
		t.Errorf("unknown $app: %+v, %v", r, ok)
	}
	if r, ok := refs["loki:stream:* <- panel:1:logs:3:0"]; !ok || !r.Assumed {
		t.Errorf("custom All value: %+v, %v", r, ok)
	}
	// The app variable reads the app label on every stream.
	if r, ok := refs["loki:stream:* <- var:1:logs:app"]; !ok || r.Assumed ||
		!reflect.DeepEqual(r.Labels, []graph.LabelRef{{Name: "app", Use: graph.LabelFilter}}) {
		t.Errorf("label_values(app) variable: %+v, %v", r, ok)
	}
	if r, ok := refs[`loki:stream:{app="shop"} <- annot:1:logs:Panics`]; !ok || r.Evidence != graph.EvidenceObserved {
		t.Errorf("annotation: %+v, %v", r, ok)
	}
	if c := consumers(out)["annot:1:logs:Panics"]; c.Kind != graph.ConsumerDashboardAnnotation || c.Name != "Logs / annotation Panics" {
		t.Errorf("annotation consumer = %+v", c)
	}
}

// Datasources tvt doesn't read (Tempo, a SQL database) still give consumers
// marked Unparsed; a query that names no metric exactly gives no references
// (TraceQL's braces mustn't become "every metric").
func TestOtherDatasources(t *testing.T) {
	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "d", Title: "D", Panels: []collect.Panel{{ID: 1, Title: "traces",
		Queries: []collect.PanelQuery{{DatasourceType: "tempo", Expr: `{ resource.service.name = "shop" }`}}}}}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := consumers(out)["panel:1:d:1:0"]; !ok || c.Unparsed != "datasource type tempo" {
		t.Errorf("tempo panel consumer %+v, %v", c, ok)
	}
	for _, r := range out.References {
		if r.ConsumerID == "panel:1:d:1:0" {
			t.Errorf("tempo panel has reference %+v", r)
		}
	}
}

func TestExpandVars(t *testing.T) {
	vars := map[string]varValues{
		"a": {values: []string{"1", "2"}},
		"b": {values: []string{"x"}},
		"c": {any: true},
	}
	for value, want := range map[string][]string{
		"$a":               {"1", "2"},
		"${a:regex}-[[b]]": {"1-x", "2-x"},
		"pre_$a$b":         {"pre_1x", "pre_2x"},
		"plain":            {"plain"},
		"$c":               nil,
		"$missing":         nil,
	} {
		got, ok := expandVars(value, vars, false)
		if ok != (want != nil) || !reflect.DeepEqual(got, want) {
			t.Errorf("expandVars(%q) = %v, %v; want %v", value, got, ok, want)
		}
	}
}

// A templated matcher with unknown values still narrows by its literal
// text: {app="shop-$env"} reads app=~"shop-.*", not every stream.
func TestVarPattern(t *testing.T) {
	for _, tc := range []struct {
		op      parse.MatchOp
		value   string
		pattern string
		narrows bool
	}{
		{parse.MatchEqual, "shop-$env", "shop-.*", true},
		{parse.MatchEqual, "a.b${x}", `a\.b.*`, true},
		{parse.MatchRegexp, "shop-$env", "shop-.*", true},
		// Regex syntax in the literal text: "[.*]" would match one "." or
		// "*", not the value (Copilot on #20).
		{parse.MatchRegexp, "[$env]", "", false},
		{parse.MatchRegexp, "api|$extra", "", false},
		{parse.MatchRegexp, "(a|$x)", "", false},
		{parse.MatchEqual, "$x", "", false},
		{parse.MatchRegexp, "${x}${y}", "", false},
		{parse.MatchNotEqual, "shop-$env", "", false},
	} {
		op, pattern, narrows := varPattern(tc.op, tc.value)
		if narrows != tc.narrows || pattern != tc.pattern || narrows && op != parse.MatchRegexp {
			t.Errorf("varPattern(%s %q) = %s %q %v, want =~ %q %v", tc.op, tc.value, op, pattern, narrows, tc.pattern, tc.narrows)
		}
	}

	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "d", Title: "D",
		Variables: []collect.Variable{{Name: "env", Type: "textbox"}},
		Panels:    []collect.Panel{{ID: 1, Title: "logs", Queries: []collect.PanelQuery{{DatasourceType: "loki", Expr: `{app="shop-$env"}`}}}}}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := refIndex(out)[`loki:stream:{app=~"shop-.*"} <- panel:1:d:1:0`]; !ok || !r.Assumed {
		t.Errorf("narrowed stream: %+v, %v; refs %v", r, ok, out.References)
	}
}

// Review fixes: a metrics() variable references the names Grafana's
// unanchored regex returns; a name plus a templated __name__ matcher with
// unknown values is assumed, with the reason.
func TestVariableAndNameMatcherFixes(t *testing.T) {
	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "d", Title: "D",
		Variables: []collect.Variable{
			{Name: "names", Type: "query", DatasourceType: "prometheus", Query: "metrics(http_request)"},
			{Name: "free", Type: "textbox"},
		},
		Panels: []collect.Panel{{ID: 1, Title: "p", Queries: []collect.PanelQuery{
			{DatasourceType: "prometheus", Expr: `node_load1{__name__=~"node_$free"}`}}}},
	}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	refs := refIndex(out)
	for _, m := range []string{"shop_http_requests_total", "shop_http_request_duration_seconds_bucket"} {
		if _, ok := refs["prom:metric:"+m+" <- var:1:d:names"]; !ok {
			t.Errorf("metrics(http_request) variable does not reference %s", m)
		}
	}
	if _, ok := refs["prom:metric:node_load1 <- var:1:d:names"]; ok {
		t.Error("metrics(http_request) variable references node_load1")
	}
	if r, ok := refs["prom:metric:node_load1 <- panel:1:d:1:0"]; !ok || !r.Assumed || len(r.Reasons) == 0 {
		t.Errorf("name plus templated __name__ matcher = %+v, %v; want assumed with a reason", r, ok)
	}
}

// add never shares slices with the reference it was given or with another
// stored reference, so a later merge can't change a different reference.
func TestAddDoesNotAlias(t *testing.T) {
	r := newResolver("prom", nil, nil)
	reasons := make([]string, 1, 8)
	reasons[0] = "a"
	labels := make([]graph.LabelRef, 1, 8)
	labels[0] = graph.LabelRef{Name: "x", Use: graph.LabelMatch}
	k1 := r.add(graph.Reference{SignalID: "s1", ConsumerID: "c", Evidence: graph.EvidenceObserved, Labels: labels, Reasons: reasons})
	k2 := r.add(graph.Reference{SignalID: "s2", ConsumerID: "c", Evidence: graph.EvidenceObserved, Labels: labels, Reasons: reasons})
	r.add(graph.Reference{SignalID: "s2", ConsumerID: "c", Evidence: graph.EvidenceObserved,
		Labels: []graph.LabelRef{{Name: "y", Use: graph.LabelGroup}}, Reasons: []string{"b"}})
	r.add(graph.Reference{SignalID: "s1", ConsumerID: "c", Evidence: graph.EvidenceObserved,
		Labels: []graph.LabelRef{{Name: "z", Use: graph.LabelGroup}}, Reasons: []string{"c"}})
	if got := r.refs[k1].Reasons; !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("s1 reasons = %v", got)
	}
	if got := r.refs[k2].Reasons; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("s2 reasons = %v", got)
	}
	if got := r.refs[k2].Labels; len(got) != 2 || got[1].Name != "y" {
		t.Errorf("s2 labels = %v", got)
	}
	if reasons[:2][1] != "" || labels[:2][1] != (graph.LabelRef{}) {
		t.Error("add wrote into the caller's backing arrays")
	}
}

// Rules that each read the previous two outputs share inputs on every
// path; following each path separately is Fibonacci work (Copilot on #20).
func TestRecordingRuleChainSharedInputs(t *testing.T) {
	const n = 40
	in := Inputs{Start: start, Window: 30 * 24 * time.Hour, MetricsSource: "prom", OrgID: 1,
		Metrics: []collect.MetricCardinality{{Metric: "base_a", ActiveSeries: 1}, {Metric: "base_b", ActiveSeries: 1}}}
	name := func(i int) string {
		switch i {
		case 0:
			return "base_a"
		case 1:
			return "base_b"
		}
		return fmt.Sprintf("level:%d", i)
	}
	for i := 2; i <= n; i++ {
		in.Rules = append(in.Rules, collect.Rule{Source: "prometheus", Namespace: "r.yml", Group: "g", Name: name(i),
			Kind: collect.RuleRecord, Language: "promql", Expr: fmt.Sprintf("sum by (job) (%s) + sum by (job) (%s)", name(i-1), name(i-2))})
	}
	in.Rules = append(in.Rules, collect.Rule{Source: "prometheus", Namespace: "r.yml", Group: "a", Name: "Top",
		Kind: collect.RuleAlert, Language: "promql", Expr: name(n) + ` > 0`})
	done := make(chan Output, 1)
	go func() {
		out, err := Build(in)
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	var out Output
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Build did not finish: shared rule inputs are followed once per path")
	}
	idx := refIndex(out)
	top := "rule:prometheus:r.yml/a/Top"
	for _, k := range []string{
		"prom:metric:base_a <- " + top + " via rule:prometheus:r.yml/g/level:2",
		"prom:metric:base_b <- " + top + " via rule:prometheus:r.yml/g/level:3",
	} {
		if _, ok := idx[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
}

// Copilot on #20: ${var.field} and Grafana's formats. A field or a format
// tvt doesn't model is unknown; :regex, and the default for multi-value or
// "All" variables, regex-escape each value; a regex literal around an
// unknown variable doesn't narrow.
func TestVariableFormats(t *testing.T) {
	in := baseInputs()
	stream := func(ms ...parse.Matcher) string { return "loki:stream:" + parse.CanonicalSelector(ms) }
	in.Dashboards = []collect.Dashboard{{UID: "d", Title: "D",
		Variables: []collect.Variable{
			{Name: "env", Type: "custom", Values: []string{"prod"}},
			{Name: "app", Type: "custom", Values: []string{"api(v2)"}},
			{Name: "svc", Type: "custom", Values: []string{"a.b", "c"}, Multi: true},
			{Name: "free", Type: "textbox"},
		},
		Panels: []collect.Panel{{ID: 1, Title: "logs", Queries: []collect.PanelQuery{
			{DatasourceType: "loki", Expr: `{job="f", env="${env.text}"}`},
			{DatasourceType: "loki", Expr: `{job="r", app=~"${app:regex}"}`},
			{DatasourceType: "loki", Expr: `{job="w", app=~"$app"}`},
			{DatasourceType: "loki", Expr: `{job="m", svc=~"$svc"}`},
			{DatasourceType: "loki", Expr: `{job="c", svc=~"${svc:csv}"}`},
			{DatasourceType: "loki", Expr: `{job="t", app=~"[$free]"}`},
		}}}}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	idx := refIndex(out)
	eq := func(l, v string) parse.Matcher { return parse.Matcher{Label: l, Op: parse.MatchEqual, Value: v} }
	re := func(l, v string) parse.Matcher { return parse.Matcher{Label: l, Op: parse.MatchRegexp, Value: v} }
	for _, tc := range []struct {
		name, signal string
		consumer     int
		assumed      bool
	}{
		{"field is unknown", stream(eq("job", "f")), 0, true},
		{":regex escapes", stream(eq("job", "r"), re("app", `api\(v2\)`)), 1, false},
		{"single value is raw", stream(eq("job", "w"), re("app", "api(v2)")), 2, false},
		{"multi escapes", stream(eq("job", "m"), re("svc", `a\.b`)), 3, false},
		{"multi, second value", stream(eq("job", "m"), re("svc", "c")), 3, false},
		{"csv is unknown", stream(eq("job", "c")), 4, true},
		{"regex literal doesn't narrow", stream(eq("job", "t")), 5, true},
	} {
		k := fmt.Sprintf("%s <- panel:1:d:1:%d", tc.signal, tc.consumer)
		r, ok := idx[k]
		if !ok || r.Assumed != tc.assumed {
			var have []string
			for _, r := range out.References {
				if strings.HasSuffix(r.ConsumerID, fmt.Sprintf(":%d", tc.consumer)) && r.ConsumerID[:7] == "panel:1" {
					have = append(have, r.SignalID)
				}
			}
			t.Errorf("%s: %s: found %v, assumed %v; consumer has %v", tc.name, k, ok, r.Assumed, have)
		}
	}
}

// Copilot on #20: a label a panel's legend or an annotation shows is read
// (legend), and whether a selector's labels reach the result is recorded
// (ResultLabels) without counting them as read.
func TestShownLabelsAndResultLabels(t *testing.T) {
	in := baseInputs()
	in.Labels = append(in.Labels, collect.LabelCardinality{Metric: "shop_cart_operations_total", Label: "instance", Values: 2, ActiveSeries: 5091})
	in.Dashboards = []collect.Dashboard{{UID: "d", Title: "D",
		Panels: []collect.Panel{{ID: 1, Title: "ops", Queries: []collect.PanelQuery{
			{DatasourceType: "prometheus", Expr: `shop_cart_operations_total{op="add"}`, Legend: "{{ instance }}"},
			{DatasourceType: "prometheus", Expr: `sum by (op) (shop_cart_operations_total)`},
		}}},
		Annotations: []collect.Annotation{{Name: "a", DatasourceType: "prometheus", Expr: `shop_http_requests_total > 0`,
			TitleFormat: "{{route}}", TagKeys: "job, pod"}},
	}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	idx := refIndex(out)
	cart := "prom:metric:shop_cart_operations_total"
	raw, sum := idx[cart+" <- panel:1:d:1:0"], idx[cart+" <- panel:1:d:1:1"]
	if !slices.Contains(raw.Labels, graph.LabelRef{Name: "instance", Use: graph.LabelLegend}) || !raw.ResultLabels {
		t.Errorf("raw panel ref = %+v, want legend instance and ResultLabels", raw)
	}
	if sum.ResultLabels {
		t.Errorf("sum by panel ref = %+v, want no ResultLabels", sum)
	}
	if _, ok := idx["prom:label:shop_cart_operations_total:instance <- panel:1:d:1:0"]; !ok {
		t.Error("legend label has no label-signal reference")
	}
	if _, ok := idx["prom:label:shop_cart_operations_total:user_id <- panel:1:d:1:0"]; ok {
		t.Error("ResultLabels counted user_id as read")
	}
	annot := idx["prom:metric:shop_http_requests_total <- annot:1:d:a"]
	for _, l := range []string{"job", "pod", "route"} {
		if !slices.Contains(annot.Labels, graph.LabelRef{Name: l, Use: graph.LabelLegend}) {
			t.Errorf("annotation ref %+v: missing legend %s", annot, l)
		}
	}
}

// Grafana-managed rules (Sprint 2 item 5): one consumer per rule, keyed by
// UID, tagged evaluated_by=grafana with its interval and datasources. Each
// query resolves in its datasource's language, a query to another kind of
// datasource reads nothing tvt tracks, and a Grafana recording rule's
// output chains to its inputs like any recording rule.
func TestGrafanaManagedRules(t *testing.T) {
	in := baseInputs()
	in.Rules = append(in.Rules,
		collect.Rule{Source: "grafana", Namespace: "f1", Group: "g", Name: "Lag", UID: "lag", Kind: collect.RuleAlert,
			EvaluatedBy: collect.EvaluatedByGrafana, Interval: 30 * time.Second, Expr: "A: …\nB: …\nC: …",
			Queries: []collect.PanelQuery{
				{DatasourceUID: "prometheus", DatasourceType: "prometheus", Expr: "max(node_load1)"},
				{DatasourceUID: "loki", DatasourceType: "loki", Expr: `count_over_time({app="shop"} | json [5m])`},
				{DatasourceUID: "es", DatasourceType: "elasticsearch", Expr: "status:500"},
			}},
		// Same title, another rule: the UID keeps them apart.
		collect.Rule{Source: "grafana", Namespace: "f1", Group: "g", Name: "Lag", UID: "lag-2", Kind: collect.RuleAlert,
			EvaluatedBy: collect.EvaluatedByGrafana, Queries: []collect.PanelQuery{{DatasourceUID: "prometheus", DatasourceType: "prometheus", Expr: "up"}}},
		collect.Rule{Source: "grafana", Namespace: "f1", Group: "rec", Name: "job:cpu:rate5m", UID: "rec", Kind: collect.RuleRecord,
			EvaluatedBy: collect.EvaluatedByGrafana, Interval: time.Minute,
			Queries: []collect.PanelQuery{{DatasourceUID: "prometheus", DatasourceType: "prometheus", Expr: "sum by (job) (rate(node_cpu_seconds_total[5m]))"}}},
	)
	in.Dashboards = []collect.Dashboard{{UID: "d", Title: "D", Panels: []collect.Panel{{ID: 1, Title: "cpu",
		Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: "job:cpu:rate5m"}}}}}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	cons := map[string]graph.Consumer{}
	for _, c := range out.Consumers {
		cons[c.ID] = c
	}
	const lag, rec = "rule:grafana:f1/g/lag", "rule:grafana:f1/rec/rec"
	c := cons[lag]
	if c.Kind != graph.ConsumerAlertRule || c.Name != "Lag" || c.EvaluatedBy != "grafana" || c.Interval != 30*time.Second ||
		!slices.Equal(c.Datasources, []string{"es", "loki", "prometheus"}) {
		t.Errorf("Grafana alert consumer %+v", c)
	}
	if _, ok := cons["rule:grafana:f1/g/lag-2"]; !ok {
		t.Error("second rule titled Lag is missing")
	}
	if c := cons[rec]; c.Kind != graph.ConsumerRecordingRule || c.Interval != time.Minute {
		t.Errorf("Grafana recording rule consumer %+v", c)
	}
	refs := refIndex(out)
	for _, k := range []string{
		"prom:metric:node_load1 <- " + lag,
		`loki:stream:{app="shop"} <- ` + lag,
		"prom:metric:node_cpu_seconds_total <- " + rec,
		"prom:metric:job:cpu:rate5m <- panel:1:d:1:0",
		"prom:metric:node_cpu_seconds_total <- panel:1:d:1:0 via " + rec,
	} {
		r, ok := refs[k]
		if !ok {
			t.Errorf("missing %s", k)
			continue
		}
		if want := cons[r.ConsumerID].Kind.Evidence(); r.Evidence != want {
			t.Errorf("%s: evidence %s, want %s", k, r.Evidence, want)
		}
	}
	// The Elasticsearch query reads nothing: the alert's only references
	// are its Prometheus and Loki queries'.
	for _, r := range out.References {
		if r.ConsumerID == lag && r.SignalID != "prom:metric:node_load1" && r.SignalID != `loki:stream:{app="shop"}` {
			t.Errorf("unexpected reference %s <- %s", r.SignalID, lag)
		}
	}
}

// TestCoarseLabelUse: a status-class matcher is a coarse use (S10 item 1);
// an exact value or a variable stays a match.
func TestCoarseLabelUse(t *testing.T) {
	for q, want := range map[string]graph.LabelUse{
		`sum(rate(http_requests_total{code=~"5.."}[5m]))`: graph.LabelCoarse,
		`sum(rate(http_requests_total{code!~"2.."}[5m]))`: graph.LabelCoarse,
		`sum(rate(http_requests_total{code="500"}[5m]))`:  graph.LabelMatch,
		`sum(rate(http_requests_total{code=~"50."}[5m]))`: graph.LabelMatch,
		`sum(rate(http_requests_total{code=~"$c"}[5m]))`:  graph.LabelMatch,
	} {
		refs, err := parse.PromQL(q)
		if err != nil || len(refs) != 1 {
			t.Fatalf("%s: %v", q, err)
		}
		ls := promLabels(refs[0])
		if len(ls) != 1 || ls[0].Use != want {
			t.Errorf("%s: %v, want %s", q, ls, want)
		}
	}
}
