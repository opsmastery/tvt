package scan

import (
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// Sprint 2 item 6: classes, owners and tiers on every signal.
func TestClassify(t *testing.T) {
	tier0, tier2 := 0, 2
	in := baseInputs()
	in.Metrics = append(in.Metrics,
		collect.MetricCardinality{Metric: "billing_invoices_total", ActiveSeries: 3},
		collect.MetricCardinality{Metric: "shop_payment_retries_total", ActiveSeries: 3},
		collect.MetricCardinality{Metric: "shop_queue_backlog", ActiveSeries: 1},
		collect.MetricCardinality{Metric: "shop_old_total", ActiveSeries: 1},
	)
	in.External = []config.ExternalConsumer{
		{Name: "finance", Metrics: []string{"billing_.*"}},
		{Name: "siem", Streams: []string{`{component="audit"}`}},
	}
	in.Owners = []config.Owner{
		{Name: "payments", Tier: &tier0, Metrics: []string{"shop_payment_.*"}},
		{Name: "shop", Tier: &tier2, Metrics: []string{"shop_.*"}, Streams: []string{`{app="shop"}`}},
	}
	viewed, stale := start.Add(-time.Hour), start.Add(-40*24*time.Hour)
	in.Dashboards = []collect.Dashboard{
		{UID: "v", Title: "Viewed", Panels: []collect.Panel{{ID: 1, Title: "cart",
			Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: "sum by (op) (rate(shop_cart_operations_total[5m]))"}}}}},
		{UID: "s", Title: "Stale", Panels: []collect.Panel{{ID: 1, Title: "old",
			Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: "shop_old_total"}}}}},
		{UID: "l", Title: "Logs", Panels: []collect.Panel{{ID: 1, Title: "logs",
			Queries: []collect.PanelQuery{{DatasourceType: "loki", Expr: `{app="shop", component="api"}`}}}}},
	}
	in.Views = []collect.DashboardView{
		{DashboardUID: "v", OrgID: 1, At: viewed, Browser: true},
		{DashboardUID: "s", OrgID: 1, At: stale, Browser: true}, // before the window
	}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	sigs := signalIDs(out)
	for id, want := range map[string]struct{ class, dormant, owner string }{
		bucket: {"hard_keep", "", "shop"}, // through the recording rule
		"prom:metric:route:shop_http_request_duration_seconds:p99_5m": {"hard_keep", "", ""},
		"prom:metric:billing_invoices_total":                          {"hard_keep", "", ""},     // external consumer
		"prom:metric:shop_cart_operations_total":                      {"keep_high", "", "shop"}, // viewed in the window
		"prom:label:shop_cart_operations_total:op":                    {"keep_high", "", "shop"}, // read by the viewed panel
		"prom:label:shop_cart_operations_total:user_id":               {"candidate", "", "shop"}, // on it, never read
		"prom:metric:shop_old_total":                                  {"candidate", "", "shop"}, // viewed only before the window
		"prom:metric:shop_payment_retries_total":                      {"dormant", "tier 0 service", "payments"},
		"prom:metric:shop_queue_backlog":                              {"dormant", "queue-depth signal", "shop"},
		"prom:metric:node_load1":                                      {"candidate", "", ""},
		`loki:stream:{component="audit"}`:                             {"hard_keep", "", ""},     // siem
		`loki:stream:{app="shop", component="api"}`:                   {"candidate", "", "shop"}, // dashboard never viewed
	} {
		s, ok := sigs[id]
		if !ok {
			t.Errorf("no signal %s", id)
			continue
		}
		if s.Class != want.class || s.Dormant != want.dormant || s.Owner != want.owner {
			t.Errorf("%s: class %s %q owner %q, want %s %q %q", id, s.Class, s.Dormant, s.Owner, want.class, want.dormant, want.owner)
		}
	}
	if s := sigs["prom:metric:shop_payment_retries_total"]; s.Tier == nil || *s.Tier != 0 {
		t.Errorf("tier %v", s.Tier)
	}
	// An external consumer reads every label of what it selects.
	for _, r := range out.References {
		if r.ConsumerID == "ext:finance" && (!r.AllLabels || r.Evidence != graph.EvidenceDeclared) {
			t.Errorf("external reference %+v", r)
		}
	}
	for _, s := range out.Signals {
		if s.Class == "" {
			t.Errorf("%s has no class", s.ID)
		}
	}
}

func TestStreamOwnerRequiresImpliedSelector(t *testing.T) {
	tests := []struct {
		name   string
		signal string
		owners []config.Owner
		want   string
	}{
		{
			name:   "non-equality matcher does not imply a different constraint",
			signal: `{app="shop", env=~"prod"}`,
			owners: []config.Owner{
				{Name: "mismatch", Streams: []string{`{app="shop", env!="prod"}`}},
				{Name: "shop", Streams: []string{`{app="shop"}`}},
			},
			want: "shop",
		},
		{
			name:   "omitted signal label is unknown",
			signal: `{app="shop"}`,
			owners: []config.Owner{
				{Name: "empty-env", Streams: []string{`{app="shop", env=""}`}},
			},
		},
		{
			name:   "equality can imply another matcher",
			signal: `{app="shop", env="dev"}`,
			owners: []config.Owner{
				{Name: "shop", Streams: []string{`{app="shop", env!="prod"}`}},
			},
			want: "shop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := newOwnerMatcher(tt.owners)
			if err != nil {
				t.Fatal(err)
			}
			got := m.match(graph.Signal{Kind: graph.KindStream, Name: tt.signal})
			if got == nil {
				if tt.want != "" {
					t.Fatalf("no owner, want %q", tt.want)
				}
				return
			}
			if got.Name != tt.want {
				t.Fatalf("owner %q, want %q", got.Name, tt.want)
			}
		})
	}
}
