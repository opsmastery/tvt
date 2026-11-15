package scan

import (
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
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

func TestMatcherMatches(t *testing.T) {
	for _, c := range []struct {
		op    parse.MatchOp
		value string
		in    string
		want  bool
	}{
		{parse.MatchEqual, "debug", "debug", true},
		{parse.MatchEqual, "debug", "info", false},
		{parse.MatchNotEqual, "debug", "info", true},
		{parse.MatchNotEqual, "debug", "debug", false},
		{parse.MatchRegexp, "info|warn", "warn", true},
		{parse.MatchRegexp, "info|warn", "debug", false},
		{parse.MatchRegexp, "deb", "debug", false}, // anchored, like Prometheus
		{parse.MatchRegexp, "", "", true},
		{parse.MatchNotRegex, "info|warn", "debug", true},
		{parse.MatchNotRegex, "info|warn", "info", false},
		{parse.MatchRegexp, "(", "(", false}, // an invalid regexp never matches
		{parse.MatchNotRegex, "(", "x", false},
		{parse.MatchOp("??"), "x", "x", false},
	} {
		if got := matcherMatches(parse.Matcher{Label: "level", Op: c.op, Value: c.value}, c.in); got != c.want {
			t.Errorf("matcherMatches(%v %q, %q) = %v, want %v", c.op, c.value, c.in, got, c.want)
		}
	}
}

// A runbook or postmortem that only names an unread metric makes it, and
// its labels, dormant: a question for the owner, not a drop candidate.
func TestClassifyMentions(t *testing.T) {
	signals := []graph.Signal{
		{ID: "prom:metric:node_load15", Kind: graph.KindMetric, Name: "node_load15", Source: "prom"},
		{ID: "prom:label:node_load15:instance", Kind: graph.KindLabel, Name: "instance", Parent: "node_load15", Source: "prom"},
		{ID: "prom:metric:node_forks_total", Kind: graph.KindMetric, Name: "node_forks_total", Source: "prom"},
		{ID: "prom:metric:plain_total", Kind: graph.KindMetric, Name: "plain_total", Source: "prom"},
	}
	in := Inputs{
		Mentions: []Mention{{Source: "postmortem pm/2026-10.md", Names: []string{"node_forks_total", "node_load15"}}},
		Runbooks: []collect.Runbook{{URL: "https://r/backlog", Mentions: []string{"node_load15"}}},
	}
	if err := classify(signals, nil, nil, nil, mentioned(in), nil, time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"prom:metric:node_load15":         "mentioned in postmortem pm/2026-10.md", // sorted: postmortem before runbook
		"prom:label:node_load15:instance": "mentioned in postmortem pm/2026-10.md",
		"prom:metric:node_forks_total":    "mentioned in postmortem pm/2026-10.md",
		"prom:metric:plain_total":         "",
	}
	for _, s := range signals {
		if s.Dormant != want[s.ID] || (want[s.ID] != "") != (s.Class == "dormant") {
			t.Errorf("%s: %s %q, want %q", s.ID, s.Class, s.Dormant, want[s.ID])
		}
	}
}

// A season the window misses is a gap, and its services' unread signals
// are dormant; a season the window touches changes nothing.
func TestSeasons(t *testing.T) {
	until := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	since := until.AddDate(0, 0, -30) // Feb 18 - Mar 20
	ss := []config.Season{
		{Name: "holiday peak", Months: []int{11, 12}, Services: []string{"shop"}},
		{Name: "quarter close", Months: []int{3, 6, 9, 12}, Services: []string{"finance"}},
	}
	srcs, missed := seasons(ss, since, until)
	if len(srcs) != 2 || srcs[0].Source != "season holiday peak" || srcs[0].Coverage != 0 ||
		srcs[0].Gap != "runs in Nov, Dec, outside the window: readers of shop can't show" {
		t.Errorf("holiday source %+v", srcs[0])
	}
	if srcs[1].Coverage != 1 || srcs[1].Gap != "" {
		t.Errorf("quarter close source %+v", srcs[1])
	}
	if len(missed) != 1 || missed["shop"] != "holiday peak" {
		t.Errorf("missed = %v", missed)
	}

	tier := 3
	owners := []config.Owner{
		{Name: "shop", Tier: &tier, Metrics: []string{"shop_.*"}},
		{Name: "finance", Metrics: []string{"ledger_.*"}},
	}
	signals := []graph.Signal{
		{ID: "prom:metric:shop_gift_cards_total", Kind: graph.KindMetric, Name: "shop_gift_cards_total", Source: "prom"},
		{ID: "prom:metric:ledger_entries_total", Kind: graph.KindMetric, Name: "ledger_entries_total", Source: "prom"},
		{ID: "prom:metric:shop_read_total", Kind: graph.KindMetric, Name: "shop_read_total", Source: "prom"},
	}
	consumers := []graph.Consumer{{ID: "q", Kind: graph.ConsumerAdhocQuery}}
	refs := []graph.Reference{{SignalID: "prom:metric:shop_read_total", ConsumerID: "q", Evidence: graph.EvidenceObserved}}
	if err := classify(signals, consumers, refs, owners, nil, missed, since, until); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"prom:metric:shop_gift_cards_total": {"dormant", "seasonal reader: holiday peak falls outside the window"},
		"prom:metric:ledger_entries_total":  {"candidate", ""}, // its season is in the window
		"prom:metric:shop_read_total":       {"candidate", ""}, // read: a season changes nothing
	}
	for _, s := range signals {
		if w := want[s.ID]; s.Class != w[0] || s.Dormant != w[1] {
			t.Errorf("%s: %s %q, want %v", s.ID, s.Class, s.Dormant, w)
		}
	}

	// A window crossing a year boundary touches both years' months.
	_, missed = seasons(ss[:1], time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 19, 0, 0, 0, 0, time.UTC))
	if len(missed) != 0 {
		t.Errorf("Dec-Jan window missed %v", missed)
	}
}
