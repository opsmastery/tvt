package scan

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/graph"
)

var start = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func labInputs() Inputs {
	return Inputs{
		Start:         start,
		Window:        30 * 24 * time.Hour,
		MetricsSource: "prom",
		Metrics: []collect.MetricCardinality{
			{Metric: "shop_cart_operations_total", ActiveSeries: 5091},
			{Metric: "shop_payment_queue_depth", ActiveSeries: 1},
		},
		Labels: []collect.LabelCardinality{
			{Metric: "shop_cart_operations_total", Label: "user_id", Values: 2000, ActiveSeries: 5091},
		},
		Rules: []collect.Rule{
			{Source: "prometheus", Namespace: "/etc/prometheus/rules/shop.yml", Group: "shop-alerts", Name: "ShopPaymentQueueBacklog",
				Kind: collect.RuleAlert, Expr: "shop_payment_queue_depth > 100", Language: "promql"},
			{Source: "prometheus", Namespace: "/etc/prometheus/rules/shop.yml", Group: "shop-recording", Name: "job:shop_cache_hit_ratio:5m",
				Kind: collect.RuleRecord, Expr: "sum(rate(shop_cache_hits_total[5m]))", Language: "promql"},
		},
		OrgID: 1,
		Dashboards: []collect.Dashboard{
			{UID: "shop-overview", Title: "Shop Overview", Panels: []collect.Panel{
				{ID: 4, Title: "Cart operations by type", Queries: []collect.PanelQuery{
					{DatasourceUID: "prometheus", DatasourceType: "prometheus", Expr: "sum by (op) (rate(shop_cart_operations_total[$__rate_interval]))"}}},
			}, Variables: []collect.Variable{
				{Name: "route", Query: "label_values(shop_http_requests_total, route)", DatasourceUID: "prometheus", DatasourceType: "prometheus"}}},
			{UID: "legacy-queue", Title: "Legacy Queue", Panels: []collect.Panel{
				{ID: 1, Title: "Legacy queue depth", Queries: []collect.PanelQuery{{Expr: "shop_legacy_queue_depth"}}}}},
		},
		Views: []collect.DashboardView{
			{DashboardUID: "shop-overview", OrgID: 1, User: "admin", At: start.Add(-2 * time.Hour), Browser: true},
			{DashboardUID: "shop-overview", OrgID: 1, User: "admin", At: start.Add(-time.Hour), Browser: true},
			// A script loading Legacy Queue through the API is evidence, not a view.
			{DashboardUID: "legacy-queue", OrgID: 1, User: "exporter", At: start.Add(-time.Hour), Browser: false},
			// Outside the window, and in another org: neither sets last_viewed.
			{DashboardUID: "legacy-queue", OrgID: 1, User: "ann", At: start.Add(-40 * 24 * time.Hour), Browser: true},
			{DashboardUID: "legacy-queue", OrgID: 2, User: "bob", At: start.Add(-time.Hour), Browser: true},
		},
		Sources: []graph.SourceCoverage{
			{Source: "grafana router log", Configured: true, Coverage: 0.6},
			{Source: "prometheus query log", Configured: false, Coverage: 0.9}, // ignored: not configured
		},
	}
}

func consumers(out Output) map[string]graph.Consumer {
	m := map[string]graph.Consumer{}
	for _, c := range out.Consumers {
		m[c.ID] = c
	}
	return m
}

func TestBuild(t *testing.T) {
	out, err := Build(labInputs())
	if err != nil {
		t.Fatal(err)
	}

	sigs := map[string]graph.Signal{}
	for _, s := range out.Signals {
		sigs[s.ID] = s
	}
	if s := sigs["prom:metric:shop_cart_operations_total"]; s.Kind != graph.KindMetric || s.Source != "prom" {
		t.Errorf("metric signal = %+v", s)
	}
	if s := sigs["prom:label:shop_cart_operations_total:user_id"]; s.Kind != graph.KindLabel || s.Parent != "shop_cart_operations_total" {
		t.Errorf("label signal = %+v", s)
	}
	if len(out.Costs) != 3 || out.Costs[0].ActiveSeries != 5091 || !out.Costs[0].ObservedAt.Equal(start) {
		t.Errorf("costs = %+v", out.Costs)
	}
	if c := out.Costs[2]; c.SignalID != "prom:label:shop_cart_operations_total:user_id" || c.DistinctValues != 2000 {
		t.Errorf("label cost = %+v, want 2000 distinct values", c)
	}

	c := consumers(out)
	alert := c["rule:prometheus:/etc/prometheus/rules/shop.yml/shop-alerts/ShopPaymentQueueBacklog"]
	if alert.Kind != graph.ConsumerAlertRule || alert.Expr != "shop_payment_queue_depth > 100" || !alert.Kind.HardKeep() {
		t.Errorf("alert = %+v", alert)
	}
	if rec := c["rule:prometheus:/etc/prometheus/rules/shop.yml/shop-recording/job:shop_cache_hit_ratio:5m"]; rec.Kind != graph.ConsumerRecordingRule {
		t.Errorf("recording rule = %+v", rec)
	}
	panel := c["panel:1:shop-overview:4:0"]
	if panel.Kind != graph.ConsumerDashboardPanel || panel.Name != "Shop Overview / Cart operations by type" ||
		!panel.LastViewed.Equal(start.Add(-time.Hour)) {
		t.Errorf("shop panel = %+v", panel)
	}
	if v := c["var:1:shop-overview:route"]; v.Kind != graph.ConsumerDashboardVariable || v.Name != "Shop Overview / $route" ||
		!v.LastViewed.Equal(start.Add(-time.Hour)) {
		t.Errorf("variable = %+v", v)
	}
	if legacy := c["panel:1:legacy-queue:1:0"]; !legacy.LastViewed.IsZero() {
		t.Errorf("Legacy Queue viewed: %+v", legacy)
	}

	// Loads: everything in the window, aggregated, API loads included.
	type key struct {
		uid, login string
		browser    bool
		org        int64
	}
	got := map[key]int64{}
	for _, l := range out.Loads {
		got[key{l.DashboardUID, l.Login, l.Browser, l.OrgID}] = l.Loads
	}
	want := map[key]int64{
		{"shop-overview", "admin", true, 1}:    2,
		{"legacy-queue", "exporter", false, 1}: 1,
		{"legacy-queue", "bob", true, 2}:       1,
	}
	if len(got) != len(want) {
		t.Errorf("loads = %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("loads[%v] = %d, want %d", k, got[k], n)
		}
	}

	// Coverage: the unconfigured source counts as 0.
	if out.LogCoverage != 0.3 {
		t.Errorf("log coverage = %v, want 0.3", out.LogCoverage)
	}
	for _, s := range out.Sources {
		if s.Source == "prometheus query log" && s.Coverage != 0 {
			t.Errorf("unconfigured source coverage = %v", s.Coverage)
		}
		if !s.WindowStart.Equal(start.Add(-30 * 24 * time.Hour)) {
			t.Errorf("window start = %v", s.WindowStart)
		}
	}
}

func TestBuildUniqueIDs(t *testing.T) {
	in := labInputs()
	// Prometheus allows two alerts with the same name in one group.
	in.Rules = append(in.Rules, in.Rules[0])
	in.Rules[2].Expr = "shop_payment_queue_depth > 500"
	in.Dashboards[0].Panels = append(in.Dashboards[0].Panels, in.Dashboards[0].Panels[0])
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	c := consumers(out)
	if len(c) != len(out.Consumers) {
		t.Fatalf("duplicate consumer IDs: %d unique of %d", len(c), len(out.Consumers))
	}
	if c["rule:prometheus:/etc/prometheus/rules/shop.yml/shop-alerts/ShopPaymentQueueBacklog#2"].Expr != "shop_payment_queue_depth > 500" {
		t.Error("second alert lost")
	}
	if _, ok := c["panel:1:shop-overview:4:0#2"]; !ok {
		t.Error("repeated panel lost")
	}
}

// A collected name that looks like a generated suffix must not collide.
func TestIDSetReservesSuffixes(t *testing.T) {
	ids := newIDSet()
	var got []string
	for _, id := range []string{"a", "a", "a#2", "a", "a#3", "b"} {
		got = append(got, ids.unique(id))
	}
	want := []string{"a", "a#2", "a#2#2", "a#3", "a#3#2", "b"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, want %v", got, want)
	}

	in := labInputs()
	r := in.Rules[0]
	in.Rules = []collect.Rule{r, r, r}
	in.Rules[2].Name += "#2" // HighLatency, HighLatency, HighLatency#2
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if c := consumers(out); len(c) != len(out.Consumers) {
		t.Errorf("duplicate consumer IDs: %d unique of %d", len(c), len(out.Consumers))
	}
}

func TestBuildMimirAndValidation(t *testing.T) {
	in := labInputs()
	in.MetricsSource = "mimir"
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Signals[0].ID != "mimir:metric:shop_cart_operations_total" || out.Signals[0].Source != "mimir" {
		t.Errorf("signal = %+v", out.Signals[0])
	}

	bad := map[string]func(*Inputs){
		"no start":     func(i *Inputs) { i.Start = time.Time{} },
		"no window":    func(i *Inputs) { i.Window = 0 },
		"bad source":   func(i *Inputs) { i.MetricsSource = "victoria" },
		"orphan label": func(i *Inputs) { i.Labels = append(i.Labels, collect.LabelCardinality{Metric: "nope", Label: "x"}) },
		"coverage > 1": func(i *Inputs) { i.Sources[0].Coverage = 1.5 },
		"source twice": func(i *Inputs) { i.Sources = append(i.Sources, i.Sources[0]) },
	}
	for name, mutate := range bad {
		in := labInputs()
		mutate(&in)
		if _, err := Build(in); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.HasPrefix(err.Error(), "scan: ") {
			t.Errorf("%s: error %q lacks package prefix", name, err)
		}
	}
}

func TestBuildLogQLRules(t *testing.T) {
	in := labInputs()
	in.Rules = append(in.Rules,
		collect.Rule{Source: "loki", Namespace: "shop-audit.yml", Group: "shop-audit", Name: "ShopAuditDenials", Kind: collect.RuleAlert,
			Expr: `sum(count_over_time({app="shop", component="audit"} |= "result=denied" [5m])) > 20`, Language: "logql"},
		// Same stream written in another order: one signal, two references.
		collect.Rule{Source: "loki", Namespace: "shop-audit.yml", Group: "shop-audit", Name: "shop:audit:rate", Kind: collect.RuleRecord,
			Expr: `sum(rate({component="audit",app="shop"}[5m])) / sum(rate({app="shop"}[5m]))`, Language: "logql"},
		// Unreadable selector: references every stream.
		collect.Rule{Source: "loki", Namespace: "x.yml", Group: "x", Name: "Broken", Kind: collect.RuleAlert,
			Expr: `count_over_time({app=~shop}[5m]) > 0`, Language: "logql"},
	)
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	streams := map[string]graph.Signal{}
	for _, s := range out.Signals {
		if s.Kind == graph.KindStream {
			streams[s.ID] = s
		}
	}
	audit := `loki:stream:{app="shop", component="audit"}`
	for _, id := range []string{audit, `loki:stream:{app="shop"}`, "loki:stream:" + AllStreams} {
		if s, ok := streams[id]; !ok || s.Source != "loki" {
			t.Errorf("stream signal %s missing or wrong: %+v", id, s)
		}
	}
	if len(streams) != 3 {
		t.Errorf("stream signals = %v, want 3", streams)
	}

	cons := consumers(out)
	refs := map[string][]string{} // signal -> consumers
	for _, r := range out.References {
		// Only the rule that didn't parse is a guess.
		if broken := r.ConsumerID == "rule:loki:x.yml/x/Broken"; r.Assumed != broken || r.AllLabels != broken {
			t.Errorf("reference %+v: assumed and all labels should be %v", r, broken)
		}
		if want := cons[r.ConsumerID].Kind.Evidence(); r.Evidence != want {
			t.Errorf("reference %+v: evidence %s, want %s", r, r.Evidence, want)
		}
		if _, ok := cons[r.ConsumerID]; !ok {
			t.Errorf("reference to unknown consumer %s", r.ConsumerID)
		}
		refs[r.SignalID] = append(refs[r.SignalID], r.ConsumerID)
	}
	alert := "rule:loki:shop-audit.yml/shop-audit/ShopAuditDenials"
	if cons[alert].Kind != graph.ConsumerAlertRule {
		t.Errorf("ShopAuditDenials consumer = %+v", cons[alert])
	}
	if got := strings.Join(refs[audit], ","); got != alert+",rule:loki:shop-audit.yml/shop-audit/shop:audit:rate" {
		t.Errorf("audit stream consumers = %s", got)
	}
	if got := refs["loki:stream:"+AllStreams]; len(got) != 1 || got[0] != "rule:loki:x.yml/x/Broken" {
		t.Errorf("all-streams consumers = %v", got)
	}
	// Label detail: the alert matches app and component on the stream.
	for _, r := range out.References {
		if r.SignalID == audit && r.ConsumerID == alert {
			want := []graph.LabelRef{{Name: "app", Use: graph.LabelMatch}, {Name: "component", Use: graph.LabelMatch}}
			if !reflect.DeepEqual(r.Labels, want) {
				t.Errorf("alert labels = %+v, want %+v", r.Labels, want)
			}
		}
	}
}
