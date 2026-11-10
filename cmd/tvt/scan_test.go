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
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
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
}

func newLab(t *testing.T, promOverride map[string]http.HandlerFunc) labServers {
	t.Helper()
	if promOverride == nil {
		promOverride = map[string]http.HandlerFunc{}
	}
	if _, ok := promOverride["/api/v1/series"]; !ok {
		promOverride["/api/v1/series"] = syntheticSeries
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
	loki := fixtureServer(t, map[string]string{"/loki/api/v1/rules": "loki/rules.yaml"}, nil)
	return labServers{prom, graf, loki}
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
		PromQueryLogs: stringList{filepath.Join(lab, "prometheus/query.log")},
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
	for _, want := range []string{"prom ", "5 rules", "loki         1 rule (", "2 dashboards", "as sa-1-tvt", "grafana log", "scan 1 written", "19 dashboard loads (19 in a browser)", ", 19 dashboard loads,"} {
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
	for _, m := range []string{"shop_http_requests_total", "shop_payment_queue_depth", "shop_legacy_queue_depth", "shop_cart_operations_total"} {
		if _, ok := ids["prom:metric:"+m]; !ok {
			t.Errorf("ground-truth metric %s is not a signal", m)
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

	// Rules are consumers; Shop Overview is viewed, Legacy Queue isn't.
	cons, err := st.Consumers(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rules, shop, legacy int
	for _, c := range cons {
		switch {
		case c.Kind == graph.ConsumerAlertRule || c.Kind == graph.ConsumerRecordingRule:
			rules++
		case strings.HasPrefix(c.ID, "panel:1:shop-overview:") || strings.HasPrefix(c.ID, "var:1:shop-overview:"):
			shop++
			if c.LastViewed.IsZero() {
				t.Errorf("Shop Overview consumer %s has no last_viewed", c.ID)
			}
		case strings.HasPrefix(c.ID, "panel:1:legacy-queue:"):
			legacy++
			if !c.LastViewed.IsZero() {
				t.Errorf("Legacy Queue consumer %s viewed at %v; it must stay unviewed", c.ID, c.LastViewed)
			}
		}
	}
	if rules != 6 || shop == 0 || legacy == 0 {
		t.Errorf("rules=%d shop=%d legacy=%d", rules, shop, legacy)
	}

	// The Loki rule is a consumer, and the stream it alerts on is a hard
	// keep: an alert rule references it. The reference is assumed until the
	// LogQL extractor exists, so it covers all the stream's labels.
	byID := map[string]graph.Consumer{}
	for _, c := range cons {
		byID[c.ID] = c
	}
	const lokiAlert = "rule:loki:shop-audit.yml/shop-audit/ShopAuditDenials"
	if c, ok := byID[lokiAlert]; !ok || c.Kind != graph.ConsumerAlertRule {
		t.Errorf("ShopAuditDenials consumer = %+v, %v", c, ok)
	}
	const auditStream = `loki:stream:{app="shop", component="audit"}`
	if s, ok := ids[auditStream]; !ok || s.Kind != graph.KindStream || s.Source != "loki" {
		t.Errorf("audit stream signal = %+v, %v", s, ok)
	}
	refs, err := st.References(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	hardKeep := false
	for _, r := range refs {
		if r.SignalID == auditStream && byID[r.ConsumerID].Kind.HardKeep() {
			hardKeep = true
			if !r.Assumed {
				t.Errorf("reference %+v is not marked assumed", r)
			}
		}
	}
	if !hardKeep {
		t.Errorf("%s is not hard-keep; refs = %+v", auditStream, refs)
	}

	// tvt's own fetches (sa-1-tvt) are not loads.
	loads, err := st.Loads(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loads) == 0 {
		t.Error("no dashboard loads")
	}
	for _, ld := range loads {
		if ld.Login == "sa-1-tvt" || ld.DashboardUID == "legacy-queue" {
			t.Errorf("load %+v should have been excluded", ld)
		}
	}

	// Both log sources are configured, and each covers part of the window.
	srcs, err := st.Sources(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 {
		t.Fatalf("sources = %+v", srcs)
	}
	for _, s := range srcs {
		if !s.Configured || s.Coverage <= 0 || s.Coverage >= 1 {
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

	cons, err := st.Consumers(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]graph.Consumer{}
	var rules int
	for _, c := range cons {
		byID[c.ID] = c
		if c.Kind == graph.ConsumerAlertRule || c.Kind == graph.ConsumerRecordingRule {
			rules++
		}
	}
	for id, kind := range map[string]graph.ConsumerKind{
		"rule:mimir:tvt-lab/shop-recording/NodeLoadHigh":                                    graph.ConsumerAlertRule,
		"rule:mimir:tvt-lab/shop-recording/route:shop_http_request_duration_seconds:p99_5m": graph.ConsumerRecordingRule,
		"rule:mimir:tvt-lab/shop-recording/job:shop_cache_hit_ratio:5m":                     graph.ConsumerRecordingRule,
		"rule:loki:shop-audit.yml/shop-audit/ShopAuditDenials":                              graph.ConsumerAlertRule,
	} {
		if byID[id].Kind != kind {
			t.Errorf("consumer %s = %+v, want kind %s", id, byID[id], kind)
		}
	}
	if rules != 4 {
		t.Errorf("%d rule consumers, want 3 Mimir + 1 Loki", rules)
	}
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
