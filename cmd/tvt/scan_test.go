package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
)

var lab = testutil.Lab

// scanTime is just after the last line of testdata/lab/grafana/grafana.log.
var scanTime = time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC)

// fixtureServer serves files from testdata/lab by URL path through the
// shared read-only server. A handler in override takes precedence.
func fixtureServer(t *testing.T, files map[string]string, override map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	routes := testutil.Routes{}
	for path, name := range files {
		routes[path] = testutil.File(t, "lab/"+name)
	}
	for path, h := range override {
		routes[path] = h
	}
	return testutil.NewServer(t, routes)
}

// syntheticSeries answers /api/v1/series with two series per metric that
// differ in one label, so label cardinality is predictable.
func syntheticSeries(w http.ResponseWriter, r *http.Request) {
	m := r.URL.Query().Get("match[]")
	fmt.Fprintf(w, `{"status":"success","data":[{"__name__":%[1]q,"job":"shop","flag":"a"},{"__name__":%[1]q,"job":"shop","flag":"b"}]}`, m)
}

type labServers struct {
	prom, grafana, loki *httptest.Server
	// promLog is a copy of the recorded Prometheus query log that the
	// fixture Prometheus appends to, as the real one does, when tvt sends
	// its probe marker.
	promLog string
	probes  *atomic.Int32
}

// probeSink answers tvt's probe query like Prometheus or Mimir and, when
// log is set, writes the marker into that query log.
func probeSink(t *testing.T, log string, n *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if _, ok := collect.ProbeNonce(q); !ok {
			t.Errorf("query endpoint got %q, not a probe marker", q)
		}
		n.Add(1)
		if log != "" {
			line, _ := json.Marshal(map[string]any{
				"httpRequest": map[string]string{"clientIP": "10.89.0.20", "path": "/api/v1/query"},
				"params":      map[string]string{"query": q}, "time": scanTime.Format(time.RFC3339Nano),
			})
			f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(f, "%s\n", line)
			f.Close()
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}
}

func newLab(t *testing.T, promOverride map[string]http.HandlerFunc) labServers {
	t.Helper()
	if promOverride == nil {
		promOverride = map[string]http.HandlerFunc{}
	}
	if _, ok := promOverride["/api/v1/series"]; !ok {
		promOverride["/api/v1/series"] = syntheticSeries
	}
	promLog := filepath.Join(t.TempDir(), "query.log")
	data, err := os.ReadFile(filepath.Join(lab, "prometheus/query.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promLog, data, 0o600); err != nil {
		t.Fatal(err)
	}
	probes := &atomic.Int32{}
	if _, ok := promOverride["/api/v1/query"]; !ok {
		promOverride["/api/v1/query"] = probeSink(t, promLog, probes)
	}
	prom := fixtureServer(t, map[string]string{
		"/api/v1/status/tsdb": "prometheus/tsdb.json",
		"/api/v1/rules":       "prometheus/rules.json",
	}, promOverride)
	graf := fixtureServer(t, map[string]string{
		"/api/search":                       "grafana/search.json",
		"/api/datasources":                  "grafana/datasources.json",
		"/api/dashboards/uid/shop-overview": "grafana/dashboard_shop-overview.json",
		"/api/dashboards/uid/legacy-queue":  "grafana/dashboard_legacy-queue.json",
		"/api/ruler/grafana/api/v1/rules":   "grafana/ruler_config.json",
	}, map[string]http.HandlerFunc{
		"/api/user": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer test-grafana-token" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"message":"Unauthorized"}`)
				return
			}
			fmt.Fprint(w, `{"login":"sa-1-tvt"}`)
		},
		"/api/org": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"id":1,"name":"Main Org."}`) },
	})
	loki := fixtureServer(t, map[string]string{
		"/loki/api/v1/rules":       "loki/rules.yaml",
		"/prometheus/api/v1/rules": "loki/prom_rules.json",
	}, nil)
	return labServers{prom, graf, loki, promLog, probes}
}

func labOptions(l labServers, db string) scanOptions {
	return scanOptions{
		Grafana:       l.grafana.URL,
		Prom:          l.prom.URL,
		Loki:          l.loki.URL,
		DB:            db,
		Window:        30,
		LabelTop:      2,
		GrafanaLogs:   stringList{filepath.Join(lab, "grafana/grafana.log")},
		PromQueryLogs: stringList{l.promLog},
		ProbeWait:     2 * time.Second,
	}
}

func env(name string) string {
	return map[string]string{"TVT_GRAFANA_TOKEN": "test-grafana-token"}[name]
}

func setNow(t *testing.T) {
	t.Helper()
	old := scanNow
	scanNow = func() time.Time { return scanTime }
	t.Cleanup(func() { scanNow = old })
}

func TestScanLab(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	db := filepath.Join(t.TempDir(), "tvt.db")
	var out bytes.Buffer
	if err := runScan(context.Background(), labOptions(l, db), env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	t.Log("\n" + out.String())
	for _, want := range []string{"prom ", "5 rules", "loki         1 rule (", "2 dashboards", "as sa-1-tvt", "grafana log", "scan 1 written", "19 dashboard loads (19 in a browser)", ", 19 dashboard loads,", " references, "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q", want)
		}
	}

	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Every metric in the TSDB status is a signal, the ground truth's included.
	var tsdb struct {
		Data struct {
			SeriesCountByMetricName []struct {
				Name string `json:"name"`
			} `json:"seriesCountByMetricName"`
		} `json:"data"`
	}
	readJSON(t, "prometheus/tsdb.json", &tsdb)
	sigs, err := st.Signals(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]graph.Signal{}
	for _, s := range sigs {
		ids[s.ID] = s
	}
	for _, m := range tsdb.Data.SeriesCountByMetricName {
		if _, ok := ids["prom:metric:"+m.Name]; !ok {
			t.Errorf("metric %s is not a signal", m.Name)
		}
	}
	// --label-top 2: the two largest metrics, flag and job each.
	if s, ok := ids["prom:label:shop_feature_flag_evaluations_total:flag"]; !ok || s.Kind != graph.KindLabel {
		t.Errorf("label signal missing: %+v", s)
	}
	var labels int
	for _, s := range sigs {
		if s.Kind == graph.KindLabel {
			labels++
		}
	}
	if labels != 4 {
		t.Errorf("%d label signals, want 4", labels)
	}

	// Ground truth, Sprint 1 rows: every seeded metric is a signal; every
	// lab rule is a consumer, Grafana-managed ones included; Shop Overview is viewed and Legacy Queue
	// isn't; the Loki alert's stream is hard-keep.
	gt := loadGroundTruth(t)
	g := readGraph(t, db)
	assertMetrics(t, g, "prom", gt.Metrics)
	want := assertRules(t, g, gt.Rules, "prometheus", "loki", "grafana")
	assertGrafanaRule(t, g)
	var rules int
	for _, c := range g.consumers {
		if c.Kind == graph.ConsumerAlertRule || c.Kind == graph.ConsumerRecordingRule {
			rules++
			// Sprint 2 item 16: every ruler's interval, for cost per rule.
			if want := map[string]time.Duration{"prometheus": 30 * time.Second, "loki": time.Minute, "grafana": time.Minute}[strings.SplitN(c.ID, ":", 3)[1]]; c.Interval != want {
				t.Errorf("rule %s: interval %s, want %s", c.ID, c.Interval, want)
			}
		}
	}
	if rules != want {
		t.Errorf("%d rule consumers, want the %d ground-truth rules", rules, want)
	}
	assertDashboards(t, g, "1", gt)
	assertHardKeepStreams(t, g, gt.Streams)
	// Sprint 2 item 3: references resolve to the right signals, and every
	// lab query parses, so none is a guess. No query creates a metric the
	// scan didn't collect (all the lab's names exist).
	assertReferences(t, g, "prom", gt)
	assertClasses(t, g, "prom", gt)
	for _, r := range g.refs {
		if r.Assumed {
			t.Errorf("lab reference %+v is assumed: %v", r, r.Reasons)
		}
	}
	var metricSignals int
	for _, s := range g.signals {
		if s.Kind == graph.KindMetric {
			metricSignals++
		}
	}
	if metricSignals != len(tsdb.Data.SeriesCountByMetricName) {
		t.Errorf("%d metric signals, want the %d collected", metricSignals, len(tsdb.Data.SeriesCountByMetricName))
	}

	// tvt's own fetches (its login, sa-1-tvt) are not loads.
	if len(g.loads) == 0 {
		t.Error("no dashboard loads")
	}
	for _, ld := range g.loads {
		if ld.Login == "sa-1-tvt" {
			t.Errorf("load %+v by tvt itself should have been excluded", ld)
		}
	}

	// The Grafana and Prometheus logs are configured, the probe proved the
	// query log, and each covers part of the window; Loki's query log is
	// recorded as not configured.
	srcs, err := st.Sources(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 3 || l.probes.Load() != 1 {
		t.Fatalf("sources = %+v, probes %d", srcs, l.probes.Load())
	}
	for _, s := range srcs {
		if s.Source == sourceLokiQueryLog {
			if s.Configured || s.Coverage != 0 || s.Gap != "" {
				t.Errorf("source %+v", s)
			}
			continue
		}
		if !s.Configured || s.Coverage <= 0 || s.Coverage >= 1 || s.Gap != "" {
			t.Errorf("source %+v", s)
		}
	}
	if info.LogCoverage <= 0 || info.LogCoverage >= 1 {
		t.Errorf("log coverage = %v", info.LogCoverage)
	}

	// A second scan is a new scan, not a duplicate.
	out.Reset()
	if err := runScan(ctx, labOptions(l, db), env, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "scan 2 written") {
		t.Errorf("second scan: %s", out.String())
	}
}

func TestScanWithoutLogs(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	o := labOptions(l, filepath.Join(t.TempDir(), "tvt.db"))
	o.GrafanaLogs, o.PromQueryLogs, o.Loki = nil, nil, ""
	var out bytes.Buffer
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"not configured (--grafana-log)", "not configured (--prom-query-log)", "log coverage 0%"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// tvt scan --mimir against the Mimir capture yields the same shop metrics as
// --prom against the Prometheus capture (both taken from the same lab), and
// Mimir's rules are consumers.
func TestScanLabMimir(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	top, err := os.ReadFile(filepath.Join(lab, "mimir/label_values_name.json"))
	if err != nil {
		t.Fatal(err)
	}
	names := &testutil.MimirNames{T: t, Names: testutil.LoadMimirNames(t, filepath.Join(lab, "mimir/label_values_name_by_prefix.json")), Top: top}
	mimirSrv := fixtureServer(t, map[string]string{"/prometheus/api/v1/rules": "mimir/rules.json"},
		map[string]http.HandlerFunc{testutil.MimirLabelValuesPath: names.ServeHTTP})

	db := filepath.Join(t.TempDir(), "tvt.db")
	o := labOptions(l, db)
	// Labels: the capture has label counts for one metric only, and the
	// Mimir collector's label calls are covered by its own tests.
	o.Prom, o.Mimir, o.LabelTop = "", mimirSrv.URL, 0
	var out bytes.Buffer
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	t.Log("\n" + out.String())
	for _, want := range []string{"mimir        1498 metrics", "3 rules", "loki         1 rule (", "scan 1 written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q", want)
		}
	}

	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sigs, err := st.Signals(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	mimirShop := map[string]bool{}
	for _, s := range sigs {
		if strings.HasPrefix(s.ID, "prom:") {
			t.Errorf("--mimir scan wrote a prom signal: %s", s.ID)
		}
		if s.Kind == graph.KindMetric && strings.HasPrefix(s.Name, "shop_") {
			if s.ID != "mimir:metric:"+s.Name || s.Source != "mimir" {
				t.Errorf("signal %+v, want a mimir metric", s)
			}
			mimirShop[s.Name] = true
		}
	}
	var tsdb struct {
		Data struct {
			SeriesCountByMetricName []struct {
				Name string `json:"name"`
			} `json:"seriesCountByMetricName"`
		} `json:"data"`
	}
	readJSON(t, "prometheus/tsdb.json", &tsdb)
	promShop := map[string]bool{}
	for _, m := range tsdb.Data.SeriesCountByMetricName {
		if strings.HasPrefix(m.Name, "shop_") {
			promShop[m.Name] = true
		}
	}
	if len(promShop) == 0 || fmt.Sprint(mimirShop) != fmt.Sprint(promShop) {
		t.Errorf("shop metrics differ:\nmimir %v\nprom  %v", mimirShop, promShop)
	}

	// Ground truth: Prometheus remote-writes every series, so every seeded
	// metric is a Mimir signal too, plus the metric only Mimir's ruler reads;
	// Mimir's and Loki's rules are consumers; views are the same.
	gt := loadGroundTruth(t)
	g := readGraph(t, db)
	assertMetrics(t, g, "mimir", gt.Metrics)
	assertMetrics(t, g, "mimir", gt.Mimir.Metrics)
	// Classes: only Mimir's own entries. The shared hard keeps come from
	// Prometheus's alert rules, which a --mimir scan doesn't read (it reads
	// Mimir's ruler), so there they are rightly keep_high or dormant.
	mgt := gt
	mgt.Metrics, mgt.Labels, mgt.Streams = gt.Mimir.Metrics, nil, nil
	assertClasses(t, g, "mimir", mgt)
	want := assertRules(t, g, gt.Mimir.Rules, "mimir") + assertRules(t, g, gt.Rules, "loki", "grafana")
	var rules int
	for _, c := range g.consumers {
		if c.Kind == graph.ConsumerAlertRule || c.Kind == graph.ConsumerRecordingRule {
			rules++
		}
	}
	if rules != want {
		t.Errorf("%d rule consumers, want the %d ground-truth Mimir and Loki rules", rules, want)
	}
	assertDashboards(t, g, "1", gt)
	assertHardKeepStreams(t, g, gt.Streams)
	// The metric only Mimir's ruler reads is protected by it.
	assertHardKeepMetrics(t, g, "mimir", gt.Mimir.Metrics)
}

// A Mimir without cardinality analysis fails the scan with the setting to
// change, and writes nothing.
func TestScanMimirAnalysisDisabled(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	body, err := os.ReadFile(filepath.Join(lab, "mimir/cardinality_disabled.txt"))
	if err != nil {
		t.Fatal(err)
	}
	mimirSrv := fixtureServer(t, nil, map[string]http.HandlerFunc{testutil.MimirLabelValuesPath: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(body)
	}})
	db := filepath.Join(t.TempDir(), "tvt.db")
	o := labOptions(l, db)
	o.Prom, o.Mimir = "", mimirSrv.URL
	err = runScan(context.Background(), o, env, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "limits.cardinality_analysis_enabled: true") {
		t.Fatalf("err = %v, want the setting named", err)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Errorf("database created after a failed scan: %v", err)
	}
}

// A failed or interrupted scan never creates or touches the database.
func TestScanAborts(t *testing.T) {
	setNow(t)
	failRules := map[string]http.HandlerFunc{"/api/v1/rules": func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"status":"error","errorType":"internal","error":"boom"}`)
	}}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name    string
		prom    map[string]http.HandlerFunc
		ctx     context.Context
		env     func(string) string
		logs    stringList
		loki    http.HandlerFunc
		wantErr string
	}{
		{"rules fail", failRules, context.Background(), env, nil, nil, "boom"},
		{"no grafana token", nil, context.Background(), func(string) string { return "" }, nil, nil, "TVT_GRAFANA_TOKEN is not set"},
		{"not a grafana log", nil, context.Background(), env, stringList{filepath.Join(lab, "prometheus/query.log")}, nil, "no line parsed as a Grafana log entry"},
		{"missing log", nil, context.Background(), env, stringList{filepath.Join(lab, "grafana/nope.log")}, nil, "grafana router log"},
		{"interrupted", nil, cancelled, env, nil, nil, "context canceled"},
		{"loki rules fail", nil, context.Background(), env, nil, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "ruler unavailable", http.StatusServiceUnavailable)
		}, "ruler unavailable"},
		{"loki rules malformed", nil, context.Background(), env, nil, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "ns:\n  - name: g\n    rules:\n      - expr: '{a=\"b\"}'\n")
		}, "exactly one of alert or record"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t, tc.prom)
			db := filepath.Join(t.TempDir(), "tvt.db")
			o := labOptions(l, db)
			if tc.logs != nil {
				o.GrafanaLogs = tc.logs
			}
			if tc.loki != nil {
				o.Loki = testutil.Serve(t, tc.loki).URL
			}
			var out bytes.Buffer
			err := runScan(tc.ctx, o, tc.env, &out)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "test-grafana-token") {
				t.Error("error leaks the token")
			}
			if _, err := os.Stat(db); !os.IsNotExist(err) {
				t.Errorf("database created after a failed scan: %v", err)
			}
		})
	}
}

// A scan that fails after the store is open removes a database it created
// and leaves an existing one with its previous scans.
func TestScanFailsMidWrite(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	old := scanBeforeWrite
	t.Cleanup(func() { scanBeforeWrite = old })
	fail := func() error { return context.Canceled }

	dir := t.TempDir()
	db := filepath.Join(dir, "tvt.db")
	scanBeforeWrite = fail
	if err := runScan(context.Background(), labOptions(l, db), env, &bytes.Buffer{}); err == nil {
		t.Fatal("scan succeeded")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Errorf("files left behind: %v", left)
	}

	scanBeforeWrite = old
	if err := runScan(context.Background(), labOptions(l, db), env, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	scanBeforeWrite = fail
	if err := runScan(context.Background(), labOptions(l, db), env, &bytes.Buffer{}); err == nil {
		t.Fatal("scan succeeded")
	}
	st, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("existing database gone: %v", err)
	}
	defer st.Close()
	if scans, err := st.Scans(context.Background()); err != nil || len(scans) != 1 {
		t.Errorf("scans = %v, %v; want the one from before", scans, err)
	}
}

func TestMimirAPI(t *testing.T) {
	for in, want := range map[string]string{
		"http://mimir:9009":            "http://mimir:9009/prometheus",
		"http://mimir:9009/":           "http://mimir:9009/prometheus",
		"http://mimir:9009/prometheus": "http://mimir:9009/prometheus",
		"http://gw/mimir/prometheus/":  "http://gw/mimir/prometheus/",
		"https://gw/tenant-a":          "https://gw/tenant-a/prometheus",
	} {
		if got := mimirAPI(in); got != want {
			t.Errorf("mimirAPI(%q) = %q, want %q", in, got, want)
		}
	}
}

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(lab, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// Item 4 on Mimir and Loki: Mimir's query stats attribute Grafana's panel
// queries to their panel; Loki's metrics.go lines carry the on-call ad hoc
// query. The Mimir capture holds a real probe marker (nonce pinned here);
// the Prometheus log in a --mimir scan can't be probed, so it is a gap.
func TestScanQueryLogsMimirLoki(t *testing.T) {
	old, oldNonce := scanNow, newProbeNonce
	scanNow = func() time.Time { return time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC) }
	newProbeNonce = func() string { return "0f1e2d3c4b5a6978" }
	t.Cleanup(func() { scanNow, newProbeNonce = old, oldNonce })

	l := newLab(t, nil)
	top, err := os.ReadFile(filepath.Join(lab, "mimir/label_values_name.json"))
	if err != nil {
		t.Fatal(err)
	}
	names := &testutil.MimirNames{T: t, Names: testutil.LoadMimirNames(t, filepath.Join(lab, "mimir/label_values_name_by_prefix.json")), Top: top}
	mimirProbes := &atomic.Int32{}
	mimirSrv := fixtureServer(t, map[string]string{"/prometheus/api/v1/rules": "mimir/rules.json"},
		map[string]http.HandlerFunc{testutil.MimirLabelValuesPath: names.ServeHTTP, "/prometheus/api/v1/query": probeSink(t, "", mimirProbes)})

	db := filepath.Join(t.TempDir(), "tvt.db")
	o := labOptions(l, db)
	o.Prom, o.Mimir, o.LabelTop = "", mimirSrv.URL, 0
	o.MimirQueryLogs = stringList{filepath.Join(lab, "mimir/query.log")}
	o.LokiQueryLogs = stringList{filepath.Join(lab, "loki/query.log")}
	var out bytes.Buffer
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	t.Log("\n" + out.String())
	if mimirProbes.Load() != 1 || l.probes.Load() != 0 {
		t.Errorf("probes: mimir %d, prometheus %d; want 1 and 0", mimirProbes.Load(), l.probes.Load())
	}

	ctx := context.Background()
	st, err := store.Open(ctx, db)
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
	bySource := map[string]graph.SourceCoverage{}
	for _, s := range srcs {
		bySource[s.Source] = s
	}
	if s := bySource[sourceMimirQueryLog]; !s.Configured || s.Gap != "" || s.Coverage <= 0 {
		t.Errorf("mimir query log %+v, want proven", s)
	}
	if s := bySource[sourceLokiQueryLog]; !s.Configured || s.Gap != "" || s.Coverage <= 0 {
		t.Errorf("loki query log %+v", s)
	}
	if s := bySource[sourcePromQueryLog]; !s.Configured || s.Coverage != 0 || !strings.Contains(s.Gap, "no Prometheus URL") {
		t.Errorf("prometheus query log %+v, want an unproven gap", s)
	}

	g := readGraph(t, db)
	var panel, stream bool
	for _, r := range g.refs {
		c := g.consumers[r.ConsumerID]
		if r.SignalID == "mimir:metric:shop_cart_operations_total" && c.Kind == graph.ConsumerDashboardPanel &&
			strings.HasPrefix(c.ID, "qlog:mimir:panel:1:shop-overview:4:") && r.Actor == "admin" && !r.LastSeen.IsZero() {
			panel = true
		}
		if r.SignalID == `loki:stream:{app="shop", component="api"}` && c.Kind == graph.ConsumerAdhocQuery &&
			strings.Contains(c.Expr, `"route=/healthz"`) && !r.LastSeen.IsZero() {
			stream = true
		}
	}
	if !panel {
		t.Error("no reference from Shop Overview panel 4's logged Mimir query")
	}
	if !stream {
		t.Error("no reference from the on-call ad hoc Loki query")
	}
	// The probe marker is never a consumer.
	for _, c := range g.consumers {
		if strings.Contains(c.Expr, "tvt_query_log_probe") {
			t.Errorf("probe marker became consumer %s", c.ID)
		}
	}
	reads, err := st.LastReads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) == 0 {
		t.Error("no last reads stored")
	}
}

// A query log that never shows the marker covers nothing and records the
// gap, but its queries still count.
func TestScanQueryLogUnproven(t *testing.T) {
	setNow(t)
	probes := &atomic.Int32{}
	l := newLab(t, map[string]http.HandlerFunc{"/api/v1/query": probeSink(t, "", probes)}) // answers, never logs
	db := filepath.Join(t.TempDir(), "tvt.db")
	o := labOptions(l, db)
	o.ProbeWait = 300 * time.Millisecond
	var out bytes.Buffer
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "prom log     70 queries, coverage 0% (gap: tvt's probe query never appeared") || probes.Load() != 1 {
		t.Errorf("output:\n%s", out.String())
	}
	g := readGraph(t, db)
	found := false
	for _, r := range g.refs {
		if r.SignalID == "prom:metric:node_memory_MemAvailable_bytes" && g.consumers[r.ConsumerID].Kind == graph.ConsumerAdhocQuery {
			found = true
		}
	}
	if !found {
		t.Error("an unproven log's queries were dropped")
	}
}

// flushingLog is a query log whose backend flushes buffered queries
// together with tvt's probe marker.
type flushingLog struct{ flushed bool }

func (f *flushingLog) Probe(context.Context, string) error { return nil }

func (f *flushingLog) ProbeSeen(context.Context, string) (bool, error) {
	f.flushed = true // the marker and the buffered query land together
	return true, nil
}

func (f *flushingLog) Queries(context.Context, time.Time) ([]collect.LoggedQuery, error) {
	q := []collect.LoggedQuery{{Source: "prometheus", Language: "promql", Expr: "up"}}
	if f.flushed {
		q = append(q, collect.LoggedQuery{Source: "prometheus", Language: "promql", Expr: "rate(shop_orders_total[5m])"})
	}
	return q, nil
}

func (f *flushingLog) Coverage(context.Context, time.Time) (float64, error) {
	if f.flushed {
		return 0.5, nil
	}
	return 0.25, nil
}

// A proven log is read after the probe wait, so queries flushed with the
// marker are part of the scan.
func TestQueryLogReadAfterProbe(t *testing.T) {
	f := &flushingLog{}
	ql := &queryLog{source: sourcePromQueryLog, label: "prom log", reader: f, probe: f}
	var out bytes.Buffer
	src, queries, err := ql.read(context.Background(), time.Now().Add(-time.Hour), "n", time.Second, &out)
	if err != nil {
		t.Fatal(err)
	}
	if src.Gap != "" || src.Coverage != 0.5 || len(queries) != 2 {
		t.Errorf("got gap %q, coverage %v, %d queries; want proven, 0.5, 2 (read after the flush)\n%s", src.Gap, src.Coverage, len(queries), out.String())
	}
}

// assertGrafanaRule: Sprint 2 item 5. The lab's Grafana-managed alert is a
// consumer tagged evaluated_by=grafana with its interval and datasource,
// and the metric only it reads is hard keep through a declared reference.
func assertGrafanaRule(t *testing.T, g scanGraph) {
	t.Helper()
	id := "rule:grafana:cg3qdhms8urr4c/shop-grafana-alerts/shop-order-export-lag"
	c, ok := g.consumers[id]
	if !ok {
		t.Fatalf("no consumer %s", id)
	}
	if c.EvaluatedBy != "grafana" || c.Interval != time.Minute || !slices.Equal(c.Datasources, []string{"prometheus"}) ||
		c.Expr != "max(shop_order_export_lag_seconds)" {
		t.Errorf("Grafana rule consumer %+v", c)
	}
	var found bool
	for _, r := range g.refsTo("prom:metric:shop_order_export_lag_seconds") {
		if r.ConsumerID == id && r.Evidence == graph.EvidenceDeclared {
			found = true
		}
	}
	if !found {
		t.Errorf("shop_order_export_lag_seconds has no declared reference from %s", id)
	}
}

// --config: an external consumer makes the metric it selects a hard keep
// (node_load1 is a seeded drop otherwise), and an owner's tier 0 makes
// its unread signals dormant. A config error fails the scan before any
// backend is contacted.
func TestScanConfig(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "tvt.yaml")
	if err := os.WriteFile(cfg, []byte(`external_consumers:
  - name: federation
    metrics: ["node_load1"]
ownership:
  owners:
    - name: cache
      tier: 0
      metrics: ["shop_cache_.*"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	o := labOptions(l, filepath.Join(dir, "tvt.db"))
	o.Config = cfg
	var out bytes.Buffer
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "; 1 external consumers") {
		t.Errorf("output:\n%s", out.String())
	}
	g := readGraph(t, o.DB)
	if s := g.signals["prom:metric:node_load1"]; s.Class != "hard_keep" {
		t.Errorf("node_load1: class %q, want hard_keep through the external consumer", s.Class)
	}
	if c, ok := g.consumers["ext:federation"]; !ok || c.Kind != graph.ConsumerExternal {
		t.Errorf("external consumer %+v", c)
	}
	var cache int
	for _, s := range g.signals {
		// Unread ones: hits and requests feed the (unread) recording rule.
		if strings.HasPrefix(s.Name, "shop_cache_") && s.Kind == graph.KindMetric && len(g.refsTo(s.ID)) == 0 {
			cache++
			if s.Owner != "cache" || s.Class != "dormant" || s.Dormant != "tier 0 service" {
				t.Errorf("%s: owner %q class %s %q", s.ID, s.Owner, s.Class, s.Dormant)
			}
		}
	}
	if cache == 0 {
		t.Error("no shop_cache_ metric signals")
	}

	// A typo fails before anything is collected or written.
	if err := os.WriteFile(cfg, []byte("external_consumer: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o.DB = filepath.Join(dir, "never.db")
	err := runScan(context.Background(), o, env, &out)
	if err == nil || !strings.Contains(err.Error(), "external_consumer") {
		t.Errorf("err %v", err)
	}
	if _, statErr := os.Stat(o.DB); !os.IsNotExist(statErr) {
		t.Error("a scan with a bad config wrote a database")
	}
}

func TestCheckTokenTransport(t *testing.T) {
	tokens := func(name string) string {
		return map[string]string{"TVT_GRAFANA_TOKEN": "g", "TVT_PROM_TOKEN": "p"}[name]
	}
	tests := []struct {
		name    string
		o       scanOptions
		getenv  func(string) string
		wantErr string
	}{
		{"https with token", scanOptions{Grafana: "https://grafana.example.com"}, tokens, ""},
		{"http loopback with token", scanOptions{Grafana: "http://localhost:3000", Prom: "http://127.0.0.1:9090"}, tokens, ""},
		{"http remote without token", scanOptions{Loki: "http://loki.example.com"}, tokens, ""},
		{"http remote with token", scanOptions{Grafana: "http://grafana.example.com"}, tokens, "--grafana: TVT_GRAFANA_TOKEN"},
		{"http remote mimir uses the prom token", scanOptions{Mimir: "http://mimir.example.com"}, tokens, "--mimir: TVT_PROM_TOKEN"},
		{"override", scanOptions{Grafana: "http://grafana.example.com", AllowInsecureToken: true}, tokens, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.o.checkTokenTransport(tc.getenv)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
