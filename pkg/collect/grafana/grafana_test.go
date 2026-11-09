package grafana

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

const fixtures = "../../../testdata/lab/grafana"

// labServer serves the recorded lab responses, plus any extra paths given
// as inline JSON (a body starting with a digit is an HTTP status).
func labServer(t *testing.T, extra map[string]string) *httptest.Server {
	t.Helper()
	files := map[string]string{
		"/api/search":                       "search.json",
		"/api/datasources":                  "datasources.json",
		"/api/dashboards/uid/shop-overview": "dashboard_shop-overview.json",
		"/api/dashboards/uid/legacy-queue":  "dashboard_legacy-queue.json",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := extra[r.URL.Path]; ok {
			if code, err := strconv.Atoi(body); err == nil {
				w.WriteHeader(code)
				fmt.Fprint(w, `{"message":"nope"}`)
				return
			}
			fmt.Fprint(w, body)
			return
		}
		name, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(filepath.Join(fixtures, name))
		if err != nil {
			t.Errorf("fixture %s: %v", name, err)
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
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
	if shop.Title != "Shop Overview" || shop.Folder != "tvt-lab" || len(shop.Panels) != 6 {
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
  {"name":"step","type":"interval","query":"1m,5m"}
 ]},
 "panels":[
  {"id":1,"type":"row","collapsed":true,"panels":[
    {"id":2,"title":"in row, default ds","datasource":null,"targets":[{"expr":"up"}]}]},
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
		2:  {{"prometheus", "prometheus", "up"}},
		3:  {{"loki", "loki", `{app="x"}`}},
		4:  {{"loki", "loki", `{app="y"}`}, {"prometheus", "prometheus", "node_load1"}},
		5:  {},
		6:  {{"mimir", "prometheus", "rate(a[5m])"}},
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
	if titles[8] != "Shared CPU" {
		t.Errorf("library panel title = %q", titles[8])
	}
	if len(d.Variables) != 1 || d.Variables[0].DatasourceUID != "mimir" || d.Variables[0].DatasourceType != "prometheus" {
		t.Errorf("variables = %+v", d.Variables)
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(srv.Close)
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path == "/api/user" {
			fmt.Fprint(w, `{"id":2,"login":"sa-1-tvt"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"invalid API key"}`)
	}))
	t.Cleanup(srv.Close)

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
		return fmt.Sprintf(`logger=context uname=bob t=2026-11-09T10:00:00Z level=info msg="Request Completed" method=%s path=%s status=%s referer=%s handler=%s`,
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
