package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/config"
)

// initServer answers the given GET paths with a status and body; anything
// else is 404, and anything but a GET fails the test (init is read-only).
func initServer(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("init sent %s %s", r.Method, r.URL.Path)
		}
		if h, ok := routes[r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func body(status int, s string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status); _, _ = w.Write([]byte(s)) }
}

func authed(token string, status int, s string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(s))
	}
}

func grafanaRoutes(datasources int) map[string]func(http.ResponseWriter, *http.Request) {
	return map[string]func(http.ResponseWriter, *http.Request){
		"/api/health":         body(200, `{"database":"ok"}`),
		"/api/user":           authed("g", 200, `{"login":"sa-1-tvt"}`),
		"/api/search":         authed("g", 200, `[]`),
		"/api/datasources":    authed("g", datasources, `[]`),
		"/api/admin/settings": authed("g", 403, `{}`),
	}
}

func TestInitWritesConfig(t *testing.T) {
	g := initServer(t, grafanaRoutes(200))
	p := initServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/status/buildinfo": body(200, `{"status":"success"}`),
		"/api/v1/status/tsdb":      body(200, `{"status":"success"}`),
		"/api/v1/status/config":    body(200, `{"status":"success","data":{"yaml":"global:\n  query_log_file: /prometheus/query.log\n"}}`),
	})
	gh := initServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/user": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			_, _ = w.Write([]byte(`{}`))
		},
	})
	t.Setenv("TVT_GRAFANA_TOKEN", "g")
	t.Setenv("GITHUB_TOKEN", "gh")
	out := filepath.Join(t.TempDir(), "tvt.yaml")
	var stdout, stderr bytes.Buffer
	code := cmdInit([]string{"--grafana", g.URL, "--prom", p.URL, "--out", out, "--github-api", gh.URL}, strings.NewReader(""), &stdout, &stderr)
	t.Log("\n" + stdout.String() + stderr.String())
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		"ok    grafana     token: signed in as sa-1-tvt",
		"warn  grafana     router log: can't check with a Viewer token",
		"ok    prometheus  query log: on: pass /prometheus/query.log as --prom-query-log",
		"warn  github      token: a classic token with the repo scope",
		"wrote " + out,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q", want)
		}
	}
	cfg, err := config.Load(out)
	if err != nil {
		t.Fatalf("the written tvt.yaml doesn't load: %v", err)
	}
	_ = cfg
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "token: g") || !strings.Contains(string(b), p.URL) {
		t.Errorf("config:\n%s", b)
	}
	// A second run refuses to overwrite it.
	if code := cmdInit([]string{"--grafana", g.URL, "--prom", p.URL, "--out", out}, strings.NewReader(""), &stdout, &stderr); code != exitUsage {
		t.Errorf("overwrite without --force: exit %d", code)
	}
}

func TestInitFails(t *testing.T) {
	g := initServer(t, grafanaRoutes(403))
	m := initServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/prometheus/api/v1/status/buildinfo":        body(200, `{}`),
		"/prometheus/api/v1/cardinality/label_names": body(400, `cardinality analysis is disabled`),
	})
	t.Setenv("TVT_GRAFANA_TOKEN", "g")
	t.Setenv("GITHUB_TOKEN", "")
	out := filepath.Join(t.TempDir(), "tvt.yaml")
	var stdout, stderr bytes.Buffer
	code := cmdInit([]string{"--grafana", g.URL, "--mimir", m.URL, "--out", out}, strings.NewReader(""), &stdout, &stderr)
	t.Log("\n" + stdout.String() + stderr.String())
	if code != exitError {
		t.Fatalf("exit %d, want %d", code, exitError)
	}
	for _, want := range []string{
		"fail  grafana     reads data sources: needs datasources:read: HTTP 403",
		"fail  mimir       cardinality analysis: set limits.cardinality_analysis_enabled: true",
		"not written",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q", want)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("tvt.yaml written despite a failed check")
	}
}

func TestInitGitHubFineGrained(t *testing.T) {
	gh := initServer(t, map[string]func(http.ResponseWriter, *http.Request){"/user": body(200, `{}`)})
	cs := githubChecks(context.Background(), gh.URL, "t")
	if len(cs) != 1 || cs[0].Level != levelOK || !strings.Contains(cs[0].Detail, "fine-grained") {
		t.Fatalf("%+v", cs)
	}
}

// TestIsTerminalDevNull: /dev/null is a character device but not a
// terminal, so tvt init < /dev/null asks nothing.
func TestIsTerminalDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("/dev/null taken for a terminal")
	}
}
