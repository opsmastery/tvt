package grafana

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// The lab's one Grafana-managed alert, from the recorded ruler config.
func TestRulesFixture(t *testing.T) {
	routes := testutil.Routes{
		"/api/ruler/grafana/api/v1/rules": testutil.File(t, "lab/grafana/ruler_config.json"),
		"/api/datasources":                testutil.File(t, "lab/grafana/datasources.json"),
	}
	srv := testutil.NewServer(t, routes)
	got, err := New(srv.URL, "").Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []collect.Rule{{
		Source: "grafana", Namespace: "cg3qdhms8urr4c", Group: "shop-grafana-alerts", Name: "Shop order export lag",
		UID: "shop-order-export-lag", Kind: collect.RuleAlert, EvaluatedBy: collect.EvaluatedByGrafana, Interval: time.Minute, For: 5 * time.Minute,
		Expr:        "max(shop_order_export_lag_seconds)",
		Queries:     []collect.PanelQuery{{DatasourceUID: "prometheus", DatasourceType: "prometheus", Expr: "max(shop_order_export_lag_seconds)"}},
		Annotations: map[string]string{"summary": "Orders haven't been exported to the warehouse for over 5 minutes"},
		Labels:      map[string]string{"severity": "warning"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rules\n got  %+v\n want %+v", got, want)
	}
}

// Queries keep their datasource (by UID or name); expressions, and queries
// with no text to a datasource tvt doesn't parse, are left out; a recording rule is named by its metric; the
// group interval stands in when the rule has none.
func TestRulesQueriesAndRecording(t *testing.T) {
	rules := `{"Ops": [{"name": "g", "interval": "30s", "rules": [
	  {"grafana_alert": {"uid": "b", "title": "Two sources", "namespace_uid": "f1", "rule_group": "g", "intervalSeconds": 0, "data": [
	    {"refId": "A", "datasourceUid": "loki", "model": {"expr": "count_over_time({app=\"shop\"} | json [5m])"}},
	    {"refId": "B", "datasourceUid": "Mimir", "model": {"expr": "up"}},
	    {"refId": "C", "datasourceUid": "__expr__", "model": {"type": "math", "expression": "$A + $B"}},
	    {"refId": "D", "datasourceUid": "es", "model": {"query": ""}}]}},
	  {"grafana_alert": {"uid": "a", "title": "Recorder", "namespace_uid": "f1", "rule_group": "g", "intervalSeconds": 60,
	    "record": {"metric": "job:up:sum", "from": "A"},
	    "data": [{"refId": "A", "datasourceUid": "prometheus", "model": {"expr": "sum by (job) (up)"}}]}}]}]}`
	srv := testutil.NewServer(t, testutil.Routes{
		"/api/ruler/grafana/api/v1/rules": body(rules),
		"/api/datasources":                testutil.File(t, "lab/grafana/datasources.json"),
	})
	got, err := New(srv.URL, "").Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rules %+v", got)
	}
	rec, two := got[0], got[1]
	if rec.Kind != collect.RuleRecord || rec.Name != "job:up:sum" || rec.Interval != time.Minute || rec.Expr != "sum by (job) (up)" {
		t.Errorf("recording rule %+v", rec)
	}
	wantQ := []collect.PanelQuery{
		{DatasourceUID: "loki", DatasourceType: "loki", Expr: `count_over_time({app="shop"} | json [5m])`},
		{DatasourceUID: "mimir", DatasourceType: "prometheus", Expr: "up"},
	}
	if two.Kind != collect.RuleAlert || two.Interval != 30*time.Second || !reflect.DeepEqual(two.Queries, wantQ) ||
		two.Expr != "A: count_over_time({app=\"shop\"} | json [5m])\nB: up" {
		t.Errorf("two-source rule %+v", two)
	}
}

// A rule tvt can't read is an error, never zero rules.
func TestRulesErrors(t *testing.T) {
	alert := `{"F": [{"name": "g", "interval": "1m", "rules": [{"grafana_alert": {"uid": "u", "title": "T", "data": [
	  {"refId": "A", "datasourceUid": "%s", "model": {"expr": "up"}}]}}]}]}`
	for _, tc := range []struct {
		name   string
		routes testutil.Routes
		want   string
	}{
		{"forbidden", testutil.Routes{"/api/ruler/grafana/api/v1/rules": status(403)}, "alert.rules:read"},
		{"alerting off", testutil.Routes{"/api/ruler/grafana/api/v1/rules": status(404)}, "HTTP 404"},
		{"null listing", testutil.Routes{"/api/ruler/grafana/api/v1/rules": body("null")}, "not a rule listing"},
		// A Prometheus or Loki query tvt can't read might be the only thing
		// protecting a signal.
		{"empty Prometheus query", testutil.Routes{
			"/api/ruler/grafana/api/v1/rules": body(`{"F": [{"name": "g", "rules": [{"grafana_alert": {"uid": "u", "title": "T", "data": [
			  {"refId": "A", "datasourceUid": "prometheus", "model": {"expr": "  "}}]}}]}]}`),
			"/api/datasources": testutil.File(t, "lab/grafana/datasources.json"),
		}, `query "A" for datasource "prometheus" has an empty or unreadable model`},
		// VictoriaMetrics queries are read as PromQL, so they fail closed
		// the same way (Sprint 2 item 17).
		{"empty VictoriaMetrics query", testutil.Routes{
			"/api/ruler/grafana/api/v1/rules": body(`{"F": [{"name": "g", "rules": [{"grafana_alert": {"uid": "u", "title": "T", "data": [
			  {"refId": "A", "datasourceUid": "vm", "model": {"expr": ""}}]}}]}]}`),
			"/api/datasources": body(`[{"uid": "vm", "name": "VM", "type": "victoriametrics-metrics-datasource"}]`),
		}, `query "A" for datasource "vm" has an empty or unreadable model`},
		{"datasource by name without the list", testutil.Routes{
			"/api/ruler/grafana/api/v1/rules": body(strings.Replace(alert, "%s", "Prometheus", 1)),
			"/api/datasources":                status(403),
		}, "datasources:read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(testutil.NewServer(t, tc.routes).URL, "").Rules(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// No rules: no datasource list needed, and an empty result.
func TestRulesNone(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Routes{"/api/ruler/grafana/api/v1/rules": body(`{}`)})
	got, err := New(srv.URL, "").Rules(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("Rules = %v, %v", got, err)
	}
}

func body(s string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, s) })
}

func status(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		fmt.Fprint(w, `{"message":"nope"}`)
	})
}

// Sprint 2 item 19: a Grafana-managed rule query on a datasource tvt
// doesn't parse is kept, Pyroscope's model read as its own query syntax
// and an empty one with no text, so scan records an assumed reader.
func TestRulesUnparsedDatasources(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Routes{
		"/api/ruler/grafana/api/v1/rules": body(`{"F": [{"name": "g", "rules": [{"grafana_alert": {"uid": "u", "title": "T", "data": [
		  {"refId": "A", "datasourceUid": "pyro", "model": {"profileTypeId": "process_cpu:cpu:nanoseconds:cpu:nanoseconds", "labelSelector": "{service_name=\"shop\"}"}},
		  {"refId": "B", "datasourceUid": "es", "model": {"metrics": [{"type": "count"}]}},
		  {"refId": "C", "datasourceUid": "__expr__", "model": {"type": "threshold"}}]}}]}]}`),
		"/api/datasources": body(`[{"uid": "pyro", "name": "Pyroscope", "type": "grafana-pyroscope-datasource"},
		  {"uid": "es", "name": "ES", "type": "elasticsearch"}]`),
	})
	got, err := New(srv.URL, "").Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []collect.PanelQuery{
		{DatasourceUID: "pyro", DatasourceType: "grafana-pyroscope-datasource", Expr: `process_cpu:cpu:nanoseconds:cpu:nanoseconds{service_name="shop"}`},
		{DatasourceUID: "es", DatasourceType: "elasticsearch"},
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0].Queries, want) {
		t.Fatalf("rules %+v, want one rule with queries %+v", got, want)
	}
}
