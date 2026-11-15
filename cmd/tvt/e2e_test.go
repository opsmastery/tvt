//go:build e2e

// End-to-end suite: the real scan against the running tvt-lab, checked
// against the lab's own ground-truth.yaml with the golden test's
// assertions. Built only with -tags e2e, so CI and the pre-push hook never
// need a lab:
//
//	make e2e                                  # exports Grafana's log, then runs this
//	go test -tags e2e ./cmd/tvt -run E2E -v   # after `make -C ../tvt-lab export-logs`
//
// Needs TVT_GRAFANA_TOKEN (a Viewer service account token for the lab's
// Grafana). Backends default to the lab's localhost ports; override with
// TVT_E2E_GRAFANA, TVT_E2E_PROM, TVT_E2E_MIMIR, TVT_E2E_LOKI, and the lab
// checkout with TVT_LAB_DIR.
//
// Every request goes through a transport that fails the test on anything
// but a GET, and on any query endpoint other than tvt's probe marker: tvt
// is read-only against real backends too.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/httpx"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
)

type liveLab struct {
	dir                        string
	grafana, prom, mimir, loki string
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// newLiveLab finds the lab checkout and checks every backend answers, so a
// stopped lab fails with one clear message instead of a scan error.
func newLiveLab(t *testing.T) liveLab {
	t.Helper()
	if os.Getenv("TVT_GRAFANA_TOKEN") == "" {
		t.Fatal("TVT_GRAFANA_TOKEN is not set: the e2e suite needs the lab's Viewer service account token")
	}
	dir, err := filepath.Abs(envOr("TVT_LAB_DIR", "../../../tvt-lab"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ground-truth.yaml")); err != nil {
		t.Fatalf("no tvt-lab checkout at %s (set TVT_LAB_DIR): %v", dir, err)
	}
	l := liveLab{
		dir:     dir,
		grafana: envOr("TVT_E2E_GRAFANA", "http://localhost:3000"),
		prom:    envOr("TVT_E2E_PROM", "http://localhost:9090"),
		mimir:   envOr("TVT_E2E_MIMIR", "http://localhost:9009"),
		loki:    envOr("TVT_E2E_LOKI", "http://localhost:3100"),
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	for name, u := range map[string]string{
		"grafana": l.grafana + "/api/health", "prometheus": l.prom + "/-/ready",
		"mimir": l.mimir + "/ready", "loki": l.loki + "/loki/api/v1/status/buildinfo",
	} {
		resp, err := hc.Get(u)
		if err != nil {
			t.Fatalf("lab %s is not answering at %s (is the lab up? `make up-mimir` in tvt-lab): %v", name, u, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("lab %s: %s returned %s", name, u, resp.Status)
		}
	}
	// The Grafana log is exported, not live: a stale export would make
	// viewed dashboards look unviewed.
	fi, err := os.Stat(filepath.Join(dir, "lab-logs", "grafana.log"))
	if err != nil || time.Since(fi.ModTime()) > 2*time.Hour {
		t.Fatalf("lab-logs/grafana.log is missing or older than 2h: run `make -C %s export-logs` (or `make e2e`)", dir)
	}
	return l
}

// config writes a tvt.yaml pointing at the live lab's SLOs and runbooks.
func (l liveLab) config(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tvt.yaml")
	body := fmt.Sprintf(`pricecard: grafana-cloud
slos:
  paths: [%q]
postmortems:
  paths: [%q]
runbooks:
  fetch: false
  mirrors:
    - url_prefix: https://github.com/Opsmastery-Summit/tvt-lab/blob/main/
      path: %q
`, filepath.Join(l.dir, "slos"), filepath.Join(l.dir, "postmortems"), l.dir)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (l liveLab) logs(name string) string { return filepath.Join(l.dir, "lab-logs", name) }

// grafanaLogs copies the exported Grafana logs, keeping only requests from
// the lab's own containers (TVT_E2E_LAB_NET, default the podman network
// 10.89.). The ground truth models the lab's simulated viewers; someone
// opening a dashboard from the host's browser is real use that tvt rightly
// counts, but it would turn a "never viewed" dashboard viewed and fail the
// suite for a reason that isn't a tvt bug. Dropped host views are logged.
func (l liveLab) grafanaLogs(t *testing.T) stringList {
	t.Helper()
	net := "remote_addr=" + envOr("TVT_E2E_LAB_NET", "10.89.")
	paths, err := filepath.Glob(l.logs("grafana.log*"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no lab-logs/grafana.log*: %v", err)
	}
	dir := t.TempDir()
	dropped := map[string]int{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var keep bytes.Buffer
		for _, line := range bytes.SplitAfter(b, []byte("\n")) {
			s := string(line)
			if strings.Contains(s, "remote_addr=") && !strings.Contains(s, net) {
				if i := strings.Index(s, "path=/api/dashboards/uid/"); i >= 0 && strings.Contains(s, "referer=") {
					uid, _, _ := strings.Cut(s[i+len("path=/api/dashboards/uid/"):], " ")
					dropped[uid]++
				}
				continue
			}
			keep.Write(line)
		}
		out := filepath.Join(dir, filepath.Base(p))
		if err := os.WriteFile(out, keep.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		// Rotation order and the active file's age come from mtimes.
		if fi, err := os.Stat(p); err == nil {
			_ = os.Chtimes(out, fi.ModTime(), fi.ModTime())
		}
	}
	if len(dropped) > 0 {
		t.Logf("dropped dashboard requests from outside the lab network: %v", dropped)
	}
	return stringList{filepath.Join(dir, "grafana.log*")}
}

// readOnly fails the test on any request a read-only tool must not send,
// then hands the rest to next (http.DefaultTransport when nil).
type readOnly struct {
	t    *testing.T
	next http.RoundTripper
	mu   sync.Mutex
	sent int
}

func (r *readOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.sent++
	r.mu.Unlock()
	if req.Method != http.MethodGet {
		r.t.Errorf("tvt sent %s %s: read-only tools only GET", req.Method, req.URL.Redacted())
		return nil, fmt.Errorf("e2e: refused %s", req.Method)
	}
	p := req.URL.Path
	if strings.HasSuffix(p, "/api/v1/query") || strings.HasSuffix(p, "/api/v1/query_range") ||
		strings.HasPrefix(p, "/api/ds/query") || strings.Contains(p, "/api/datasources/proxy/") {
		q := req.URL.Query().Get("query")
		_, probe := collect.ProbeNonce(q)
		if strings.HasPrefix(p, "/loki/") {
			_, probe = collect.LokiProbeNonce(q) // S5 item 5
		}
		if !probe || !strings.HasSuffix(p, "/api/v1/query") || strings.Contains(p, "/api/datasources/proxy/") {
			r.t.Errorf("tvt ran a query against %s: only the probe marker is allowed", req.URL.Redacted())
			return nil, fmt.Errorf("e2e: refused query")
		}
	}
	if r.next != nil {
		return r.next.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// scanClient is the client tvt uses in production (httpx.DefaultClient:
// its timeout and redirect policy) with the read-only guard in front.
func scanClient(ro *readOnly) *http.Client {
	c := httpx.DefaultClient()
	c.Transport = ro
	return c
}

func useReadOnly(t *testing.T) *readOnly {
	t.Helper()
	ro := &readOnly{t: t}
	old := scanHTTP
	scanHTTP = scanClient(ro)
	t.Cleanup(func() { scanHTTP = old })
	return ro
}

func liveScan(t *testing.T, o scanOptions) (scanGraph, store.ScanInfo, []graph.SourceCoverage, string) {
	t.Helper()
	ro := useReadOnly(t)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := runScan(ctx, o, os.Getenv, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	t.Log("\n" + out.String())
	if ro.sent == 0 {
		t.Fatal("the scan sent no requests through the read-only transport")
	}
	st, err := store.Open(ctx, o.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := st.Sources(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	return readGraph(t, o.DB), info, srcs, out.String()
}

func liveGroundTruth(t *testing.T, l liveLab) groundTruth {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(l.dir, "ground-truth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return parseGroundTruth(t, b)
}

// TestE2EScanPrometheus is TestScanLab against the live lab: Prometheus,
// Loki and Grafana with every log the lab writes, the probe sent for real.
func TestE2EScanPrometheus(t *testing.T) {
	l := newLiveLab(t)
	db := filepath.Join(t.TempDir(), "tvt.db")
	g, info, srcs, out := liveScan(t, scanOptions{
		Grafana: l.grafana, Prom: l.prom, Loki: l.loki, DB: db,
		Window: 30, LabelTop: 2, ProbeWait: 45 * time.Second,
		GrafanaLogs:   l.grafanaLogs(t),
		PromQueryLogs: stringList{l.logs("prometheus/query.log")},
		LokiQueryLogs: stringList{l.logs("loki.log")},
		Config:        l.config(t),
	})
	for _, want := range []string{"probe found", "scan 1 written", "as sa-1-tvt"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}

	assertCosts(t, db, "prom", "grafana-cloud-pro", false)
	gt := liveGroundTruth(t, l)
	assertMetrics(t, g, "prom", gt.Metrics)
	want := assertRules(t, g, gt.Rules, "prometheus", "loki", "grafana")
	var rules int
	for _, c := range g.consumers {
		if (c.Kind == graph.ConsumerAlertRule || c.Kind == graph.ConsumerRecordingRule) && !strings.HasPrefix(c.ID, "qlog:") {
			rules++
		}
	}
	if rules != want {
		t.Errorf("%d rule consumers, want the %d ground-truth rules", rules, want)
	}
	assertDashboards(t, g, "1", gt)
	assertHardKeepStreams(t, g, gt.Streams)
	assertReferences(t, g, "prom", gt)
	assertClasses(t, g, "prom", gt)
	assertDeclared(t, g, "prom", l.dir, gt)

	for _, ld := range g.loads {
		if ld.Login == "sa-1-tvt" {
			t.Errorf("load %+v by tvt itself should have been excluded", ld)
		}
	}
	if len(g.loads) == 0 {
		t.Error("no dashboard loads: is the lab's viewer running?")
	}

	// Every log the lab writes is configured. The Prometheus and Loki logs
	// are proven by their probes (Loki's since S5 item 5). Declared
	// sources are read in full.
	for _, s := range srcs {
		switch s.Source {
		case scan.SourceSLOs, scan.SourceRunbooks, scan.SourceDatasources:
			if !s.Configured || s.Coverage != 1 || s.Gap != "" {
				t.Errorf("source %+v", s)
			}
		case sourcePromQueryLog, sourceLokiQueryLog:
			if !s.Configured || s.Coverage <= 0 || s.Gap != "" {
				t.Errorf("query log not proven: %+v", s)
			}
		default:
			if !s.Configured || s.Coverage <= 0 {
				t.Errorf("source %+v", s)
			}
		}
	}
	if info.LogCoverage <= 0 {
		t.Errorf("log coverage = %v", info.LogCoverage)
	}
}

// TestE2EScanMimir is TestScanLabMimir against the live lab: Mimir's
// partitioned cardinality API and ruler, the query-frontend log proven by
// the probe.
func TestE2EScanMimir(t *testing.T) {
	l := newLiveLab(t)
	db := filepath.Join(t.TempDir(), "tvt.db")
	g, _, srcs, out := liveScan(t, scanOptions{
		Grafana: l.grafana, Mimir: l.mimir, Loki: l.loki, DB: db,
		Window: 30, ProbeWait: 45 * time.Second,
		GrafanaLogs:    l.grafanaLogs(t),
		MimirQueryLogs: stringList{l.logs("mimir.log")},
		Config:         l.config(t),
	})
	if !strings.Contains(out, "scan 1 written") {
		t.Errorf("output missing %q", "scan 1 written")
	}
	for _, s := range g.signals {
		if strings.HasPrefix(s.ID, "prom:") {
			t.Errorf("--mimir scan wrote a prom signal: %s", s.ID)
		}
	}

	assertCosts(t, db, "mimir", "grafana-cloud-pro", true)
	gt := liveGroundTruth(t, l)
	assertMetrics(t, g, "mimir", gt.Metrics)
	assertMetrics(t, g, "mimir", gt.Mimir.Metrics)
	mgt := gt
	mgt.Metrics, mgt.Labels, mgt.Streams = gt.Mimir.Metrics, nil, nil
	assertClasses(t, g, "mimir", mgt)
	want := assertRules(t, g, gt.Mimir.Rules, "mimir") + assertRules(t, g, gt.Rules, "loki", "grafana")
	var rules int
	for _, c := range g.consumers {
		if (c.Kind == graph.ConsumerAlertRule || c.Kind == graph.ConsumerRecordingRule) && !strings.HasPrefix(c.ID, "qlog:") {
			rules++
		}
	}
	if rules != want {
		t.Errorf("%d rule consumers, want the %d ground-truth Mimir and Loki rules", rules, want)
	}
	assertDashboards(t, g, "1", gt)
	assertHardKeepStreams(t, g, gt.Streams)
	assertHardKeepMetrics(t, g, "mimir", gt.Mimir.Metrics)

	for _, s := range srcs {
		if s.Source == sourceMimirQueryLog && (!s.Configured || s.Coverage <= 0 || s.Gap != "") {
			t.Errorf("mimir query log not proven: %+v", s)
		}
	}
}

// TestE2EReadOnlyTransport checks the guard itself: a POST or a query that
// isn't the probe is refused before it leaves the machine.
// TestE2EReport is S4's done-when on the live lab: from a cold start (no
// database), scan and report the top 50 in under 30 minutes; no hard
// keep, keep_high, dormant or record signal is ever recommended; every row
// lists its evidence gaps.
func TestE2EReport(t *testing.T) {
	l := newLiveLab(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "tvt.db")
	start := time.Now()
	g, _, _, _ := liveScan(t, scanOptions{
		Grafana: l.grafana, Prom: l.prom, Loki: l.loki, DB: db,
		Window: 30, LabelTop: 2, ProbeWait: 45 * time.Second,
		GrafanaLogs:   l.grafanaLogs(t),
		PromQueryLogs: stringList{l.logs("prometheus/query.log")},
		LokiQueryLogs: stringList{l.logs("loki.log")},
		Config:        l.config(t),
	})
	var out bytes.Buffer
	jsonPath, htmlPath := filepath.Join(dir, "report.json"), filepath.Join(dir, "report.html")
	if err := runReport(context.Background(), reportOptions{DB: db, Config: l.config(t), JSON: jsonPath, HTML: htmlPath, Top: 50}, &out); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("cold start to report: %s\n%s", elapsed.Round(time.Second), out.String())
	if elapsed > 30*time.Minute {
		t.Errorf("cold start to report took %s, want under 30m", elapsed)
	}
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var r report.Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(htmlPath); err != nil {
		t.Errorf("no HTML report: %v", err)
	}
	if len(r.Recommendations) == 0 || len(r.Recommendations) > 50 {
		t.Errorf("%d recommendations, want 1-50", len(r.Recommendations))
	}
	for _, rec := range r.Recommendations {
		if c := g.signals[rec.Signal.ID].Class; c != string(score.ClassCandidate) {
			t.Errorf("%s (%s) recommended", rec.Signal.ID, c)
		}
		if len(rec.Evidence.Gaps)+len(rec.Evidence.Acknowledged) == 0 {
			t.Errorf("%s lists no evidence gaps", rec.ID)
		}
	}
}

func TestE2EReadOnlyTransport(t *testing.T) {
	ft := &testing.T{}
	ro := &readOnly{t: ft}
	for _, req := range []*http.Request{
		mustReq(t, http.MethodPost, "http://127.0.0.1:1/api/v1/query"),
		mustReq(t, http.MethodGet, "http://127.0.0.1:1/api/v1/query?query="+url.QueryEscape("up")),
		// Each marker only on its own backend's instant-query endpoint.
		mustReq(t, http.MethodGet, "http://127.0.0.1:1/api/v1/query?query="+url.QueryEscape(collect.LokiProbeQuery("ab12"))),
		mustReq(t, http.MethodGet, "http://127.0.0.1:1/loki/api/v1/query?query="+url.QueryEscape(collect.ProbeQuery("ab12"))),
		mustReq(t, http.MethodGet, "http://127.0.0.1:1/loki/api/v1/query_range?query="+url.QueryEscape(collect.LokiProbeQuery("ab12"))),
		mustReq(t, http.MethodGet, "http://127.0.0.1:1/loki/api/v1/query?query="+url.QueryEscape(`count_over_time({app="shop"}[1m])`)),
	} {
		if _, err := ro.RoundTrip(req); err == nil {
			t.Errorf("%s %s was let through", req.Method, req.URL)
		}
	}
}

// TestE2EScanClientRefusesDowngrade: the scan's client is the production
// one, so an https backend redirecting to plain http is refused and the
// token never reaches the http server.
func TestE2EScanClientRefusesDowngrade(t *testing.T) {
	var leaked string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
	}))
	defer tls.Close()
	c := scanClient(&readOnly{t: t, next: tls.Client().Transport})
	req, err := httpx.NewRequest(context.Background(), tls.URL, "/api/search", nil, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("https->http redirect was followed")
	}
	if leaked != "" {
		t.Fatalf("plain-http server received Authorization %q", leaked)
	}
}

func mustReq(t *testing.T, method, u string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
