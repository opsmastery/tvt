package testutil

import (
	"github.com/opsmastery/tvt/pkg/collect"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Testdata is the absolute path of the repository's testdata directory.
var Testdata = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata")
}()

// Lab is testdata/lab: recorded responses from tvt-lab.
var Lab = filepath.Join(Testdata, "lab")

// Forbidden reports whether a request would run a query on a backend that
// logs queries as usage (invariant 4: tvt never runs PromQL or LogQL
// there). It covers Prometheus and Mimir (any prefix), Loki, and Grafana's
// query and datasource-proxy APIs, which forward to those backends. The
// one exception is the query-log probe: an instant query whose text is a
// marker selecting nothing, tagged collect.ProbeTag, to Prometheus or Mimir
// (collect.ProbeQuery) or to Loki (collect.LokiProbeQuery, S5 item 5).
func Forbidden(r *http.Request) bool {
	path := r.URL.Path
	if strings.HasSuffix(path, "/api/v1/query") && !strings.Contains(path, "/loki/") && !strings.Contains(path, "/api/datasources/proxy/") {
		if _, ok := collect.ProbeNonce(r.URL.Query().Get("query")); ok && r.Header.Get("X-Query-Tags") == collect.ProbeTag {
			return false
		}
	}
	if strings.HasSuffix(path, "/loki/api/v1/query") && !strings.Contains(path, "/api/datasources/proxy/") {
		if _, ok := collect.LokiProbeNonce(r.URL.Query().Get("query")); ok && r.Header.Get("X-Query-Tags") == collect.ProbeTag {
			return false
		}
	}
	// Per-level bytes of one stream (S10 item 3): the one LogQL metric
	// query tvt sends, in exactly its shape and tagged as tvt's.
	if strings.HasSuffix(path, "/loki/api/v1/query") && !strings.Contains(path, "/api/datasources/proxy/") &&
		collect.IsLokiLevelQuery(r.URL.Query().Get("query")) && r.Header.Get("X-Query-Tags") == collect.LevelTag {
		return false
	}
	// An attribute sample of one stream (S10 item 7): a plain selector, at
	// most collect.AttrLineLimit lines, tagged as tvt's.
	if strings.HasSuffix(path, "/loki/api/v1/query_range") && !strings.Contains(path, "/api/datasources/proxy/") &&
		collect.IsLokiAttrQuery(r.URL.Query().Get("query")) && r.Header.Get("X-Query-Tags") == collect.AttrTag {
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= collect.AttrLineLimit {
			return false
		}
	}
	// Usage Insights (Grafana Cloud's view events) is read through the
	// data source proxy, tagged as tvt's own; nothing else may be.
	if strings.HasPrefix(path, "/api/datasources/proxy/uid/"+collect.UsageInsightsUID+"/loki/api/v1/query_range") &&
		r.Header.Get("X-Query-Tags") == collect.UsageInsightsTag {
		return false
	}
	for _, suffix := range []string{"/api/v1/query", "/api/v1/query_range"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	for _, part := range []string{"/loki/api/v1/query", "/api/ds/query", "/api/datasources/proxy/"} {
		if strings.Contains(path, part) {
			return true
		}
	}
	return false
}

// Serve starts a test server for h that fails the test on any request
// other than a GET and on any request to a query endpoint (invariant 4:
// read-only, no PromQL or LogQL run beyond the probe marker and the
// per-level bytes query). Such requests get 405 or 403 and never
// reach h. The server is closed when the test ends.
func Serve(t testing.TB, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s: tvt must only send GET requests", r.Method, r.URL.Path)
			http.Error(w, "tvt must only GET", http.StatusMethodNotAllowed)
			return
		}
		if Forbidden(r) {
			t.Errorf("GET %s: tvt must never run queries against a backend that logs them as usage", r.URL.Path)
			http.Error(w, "query endpoints are off limits", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Routes maps an exact URL path to its handler.
type Routes map[string]http.Handler

// NewServer is Serve with handlers chosen by exact URL path; any other path
// gets 404.
func NewServer(t testing.TB, routes Routes) *httptest.Server {
	t.Helper()
	return Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := routes[r.URL.Path]; ok {
			h.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
}

// File serves testdata/<name> (name is relative to Testdata, e.g.
// "lab/prometheus/rules.json"). The file is read now, so a missing fixture
// fails the test instead of turning into an HTTP error a test might expect.
// The content type follows the extension.
func File(t testing.TB, name string) http.Handler {
	t.Helper()
	b := ReadFile(t, name)
	ctype := "text/plain; charset=utf-8"
	switch filepath.Ext(name) {
	case ".json":
		ctype = "application/json"
	case ".yaml", ".yml":
		ctype = "application/yaml"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.Write(b)
	})
}

// ReadFile returns testdata/<name> (or name itself when it is absolute),
// failing the test if it can't be read.
func ReadFile(t testing.TB, name string) []byte {
	t.Helper()
	if !filepath.IsAbs(name) {
		name = filepath.Join(Testdata, name)
	}
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Dashboards maps each recorded dashboard's UID to its fixture file name in
// lab/grafana (dashboard_<uid>.json), so a dashboard the lab gains is served
// as soon as capture-fixtures.sh records it.
func Dashboards(t testing.TB) map[string]string {
	t.Helper()
	return DashboardsIn(t, Lab)
}

// DashboardsIn is Dashboards for the fixtures in dir (a lab snapshot laid
// out like testdata/lab).
func DashboardsIn(t testing.TB, dir string) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "grafana", "dashboard_*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no dashboard fixtures in %s: %v", filepath.Join(dir, "grafana"), err)
	}
	out := map[string]string{}
	for _, p := range paths {
		name := filepath.Base(p)
		out[strings.TrimSuffix(strings.TrimPrefix(name, "dashboard_"), ".json")] = name
	}
	return out
}
