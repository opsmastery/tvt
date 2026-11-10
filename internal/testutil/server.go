package testutil

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

// forbidden reports whether a request path would run a query on a backend
// that logs queries as usage (invariant 4: tvt never runs PromQL or LogQL
// there). It covers Prometheus and Mimir (any prefix), Loki, and Grafana's
// query and datasource-proxy APIs, which forward to those backends.
func forbidden(path string) bool {
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
// read-only, no PromQL or LogQL run). Such requests get 405 or 403 and never
// reach h. The server is closed when the test ends.
func Serve(t testing.TB, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s: tvt must only send GET requests", r.Method, r.URL.Path)
			http.Error(w, "tvt must only GET", http.StatusMethodNotAllowed)
			return
		}
		if forbidden(r.URL.Path) {
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

// ReadFile returns testdata/<name>, failing the test if it can't be read.
func ReadFile(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(Testdata, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
