package testutil

import (
	"net/url"

	"fmt"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// recorder captures Errorf calls instead of failing the test.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) Helper() {}

func TestServeRejectsWritesAndQueries(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
		tag          string
	}{
		{"POST", "/api/v1/rules", 405, ""},
		{"PUT", "/anything", 405, ""},
		{"DELETE", "/api/dashboards/uid/x", 405, ""},
		{"GET", "/api/v1/query", 403, ""},
		{"GET", "/api/v1/query_range", 403, ""},
		{"GET", "/prometheus/api/v1/query", 403, ""},
		{"GET", "/prometheus/api/v1/query_range", 403, ""},
		{"GET", "/loki/api/v1/query", 403, ""},
		{"GET", "/loki/api/v1/query_range", 403, ""},
		{"GET", "/api/ds/query", 403, ""},
		{"GET", "/api/datasources/proxy/uid/prom/api/v1/series", 403, ""},
		// Usage Insights is the one proxy read allowed, and only tagged.
		{"GET", "/api/datasources/proxy/uid/" + collect.UsageInsightsUID + "/loki/api/v1/query_range", 403, ""},
		{"GET", "/api/datasources/proxy/uid/grafanacloud-logs/loki/api/v1/query_range", 403, collect.UsageInsightsTag},
		{"GET", "/api/datasources/proxy/uid/" + collect.UsageInsightsUID + "-x/loki/api/v1/query_range", 403, collect.UsageInsightsTag},
		// A probe marker is allowed only as itself, tagged, on its own
		// backend's instant-query endpoint (Prometheus or Mimir, or Loki).
		{"GET", "/api/v1/query?query=" + url.QueryEscape(collect.ProbeQuery("ab12")), 403, ""},
		{"GET", "/api/v1/query?query=" + url.QueryEscape(collect.ProbeQuery("ab12")+" + up"), 403, collect.ProbeTag},
		{"GET", "/api/v1/query_range?query=" + url.QueryEscape(collect.ProbeQuery("ab12")), 403, collect.ProbeTag},
		{"GET", "/loki/api/v1/query?query=" + url.QueryEscape(collect.ProbeQuery("ab12")), 403, collect.ProbeTag},
		{"GET", "/loki/api/v1/query?query=" + url.QueryEscape(collect.LokiProbeQuery("ab12")), 403, ""},
		{"GET", "/loki/api/v1/query?query=" + url.QueryEscape(collect.LokiProbeQuery("ab12")+" or count_over_time({app=\"shop\"}[1m])"), 403, collect.ProbeTag},
		{"GET", "/loki/api/v1/query_range?query=" + url.QueryEscape(collect.LokiProbeQuery("ab12")), 403, collect.ProbeTag},
		{"GET", "/api/v1/query?query=" + url.QueryEscape(collect.LokiProbeQuery("ab12")), 403, collect.ProbeTag},
	} {
		rec := &recorder{TB: t}
		reached := false
		srv := Serve(rec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		if tc.tag != "" {
			req.Header.Set("X-Query-Tags", tc.tag)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status || reached {
			t.Errorf("%s %s: status %d, reached handler %v; want %d and not reached", tc.method, tc.path, resp.StatusCode, reached, tc.status)
		}
		if len(rec.errs) != 1 {
			t.Errorf("%s %s: test failures %q, want exactly one", tc.method, tc.path, rec.errs)
		}
	}
}

func TestServeAllowsReads(t *testing.T) {
	rec := &recorder{TB: t}
	srv := NewServer(rec, Routes{
		"/api/v1/rules":                     File(t, "lab/prometheus/rules.json"),
		"/loki/api/v1/rules":                File(t, "lab/loki/rules.yaml"),
		"/api/v1/status/tsdb":               File(t, "lab/prometheus/tsdb.json"),
		"/loki/api/v1/index/volume":         File(t, "lab/loki/volume_by_stream.json"),
		"/api/v1/series":                    File(t, "lab/prometheus/series_cart_ops.json"),
		"/api/v1/querylog-lookalike-not-it": File(t, "lab/prometheus/buildinfo.json"),
	})
	for path, ctype := range map[string]string{
		"/api/v1/rules":                     "application/json",
		"/loki/api/v1/rules":                "application/yaml",
		"/api/v1/status/tsdb":               "application/json",
		"/loki/api/v1/index/volume":         "application/json",
		"/api/v1/series":                    "application/json",
		"/api/v1/querylog-lookalike-not-it": "application/json",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ctype || len(b) == 0 {
			t.Errorf("GET %s: %d %q, %d bytes", path, resp.StatusCode, resp.Header.Get("Content-Type"), len(b))
		}
	}
	resp, err := http.Get(srv.URL + "/not/routed")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unrouted path: %d, want 404", resp.StatusCode)
	}
	if len(rec.errs) != 0 {
		t.Errorf("reads failed the test: %q", rec.errs)
	}
}

func TestFileMissingFailsTest(t *testing.T) {
	ft := &fatalRecorder{TB: t}
	func() {
		defer func() { recover() }()
		File(ft, "lab/does-not-exist.json")
	}()
	if !ft.fatal || !strings.Contains(ft.msg, "does-not-exist") {
		t.Errorf("missing fixture: fatal %v %q", ft.fatal, ft.msg)
	}
}

type fatalRecorder struct {
	testing.TB
	fatal bool
	msg   string
}

func (f *fatalRecorder) Helper() {}
func (f *fatalRecorder) Fatal(args ...any) {
	f.fatal, f.msg = true, fmt.Sprint(args...)
	panic("fatal")
}

func TestServeAllowsTheProbe(t *testing.T) {
	for path, query := range map[string]string{
		"/api/v1/query":            collect.ProbeQuery("ab12"),
		"/prometheus/api/v1/query": collect.ProbeQuery("ab12"),
		"/loki/api/v1/query":       collect.LokiProbeQuery("ab12"),
	} {
		reached := false
		srv := Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
		req, _ := http.NewRequest("GET", srv.URL+path+"?query="+url.QueryEscape(query), nil)
		req.Header.Set("X-Query-Tags", collect.ProbeTag)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if !reached {
			t.Errorf("%s: probe blocked (%d)", path, resp.StatusCode)
		}
	}
}
