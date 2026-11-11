package grafana

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

var fixtures = filepath.Join(testutil.Lab, "grafana")

// labServer serves the recorded lab responses, plus any extra paths given
// as inline JSON (a body starting with a digit is an HTTP status).
func labServer(t *testing.T, extra map[string]string) *httptest.Server {
	t.Helper()
	routes := testutil.Routes{}
	for path, name := range map[string]string{
		"/api/search":                       "search.json",
		"/api/datasources":                  "datasources.json",
		"/api/dashboards/uid/shop-overview": "dashboard_shop-overview.json",
		"/api/dashboards/uid/legacy-queue":  "dashboard_legacy-queue.json",
	} {
		routes[path] = testutil.File(t, "lab/grafana/"+name)
	}
	for path, body := range extra {
		routes[path] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if code, err := strconv.Atoi(body); err == nil {
				w.WriteHeader(code)
				fmt.Fprint(w, `{"message":"nope"}`)
				return
			}
			fmt.Fprint(w, body)
		})
	}
	return testutil.NewServer(t, routes)
}

func byUID(ds []collect.Dashboard) map[string]collect.Dashboard {
	m := map[string]collect.Dashboard{}
	for _, d := range ds {
		m[d.UID] = d
	}
	return m
}

func TestDashboardsFixture(t *testing.T) {
	got, err := New(labServer(t, nil).URL, "").Dashboards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := byUID(got)
	if len(m) != 2 {
		t.Fatalf("got %d dashboards, want 2", len(got))
	}

	shop := m["shop-overview"]
	if shop.Title != "Shop Overview" || shop.Folder != "tvt-lab" || len(shop.Panels) != 8 {
		t.Fatalf("shop-overview = %+v", shop)
	}
	p1 := shop.Panels[0].Queries[0]
	if p1.DatasourceUID != "prometheus" || p1.DatasourceType != "prometheus" ||
		!strings.Contains(p1.Expr, `shop_http_requests_total{route=~"$route"}`) {
		t.Errorf("panel 1 query = %+v", p1)
	}
	if q := shop.Panels[5].Queries[0]; q.DatasourceType != "loki" || !strings.HasPrefix(q.Expr, `{app="shop"`) {
		t.Errorf("panel 6 query = %+v", q)
	}
	if len(shop.Variables) != 1 || shop.Variables[0].Name != "route" ||
		shop.Variables[0].Query != "label_values(shop_http_requests_total, route)" ||
		shop.Variables[0].DatasourceType != "prometheus" {
		t.Errorf("variables = %+v", shop.Variables)
	}
	if !shop.LastViewed.IsZero() {
		t.Error("LastViewed set from an OSS API")
	}

	legacy := m["legacy-queue"]
	if len(legacy.Panels) != 2 || legacy.Panels[0].Queries[0].Expr != "shop_legacy_queue_depth" {
		t.Errorf("legacy-queue = %+v", legacy)
	}
}

// edgeDashboard exercises datasource resolution. Datasources in the lab
// fixture: prometheus (default), mimir (prometheus type), loki.
const edgeDashboard = `{"dashboard":{"uid":"edge","title":"Edge cases",
 "templating":{"list":[
  {"name":"ds","type":"datasource","query":"prometheus","current":{"value":"mimir"}},
  {"name":"anyprom","type":"datasource","query":"prometheus","current":{"value":"${DS_PROM}"}},
  {"name":"job","type":"query","datasource":"$ds","query":"label_values(up, job)"},
  {"name":"step","type":"interval","query":"1m,5m"},
  {"name":"env","type":"custom","query":"Production : prod, staging,a\\,b","includeAll":true,"allValue":".*"},
  {"name":"region","type":"custom","query":"ignored","multi":true,"options":[{"value":"$__all"},{"value":"eu"},{"value":"us"}]},
  {"name":"tenant","type":"constant","query":"acme"},
  {"name":"free","type":"textbox","query":"x"},
  {"name":"level","type":"query","datasource":{"type":"loki","uid":"loki"},"query":{"type":1,"label":"level","stream":"{app=\"shop\"}","refId":"LokiVariableQueryEditor-VariableQuery"}},
  {"name":"lnames","type":"query","datasource":{"type":"loki","uid":"loki"},"query":{"type":0,"refId":"LokiVariableQueryEditor-VariableQuery"}},
  {"name":"inst","type":"query","datasource":{"type":"prometheus","uid":"prometheus"},"query":{"qryType":1,"metric":"up","label":"instance","refId":"PrometheusVariableQueryEditor-VariableQuery"}}
 ]},
 "annotations":{"list":[
  {"name":"Annotations & Alerts","builtIn":1,"datasource":{"type":"grafana","uid":"-- Grafana --"},"enable":true},
  {"name":"Deploys","datasource":{"type":"prometheus","uid":"prometheus"},"enable":false,"expr":"changes(deploy_info[1m]) > 0","titleFormat":"Deploy {{ version }}","textFormat":"{{job}}","tagKeys":"job,instance"},
  {"name":"Errors","datasource":{"type":"loki","uid":"loki"},"target":{"expr":"{app=\"shop\"} |= \"panic\""}},
  {"name":"Empty","datasource":{"type":"prometheus","uid":"prometheus"}}
 ]},
 "panels":[
  {"id":1,"type":"row","collapsed":true,"panels":[
    {"id":2,"title":"in row, default ds","datasource":null,"targets":[{"expr":"up","legendFormat":"{{instance}}"}]}]},
  {"id":3,"title":"legacy name","datasource":"Loki","targets":[{"expr":"{app=\"x\"}"}]},
  {"id":4,"title":"mixed","datasource":{"uid":"-- Mixed --"},"targets":[
    {"datasource":{"type":"loki","uid":"loki"},"expr":"{app=\"y\"}"},
    {"expr":"node_load1"},
    {"datasource":{"type":"__expr__","uid":"__expr__"},"expression":"$A + $B"}]},
  {"id":5,"title":"builtin","datasource":{"type":"datasource","uid":"-- Grafana --"},"targets":[{"queryType":"randomWalk"}]},
  {"id":6,"title":"ds variable","datasource":{"type":"prometheus","uid":"${ds}"},"targets":[{"expr":"rate(a[5m])"}]},
  {"id":7,"title":"unpinned variable","datasource":"$anyprom","targets":[{"expr":"b"}]},
  {"id":8,"title":"","libraryPanel":{"uid":"lib1"}},
  {"id":9,"title":"deleted library panel","libraryPanel":{"uid":"gone"}},
  {"id":10,"title":"target overrides panel","datasource":{"uid":"prometheus"},"targets":[
    {"datasource":{"uid":"mimir"},"expr":"c"},{"expr":""}]},
  {"id":11,"title":"query object","datasource":{"type":"loki","uid":"loki"},"targets":[{"query":{"query":"{app=\"z\"}"}}]}
 ]}}`

func TestDashboardEdgeCases(t *testing.T) {
	srv := labServer(t, map[string]string{
		"/api/search":                `[{"uid":"edge","title":"Edge cases","folderTitle":""}]`,
		"/api/dashboards/uid/edge":   edgeDashboard,
		"/api/library-elements/lib1": `{"result":{"model":{"title":"Shared CPU","datasource":{"uid":"prometheus"},"targets":[{"expr":"node_cpu_seconds_total"}]}}}`,
		"/api/library-elements/gone": "404",
	})
	got, err := New(srv.URL, "").Dashboards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := got[0]

	type q struct{ uid, typ, expr string }
	panels := map[int][]q{}
	titles := map[int]string{}
	for _, p := range d.Panels {
		titles[p.ID] = p.Title
		panels[p.ID] = []q{}
		for _, x := range p.Queries {
			panels[p.ID] = append(panels[p.ID], q{x.DatasourceUID, x.DatasourceType, x.Expr})
		}
	}
	want := map[int][]q{
		2: {{"prometheus", "prometheus", "up"}},
		3: {{"loki", "loki", `{app="x"}`}},
		4: {{"loki", "loki", `{app="y"}`}, {"prometheus", "prometheus", "node_load1"}},
		5: {},
		// Datasource variables stay symbolic: viewers can switch away from
		// the saved selection ("mimir"), so they stand for every
		// datasource of the variable's type.
		6:  {{"$ds", "prometheus", "rate(a[5m])"}},
		7:  {{"$anyprom", "prometheus", "b"}},
		8:  {{"prometheus", "prometheus", "node_cpu_seconds_total"}},
		10: {{"mimir", "prometheus", "c"}},
		11: {{"loki", "loki", `{app="z"}`}},
	}
	for id, w := range want {
		if fmt.Sprint(panels[id]) != fmt.Sprint(w) {
			t.Errorf("panel %d (%s): got %v, want %v", id, titles[id], panels[id], w)
		}
	}
	if _, ok := panels[9]; ok {
		t.Error("deleted library panel kept")
	}
	if _, ok := panels[1]; ok {
		t.Error("row kept as a panel")
	}
	if l := d.Panels[0].Queries[0].Legend; d.Panels[0].ID != 2 || l != "{{instance}}" {
		t.Errorf("panel 2 legend = %q", l)
	}
	if titles[8] != "Shared CPU" {
		t.Errorf("library panel title = %q", titles[8])
	}
	// Query variables keep their datasource; custom and constant ones
	// their values; datasource and interval variables are left out.
	wantVars := []collect.Variable{
		{Name: "job", Type: "query", Query: "label_values(up, job)", DatasourceUID: "$ds", DatasourceType: "prometheus"},
		{Name: "env", Type: "custom", Query: `Production : prod, staging,a\,b`, Values: []string{"prod", "staging", "a,b"}, IncludeAll: true, AllValue: ".*"},
		{Name: "region", Type: "custom", Query: "ignored", Values: []string{"eu", "us"}, Multi: true},
		{Name: "tenant", Type: "constant", Query: "acme", Values: []string{"acme"}},
		{Name: "free", Type: "textbox", Query: "x"},
		{Name: "level", Type: "query", Query: `label_values({app="shop"}, level)`, DatasourceUID: "loki", DatasourceType: "loki"},
		{Name: "lnames", Type: "query", DatasourceUID: "loki", DatasourceType: "loki"},
		{Name: "inst", Type: "query", Query: "label_values(up, instance)", DatasourceUID: "prometheus", DatasourceType: "prometheus"},
	}
	if !reflect.DeepEqual(d.Variables, wantVars) {
		t.Errorf("variables\n got  %+v\n want %+v", d.Variables, wantVars)
	}
	// Annotations: the built-in and the empty one are left out; a disabled
	// one is kept (viewers can turn it on).
	wantAnn := []collect.Annotation{
		{Name: "Deploys", Expr: "changes(deploy_info[1m]) > 0", DatasourceUID: "prometheus", DatasourceType: "prometheus",
			TitleFormat: "Deploy {{ version }}", TextFormat: "{{job}}", TagKeys: "job,instance"},
		{Name: "Errors", Expr: `{app="shop"} |= "panic"`, DatasourceUID: "loki", DatasourceType: "loki", Enabled: true},
	}
	if !reflect.DeepEqual(d.Annotations, wantAnn) {
		t.Errorf("annotations\n got  %+v\n want %+v", d.Annotations, wantAnn)
	}
}

func TestDashboardsWithoutDatasourceAccess(t *testing.T) {
	// A Viewer token may get 403 on /api/datasources; explicit UIDs still resolve.
	srv := labServer(t, map[string]string{"/api/datasources": "403"})
	got, err := New(srv.URL, "").Dashboards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q := byUID(got)["shop-overview"].Panels[0].Queries[0]; q.DatasourceUID != "prometheus" || q.DatasourceType != "prometheus" {
		t.Errorf("query = %+v", q)
	}
}

func TestDashboardsNeedingDatasourceListFail(t *testing.T) {
	cases := map[string]string{
		"panel on the default": `{"dashboard":{"uid":"x","title":"X","panels":[
			{"id":1,"title":"p","datasource":null,"targets":[{"expr":"up"}]}]}}`,
		"legacy name": `{"dashboard":{"uid":"x","title":"X","panels":[
			{"id":1,"title":"p","datasource":"Prometheus","targets":[{"expr":"up"}]}]}}`,
		"mixed target without datasource": `{"dashboard":{"uid":"x","title":"X","panels":[
			{"id":1,"title":"p","datasource":{"uid":"-- Mixed --"},"targets":[{"expr":"up"}]}]}}`,
		"variable on the default": `{"dashboard":{"uid":"x","title":"X","panels":[],
			"templating":{"list":[{"name":"job","type":"query","query":"label_values(up, job)"}]}}}`,
	}
	for name, dash := range cases {
		srv := labServer(t, map[string]string{
			"/api/datasources":      "403",
			"/api/search":           `[{"uid":"x","title":"X"}]`,
			"/api/dashboards/uid/x": dash,
		})
		_, err := New(srv.URL, "").Dashboards(context.Background())
		if err == nil || !strings.Contains(err.Error(), "datasources:read") {
			t.Errorf("%s: err = %v, want an actionable datasources:read error", name, err)
		}
	}

	// Explicit UIDs and types, and a panel default no target uses, still
	// work without the list.
	srv := labServer(t, map[string]string{
		"/api/datasources": "403",
		"/api/search":      `[{"uid":"x","title":"X"}]`,
		"/api/dashboards/uid/x": `{"dashboard":{"uid":"x","title":"X","panels":[
			{"id":1,"title":"p","datasource":null,"targets":[{"datasource":{"type":"loki","uid":"loki"},"expr":"{a=\"b\"}"}]}]}}`,
	})
	got, err := New(srv.URL, "").Dashboards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q := got[0].Panels[0].Queries; len(q) != 1 || q[0].DatasourceUID != "loki" {
		t.Errorf("queries = %+v", q)
	}
}

func TestUnreadableDashboardFails(t *testing.T) {
	srv := labServer(t, map[string]string{"/api/dashboards/uid/legacy-queue": "403"})
	_, err := New(srv.URL, "").Dashboards(context.Background())
	if err == nil || !strings.Contains(err.Error(), "legacy-queue") {
		t.Fatalf("err = %v, want failure naming the dashboard", err)
	}
}

func TestSearchPaginates(t *testing.T) {
	old := searchPageSize
	searchPageSize = 1
	t.Cleanup(func() { searchPageSize = old })

	var pages []string
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		if r.URL.Query().Get("type") != "dash-db" {
			t.Errorf("type = %q", r.URL.Query().Get("type"))
		}
		switch r.URL.Query().Get("page") {
		case "1":
			fmt.Fprint(w, `[{"uid":"a"}]`)
		case "2":
			fmt.Fprint(w, `[{"uid":"b"}]`)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	hits, err := New(srv.URL, "").search(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || strings.Join(pages, ",") != "1,2,3" {
		t.Errorf("hits = %v, pages = %v", hits, pages)
	}
}

func TestLoginAndToken(t *testing.T) {
	const token = "glsa_s3cret"
	var auth string
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path == "/api/user" {
			fmt.Fprint(w, `{"id":2,"login":"sa-1-tvt"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"invalid API key"}`)
	}))

	c := New(srv.URL, token)
	login, err := c.Login(context.Background())
	if err != nil || login != "sa-1-tvt" {
		t.Fatalf("Login = %q, %v", login, err)
	}
	if auth != "Bearer "+token {
		t.Errorf("Authorization = %q", auth)
	}
	_, err = c.Dashboards(context.Background())
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("err = %v", err)
	}
}

func TestDashboardViewsFixture(t *testing.T) {
	rl := &RouterLog{Paths: []string{filepath.Join(fixtures, "router.log")}}
	got, err := rl.DashboardViews(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		uid     string
		browser bool
	}
	count := map[key]int{}
	for i, v := range got {
		count[key{v.DashboardUID, v.Browser}]++
		if v.User != "admin" || v.At.IsZero() {
			t.Errorf("bad view %+v", v)
		}
		if i > 0 && v.At.Before(got[i-1].At) {
			t.Fatal("not sorted")
		}
	}
	// The recorded log has the viewer's browser-style loads of Shop Overview,
	// one manual browser view of each dashboard (spike 1), and scripted API
	// fetches with no Referer (capture-fixtures.sh), like tvt's own scan.
	want := map[key]int{
		{"shop-overview", true}:  11,
		{"shop-overview", false}: 4,
		{"legacy-queue", true}:   1,
		{"legacy-queue", false}:  4,
	}
	if fmt.Sprint(count) != fmt.Sprint(want) {
		t.Errorf("views = %v, want %v", count, want)
	}

	rl.ExcludeUsers = []string{"admin"}
	if got, _ := rl.DashboardViews(context.Background(), time.Time{}); len(got) != 0 {
		t.Errorf("excluded user still has %d views", len(got))
	}
}

func writeLog(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "grafana.log")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDashboardViewsFilters(t *testing.T) {
	line := func(method, path, status, handler, referer string) string {
		return fmt.Sprintf(`logger=context userId=2 orgId=1 uname=bob t=2026-11-09T10:00:00Z level=info msg="Request Completed" method=%s path=%s status=%s referer=%s handler=%s`,
			method, path, status, referer, handler)
	}
	p := writeLog(t, strings.Join([]string{
		line("GET", "/api/dashboards/uid/a", "200", dashboardHandler, "https://g/d/a/slug?orgId=1"),
		line("GET", "/api/dashboards/uid/a", "404", dashboardHandler, ""),
		line("DELETE", "/api/dashboards/uid/a", "200", dashboardHandler, ""),
		line("GET", "/api/dashboards/uid/a/public-dashboards", "200", "/api/dashboards/uid/:dashboardUid/public-dashboards", ""),
		line("GET", "/api/dashboards/uid/b", "200", dashboardHandler, "https://g/d/a/other"),
		`not logfmt at all`,
	}, "\n"))
	got, err := (&RouterLog{Paths: []string{p}}).DashboardViews(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].DashboardUID != "a" || !got[0].Browser || got[1].DashboardUID != "b" || got[1].Browser {
		t.Errorf("got %+v", got)
	}
}

func TestRouterLogErrorsAndCoverage(t *testing.T) {
	if _, err := (&RouterLog{Paths: []string{writeLog(t, "GET / 200\n")}}).DashboardViews(context.Background(), time.Time{}); err == nil {
		t.Error("non-Grafana file accepted")
	}
	if _, err := (&RouterLog{}).DashboardViews(context.Background(), time.Time{}); err == nil {
		t.Error("no paths accepted")
	}
	// logfmt with a t= timestamp but not Grafana's (no logger=).
	if _, err := (&RouterLog{Paths: []string{writeLog(t, `t=2026-11-09T10:00:00Z level=info msg="hello"`+"\n")}}).DashboardViews(context.Background(), time.Time{}); err == nil {
		t.Error("non-Grafana logfmt accepted")
	}

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	p := writeLog(t, `logger=context t=2026-11-03T12:00:00Z msg="Request Completed"`+"\n")
	rl := &RouterLog{Paths: []string{p}, Now: func() time.Time { return now }}
	cov, err := rl.Coverage(context.Background(), now.Add(-30*24*time.Hour))
	if err != nil || math.Abs(cov-0.2) > 0.001 {
		t.Errorf("coverage = %v, %v; want 0.2 (6 of 30 days)", cov, err)
	}
	cov, err = rl.Coverage(context.Background(), now.Add(-24*time.Hour))
	if err != nil || cov != 1 {
		t.Errorf("coverage = %v, %v; want 1 (log predates window)", cov, err)
	}
}

func TestRefererIsDashboard(t *testing.T) {
	cases := map[string]bool{
		"https://g/d/a/slug?orgId=1":           true,
		"https://g/d/a":                        true,
		"https://example.com/grafana/d/a/slug": true, // sub-path
		"https://g/d-solo/a/slug?panelId=2":    true, // embedded panel
		"https://g/d/ab/other-dashboard":       false,
		"https://g/d/b/other?return=/d/a/slug": false, // only in the query
		"https://g/dashboards/f/a/":            false,
		"":                                     false,
		"http://g/d/a%2Fx/slug":                false,
	}
	for ref, want := range cases {
		if got := refererIsDashboard(ref, "a"); got != want {
			t.Errorf("refererIsDashboard(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestDashboardViewsByOrg(t *testing.T) {
	line := func(org, uid string) string {
		return fmt.Sprintf(`logger=context userId=1 orgId=%s uname=ann t=2026-11-09T10:00:00Z msg="Request Completed" method=GET path=/api/dashboards/uid/%s status=200 referer=https://g/d/%s/s handler=/api/dashboards/uid/:uid`,
			org, uid, uid)
	}
	p := writeLog(t, line("1", "x")+"\n"+line("2", "x")+"\n"+line("2", "y")+"\n")

	all, err := (&RouterLog{Paths: []string{p}}).DashboardViews(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].OrgID != 1 || all[1].OrgID != 2 {
		t.Errorf("all orgs = %+v", all)
	}
	org1, err := (&RouterLog{Paths: []string{p}, OrgID: 1}).DashboardViews(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(org1) != 1 || org1[0].DashboardUID != "x" || org1[0].OrgID != 1 {
		t.Errorf("org 1 = %+v", org1)
	}
}

func TestOrgID(t *testing.T) {
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/org" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id":3,"name":"Shop"}`)
	}))
	id, err := New(srv.URL, "").OrgID(context.Background())
	if err != nil || id != 3 {
		t.Errorf("OrgID = %d, %v", id, err)
	}
}

// Lines stamped after now (clock skew, or a log newer than the scan's start)
// must not make coverage negative or above 1.
func TestRouterLogCoverageIgnoresFutureLines(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	line := func(at time.Time) string {
		return `logger=context userId=1 orgId=1 uname=admin t=` + at.Format(time.RFC3339Nano) +
			` level=info msg="Request Completed" method=GET path=/api/health status=200 referer= handler=/api/health` + "\n"
	}
	path := filepath.Join(t.TempDir(), "grafana.log")
	since := now.Add(-10 * time.Hour)
	for name, lines := range map[string][]time.Time{
		"only future lines":    {now.Add(time.Hour), now.Add(2 * time.Hour)},
		"half the window":      {now.Add(-5 * time.Hour), now.Add(time.Hour)},
		"future first in file": {now.Add(3 * time.Hour), now.Add(-5 * time.Hour)},
	} {
		var b strings.Builder
		for _, at := range lines {
			b.WriteString(line(at))
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		rl := &RouterLog{Paths: []string{path}, Now: func() time.Time { return now }}
		got, err := rl.Coverage(context.Background(), since)
		want := map[string]float64{"only future lines": 0, "half the window": 0.5, "future first in file": 0.5}[name]
		if err != nil || got != want {
			t.Errorf("%s: coverage = %v, %v; want %v", name, got, err, want)
		}
	}
}
