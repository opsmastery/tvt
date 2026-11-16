package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/loki"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
)

var lab = testutil.Lab

// scanTime is just after the last line of testdata/lab/grafana/grafana.log.
var scanTime = time.Date(2026, 11, 15, 2, 10, 0, 0, time.UTC) // just after the Nov 15 capture

// fixtureServer serves files from testdata/lab by URL path through the
// shared read-only server. A handler in override takes precedence.
func fixtureServer(t *testing.T, files map[string]string, override map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	return fixtureServerAt(t, lab, files, override)
}

// fixtureServerAt is fixtureServer for the lab snapshot in dir.
func fixtureServerAt(t *testing.T, dir string, files map[string]string, override map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	routes := testutil.Routes{}
	for path, name := range files {
		routes[path] = testutil.File(t, filepath.Join(dir, name))
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
	// lokiLog is the same for Loki's recorded query log (empty when the
	// snapshot has none); lokiProbes counts Loki markers (S5 item 5).
	lokiLog    string
	lokiProbes *atomic.Int32
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
				"params":      map[string]string{"query": q}, "time": scanNow().Format(time.RFC3339Nano),
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

// lokiProbeSink answers tvt's Loki probe like Loki does and, when log is
// set, writes the frontend metrics.go line Loki logs for it.
func lokiProbeSink(t *testing.T, log string, n *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if _, ok := collect.LokiProbeNonce(q); !ok {
			t.Errorf("Loki query endpoint got %q, not a probe marker", q)
		}
		n.Add(1)
		if log != "" {
			f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(f, "level=info ts=%s caller=metrics.go:237 component=frontend org_id=fake latency=fast query=%q query_type=metric range_type=instant status=200 source=%s\n",
				scanNow().Format(time.RFC3339Nano), q, strings.TrimPrefix(r.Header.Get("X-Query-Tags"), "Source="))
			f.Close()
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}
}

// copyLog copies dir/rel into a temp file the fixture backends can append
// to; "" when the snapshot has no such log.
func copyLog(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), filepath.Base(rel))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newLab(t *testing.T, promOverride map[string]http.HandlerFunc) labServers {
	t.Helper()
	return newLabAt(t, lab, promOverride)
}

// newLabAt serves the lab snapshot in dir (laid out like testdata/lab).
// Override keys are Prometheus paths, or "loki:" plus a Loki path.
func newLabAt(t *testing.T, dir string, override map[string]http.HandlerFunc) labServers {
	t.Helper()
	promOverride, lokiOverride := map[string]http.HandlerFunc{}, map[string]http.HandlerFunc{}
	for k, h := range override {
		if path, ok := strings.CutPrefix(k, "loki:"); ok {
			lokiOverride[path] = h
		} else {
			promOverride[k] = h
		}
	}
	if _, ok := promOverride["/api/v1/series"]; !ok {
		promOverride["/api/v1/series"] = syntheticSeries
	}
	promLog := copyLog(t, dir, "prometheus/query.log")
	if promLog == "" {
		t.Fatalf("%s has no prometheus/query.log", dir)
	}
	probes := &atomic.Int32{}
	if _, ok := promOverride["/api/v1/query"]; !ok {
		promOverride["/api/v1/query"] = probeSink(t, promLog, probes)
	}
	for path, h := range labelMetricsRoutes(t, dir) {
		if _, ok := promOverride[path]; !ok {
			promOverride[path] = h
		}
	}
	for path, h := range overflowRoutes(t, dir, "", promOverride["/api/v1/series"]) {
		promOverride[path] = h
	}
	prom := fixtureServerAt(t, dir, map[string]string{
		"/api/v1/status/tsdb":      "prometheus/tsdb.json",
		"/api/v1/rules":            "prometheus/rules.json",
		"/api/v1/targets":          "prometheus/targets.json",
		"/api/v1/targets/metadata": "prometheus/targets_metadata.json",
		"/api/v1/status/flags":     "prometheus/flags.json",
		"/metrics":                 "prometheus/metrics.txt",
	}, promOverride)
	grafFiles := map[string]string{
		"/api/search":                     "grafana/search.json",
		"/api/datasources":                "grafana/datasources.json",
		"/api/ruler/grafana/api/v1/rules": "grafana/ruler_config.json",
	}
	for uid, file := range testutil.DashboardsIn(t, dir) {
		grafFiles["/api/dashboards/uid/"+uid] = "grafana/" + file
	}
	graf := fixtureServerAt(t, dir, grafFiles, map[string]http.HandlerFunc{
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
	lokiLog, lokiProbes := copyLog(t, dir, "loki/query.log"), &atomic.Int32{}
	for path, h := range patternsRoute(t, dir) {
		if _, ok := lokiOverride[path]; !ok {
			lokiOverride[path] = h
		}
	}
	if _, ok := lokiOverride["/loki/api/v1/query"]; !ok {
		lokiOverride["/loki/api/v1/query"] = lokiProbeSink(t, lokiLog, lokiProbes)
	}
	loki := fixtureServerAt(t, dir, map[string]string{
		"/loki/api/v1/rules":        "loki/rules.yaml",
		"/prometheus/api/v1/rules":  "loki/prom_rules.json",
		"/loki/api/v1/labels":       "loki/labels.json",
		"/loki/api/v1/index/volume": "loki/volume_by_stream.json",
	}, lokiOverride)
	return labServers{prom, graf, loki, promLog, probes, lokiLog, lokiProbes}
}

// labelMetricsRoutes answer /api/v1/label/<l>/values and
// /api/v1/label/__name__/values?match[]={<l>="<v>"} from the snapshot's
// prometheus/label_metrics.json (S6 item 4), as Prometheus would. A
// snapshot without it has no routes: the label lookup gets 404s.
func labelMetricsRoutes(t *testing.T, dir string) map[string]http.HandlerFunc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "prometheus/label_metrics.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var recorded map[string]map[string][]string
	if err := json.Unmarshal(b, &recorded); err != nil {
		t.Fatal(err)
	}
	reply := func(w http.ResponseWriter, data []string) {
		if data == nil {
			data = []string{}
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
	}
	routes := map[string]http.HandlerFunc{}
	for l, byValue := range recorded {
		values := slices.Sorted(maps.Keys(byValue))
		routes["/api/v1/label/"+l+"/values"] = func(w http.ResponseWriter, r *http.Request) { reply(w, values) }
	}
	matchRe := regexp.MustCompile(`^\{([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"\}$`)
	routes["/api/v1/label/__name__/values"] = func(w http.ResponseWriter, r *http.Request) {
		m := matchRe.FindStringSubmatch(r.URL.Query().Get("match[]"))
		if m == nil {
			t.Errorf("label values match[] %q: not one equality matcher", r.URL.Query().Get("match[]"))
			http.NotFound(w, r)
			return
		}
		v, err := strconv.Unquote(`"` + m[2] + `"`)
		if err != nil {
			t.Error(err)
		}
		reply(w, recorded[m[1]][v])
	}
	return routes
}

// overflowRoutes answer the overflow reader (S6 item 5) from the snapshot's
// prometheus/overflow.json ({"series": [...], "labels": {metric: [names]}},
// recorded by capture-fixtures.sh): /api/v1/series for an
// otel.metric.overflow match, and /api/v1/labels for a metric's names. A
// snapshot without the file has no overflow series. Other series requests
// go to next (nil: none expected). prefix is "/prometheus" for Mimir.
func overflowRoutes(t *testing.T, dir, prefix string, next http.HandlerFunc) map[string]http.HandlerFunc {
	t.Helper()
	var rec struct {
		Series []map[string]string `json:"series"`
		Labels map[string][]string `json:"labels"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "prometheus/overflow.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		t.Fatal(err)
	default:
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatal(err)
		}
	}
	reply := func(w http.ResponseWriter, data any) {
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
	}
	nameRe := regexp.MustCompile(`^\{__name__="([^"]+)"\}$`)
	return map[string]http.HandlerFunc{
		prefix + "/api/v1/series": func(w http.ResponseWriter, r *http.Request) {
			m := r.URL.Query().Get("match[]")
			switch {
			case m == `{otel_metric_overflow="true"}`:
				reply(w, cmpNil(rec.Series))
			case m == `{"otel.metric.overflow"="true"}`:
				reply(w, []map[string]string{}) // recorded with dots translated
			case next != nil:
				next(w, r)
			default:
				t.Errorf("unexpected series match[] %q", m)
				http.NotFound(w, r)
			}
		},
		prefix + "/api/v1/labels": func(w http.ResponseWriter, r *http.Request) {
			m := nameRe.FindStringSubmatch(r.URL.Query().Get("match[]"))
			if m == nil {
				t.Errorf("labels match[] %q: not one metric name", r.URL.Query().Get("match[]"))
				http.NotFound(w, r)
				return
			}
			reply(w, cmpNil(rec.Labels[m[1]]))
		},
	}
}

func cmpNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// patternsRoute serves the snapshot's recorded pattern API answers
// (loki/patterns.json, one per stream selector, S5 items 3-4) by the
// query's selector. A snapshot without them has no route, so the scan sees
// a Loki without the pattern ingester (404).
func patternsRoute(t *testing.T, dir string) map[string]http.HandlerFunc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "loki/patterns.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var recorded map[string]json.RawMessage
	if err := json.Unmarshal(b, &recorded); err != nil {
		t.Fatal(err)
	}
	return map[string]http.HandlerFunc{"/loki/api/v1/patterns": func(w http.ResponseWriter, r *http.Request) {
		body, ok := recorded[plainSelector(t, r.URL.Query().Get("query"))]
		if !ok {
			t.Errorf("patterns query %q: no recorded answer", r.URL.Query().Get("query"))
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}}
}

// plainSelector drops the label="" matchers tvt adds to ask Loki for
// exactly one stream (loki.ExactStreamSelector), giving the recorded key.
func plainSelector(t *testing.T, sel string) string {
	t.Helper()
	refs, err := parse.LogQL(sel)
	if err != nil || len(refs) != 1 {
		t.Errorf("patterns query %q: %v", sel, err)
		return sel
	}
	var ms []parse.Matcher
	for _, m := range refs[0].Matchers {
		if m.Op != parse.MatchEqual || m.Value != "" {
			ms = append(ms, m)
		}
	}
	return parse.CanonicalSelector(ms)
}

func labOptions(l labServers, db string) scanOptions {
	return labOptionsAt(lab, l, db)
}

// labOptionsAt is labOptions for the lab snapshot in dir.
func labOptionsAt(dir string, l labServers, db string) scanOptions {
	return scanOptions{
		Grafana:       l.grafana.URL,
		Prom:          l.prom.URL,
		Loki:          l.loki.URL,
		DB:            db,
		Window:        30,
		LabelTop:      2,
		GrafanaLogs:   stringList{filepath.Join(dir, "grafana/grafana.log")},
		PromQueryLogs: stringList{l.promLog},
		ProbeWait:     2 * time.Second,
		// The lab's SLO definitions and runbooks (Sprint 2 item 7).
		Config: filepath.Join(dir, "tvt-lab.yaml"),
	}
}

func env(name string) string {
	return map[string]string{"TVT_GRAFANA_TOKEN": "test-grafana-token"}[name]
}

func setNow(t *testing.T) {
	t.Helper()
	setNowAt(t, scanTime)
}

// setNowAt fixes the scan clock at now for the test.
func setNowAt(t *testing.T, now time.Time) {
	t.Helper()
	old := scanNow
	scanNow = func() time.Time { return now }
	t.Cleanup(func() { scanNow = old })
}

// The self-hosted card prices the same lab by disk: every metric gets bytes
// over the lab's measured 45d retention at its measured bytes per sample.
func TestScanLabSelfHosted(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	db := filepath.Join(t.TempDir(), "tvt.db")
	o := labOptions(l, db)
	o.Config = filepath.Join(lab, "tvt-lab-selfhosted.yaml")
	var out bytes.Buffer
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	t.Log("\n" + out.String())
	for _, want := range []string{"pricecard    lab-self-hosted (USD, self-hosted)", "disk model   0.62 bytes/sample (measured", "1080h0m0s retention (measured"} {
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
	if info.PriceKind != cost.KindSelfHosted || info.PricePreset || info.SelfHosted == nil ||
		info.SelfHosted.Retention != 45*24*time.Hour || info.SelfHosted.HeadBytesPerSeries <= 0 {
		t.Fatalf("scan = %+v %+v", info, info.SelfHosted)
	}
	costs, err := st.Costs(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var metrics int
	var disk, total float64
	for _, c := range costs {
		if !strings.HasPrefix(c.SignalID, "prom:metric:") || c.ActiveSeries == 0 {
			continue
		}
		metrics++
		disk += float64(c.DiskBytes)
		total += c.MonthlyCost
		if c.DiskBytes <= 0 || math.Abs(c.MonthlyCost-float64(c.DiskBytes)/1e9*0.08) > 1e-6 {
			t.Errorf("%s: disk %d monthly %v", c.SignalID, c.DiskBytes, c.MonthlyCost)
		}
	}
	if metrics == 0 {
		t.Fatal("no metric costs")
	}
	t.Logf("%d metrics hold %.1f GB (2 replicas), %.2f USD/month", metrics, disk/1e9, total)
}

// What-if pricing (S3 item 4): the stored grafana-cloud scan repriced at
// the self-hosted card gives what scanning with that card gives, signal by
// signal, and repricing back gives the original. No rescan, no backend.
func TestRepriceMatchesRescan(t *testing.T) {
	setNow(t)
	scanAt := func(config string) (store.ScanInfo, []graph.Cost) {
		l := newLab(t, nil)
		db := filepath.Join(t.TempDir(), "tvt.db")
		o := labOptions(l, db)
		o.Config = filepath.Join(lab, config)
		var out bytes.Buffer
		if err := runScan(context.Background(), o, env, &out); err != nil {
			t.Fatalf("scan %s: %v\n%s", config, err, out.String())
		}
		st, err := store.Open(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		info, err := st.LatestScan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		costs, err := st.Costs(context.Background(), info.ID)
		if err != nil {
			t.Fatal(err)
		}
		return info, costs
	}
	vendorInfo, vendorCosts := scanAt("tvt-lab.yaml")
	_, selfCosts := scanAt("tvt-lab-selfhosted.yaml")
	if vendorInfo.TSDB == nil || vendorInfo.TSDB.BytesPerSample == 0 {
		t.Fatalf("vendor scan stored no TSDB measurements: %+v", vendorInfo)
	}
	selfCard, err := cost.Load(filepath.Join(lab, "pricecard-selfhosted.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	vendorCard, err := cost.Load("grafana-cloud")
	if err != nil {
		t.Fatal(err)
	}
	same := func(name string, got, want []graph.Cost) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d costs, want %d", name, len(got), len(want))
		}
		var total float64
		for i := range got {
			g, w := got[i], want[i]
			total += g.MonthlyCost
			if g.SignalID != w.SignalID || g.DiskBytes != w.DiskBytes || math.Abs(g.MonthlyCost-w.MonthlyCost) > 1e-9 {
				t.Errorf("%s %s: disk %d monthly %v, want %d %v", name, g.SignalID, g.DiskBytes, g.MonthlyCost, w.DiskBytes, w.MonthlyCost)
			}
		}
		t.Logf("%s: %d costs, %.2f/month", name, len(got), total)
	}
	repriced, _, err := cost.Reprice(selfCard, vendorCosts, vendorInfo.TSDB)
	if err != nil {
		t.Fatal(err)
	}
	same("grafana-cloud → self-hosted", repriced, selfCosts)
	back, _, err := cost.Reprice(vendorCard, repriced, vendorInfo.TSDB)
	if err != nil {
		t.Fatal(err)
	}
	same("→ back to grafana-cloud", back, vendorCosts)
}

// S3 items 5 and 6 on real lab traffic: query_agents.log is a window of
// the lab Prometheus's query log captured Nov 14 (Grafana's panel queries
// and Grafana-managed rule evaluations, the sweep agent, the finance
// agent), appended to the Nov 15 capture (which has a few agent queries of
// its own) so both fall in the window. The sweep
// keeps nothing: every ground-truth drop stays a drop. The finance agent's
// named read counts, and the rule's evaluations join to it.
func TestScanLabAgents(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	agents, err := os.ReadFile(filepath.Join(lab, "prometheus/query_agents.log"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(l.promLog, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(agents); err != nil {
		t.Fatal(err)
	}
	f.Close()
	db := filepath.Join(t.TempDir(), "tvt.db")
	var out bytes.Buffer
	if err := runScan(context.Background(), labOptions(l, db), env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	g := readGraph(t, db)
	gt := loadGroundTruth(t)
	assertMetrics(t, g, "prom", gt.Metrics)
	assertReferences(t, g, "prom", gt)
	assertClasses(t, g, "prom", gt)

	var sweeps, sweepConsumers int
	swept := map[string]bool{}
	for _, r := range g.refs {
		c := g.consumers[r.ConsumerID]
		if strings.Contains(c.Expr, `{__name__=~".+"}`) {
			swept[r.ConsumerID] = true
			if !r.Sweep {
				t.Errorf("sweep agent's reference to %s isn't a sweep", r.SignalID)
			}
			sweeps++
		}
	}
	sweepConsumers = len(swept)
	if sweepConsumers != 1 || sweeps < 1000 {
		t.Errorf("sweep: %d consumers, %d references", sweepConsumers, sweeps)
	}
	var finance, rule bool
	for _, r := range g.refsTo("prom:metric:shop_revenue_dollars_total") {
		if c := g.consumers[r.ConsumerID]; c.Kind == graph.ConsumerAdhocQuery && !r.LastSeen.IsZero() {
			finance = true
		}
	}
	for _, c := range g.consumers {
		if c.Client == "rule" && strings.HasPrefix(c.Name, "Shop order export lag") {
			rule = true
		}
	}
	if !finance {
		t.Error("the finance agent's read of shop_revenue_dollars_total isn't a read")
	}

	// The sweep keeps nothing on its own: a candidate only the sweep
	// touched weighs as a sweep, below a stale dashboard, and ranks by
	// its cost.
	read, sweptBy := map[string]bool{}, map[string]bool{}
	for _, r := range g.refs {
		if r.Sweep {
			sweptBy[r.SignalID] = true
		} else {
			read[r.SignalID] = true
		}
	}
	scores := assertScores(t, db)
	var sweptOnly int
	for id, r := range scores {
		if read[id] || !sweptBy[id] {
			continue
		}
		sweptOnly++
		if r.Strongest != "sweep" || r.UsageWeight > score.DefaultWeights.Sweep || r.UsageWeight >= score.DefaultWeights.StaleDashboard {
			t.Errorf("%s, touched only by the sweep, scores %+v", id, r)
		}
	}
	if sweptOnly == 0 {
		t.Error("no candidate is touched only by the sweep")
	}
	if r, ok := scores["prom:metric:shop_revenue_dollars_total"]; ok && r.Strongest != "adhoc_query" {
		t.Errorf("the finance read weighs as %+v", r)
	}
	if !rule {
		t.Error("the Grafana-managed rule's evaluations don't join to Shop order export lag")
	}
	for _, s := range g.signals {
		if s.ID == "prom:metric:shop_revenue_dollars_total" && s.Class != "candidate" {
			t.Errorf("shop_revenue_dollars_total: class %s, want candidate (read by an agent only)", s.Class)
		}
	}
	t.Logf("sweep: %d references, all weak", sweeps)
}

// noOwnersGap is the slo definitions gap when tvt.yaml names no owners
// (S6 item 3); the lab configs name none.
const noOwnersGap = "no ownership.owners in tvt.yaml: services without an SLO can't be listed"

func TestScanLab(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	db := filepath.Join(t.TempDir(), "tvt.db")
	var out bytes.Buffer
	if err := runScan(context.Background(), labOptions(l, db), env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	t.Log("\n" + out.String())
	for _, want := range []string{"prom ", "6 rules", "loki         1 rule (5 streams, 2.0 GB/day)", "3 dashboards", "as sa-1-tvt", "grafana log", "scan 1 written", "56 dashboard loads (48 in a browser)", ", 56 dashboard loads,", " references, ", "pricecard    grafana-cloud-pro (USD, a vendor preset: approximate"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q", want)
		}
	}
	assertCosts(t, db, "prom", "grafana-cloud-pro", false)
	assertStreamCosts(t, db)
	assertLoggedClients(t, db)
	for id, r := range assertScores(t, db) {
		if r.Held != "" {
			t.Errorf("%s held (%s) in a 30-day window", id, r.Held)
		}
	}
	if !strings.Contains(out.String(), "candidates ranked; top ") {
		t.Errorf("output has no scores line")
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
		if (c.Kind == graph.ConsumerAlertRule || c.Kind == graph.ConsumerRecordingRule) && !strings.HasPrefix(c.ID, "qlog:") {
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
	assertDeclared(t, g, "prom", lab, gt)
	// Sprint 2 item 18: the lab's one absence alert is the only consumer S6
	// must skip when it looks for rules that return nothing.
	var absence []string
	for _, c := range g.consumers {
		if parse.UsesAbsence(c.Expr) {
			absence = append(absence, c.Name)
		}
	}
	if len(absence) != 1 || absence[0] != "ShopInventorySyncMissing" {
		t.Errorf("consumers using absent(): %v, want [ShopInventorySyncMissing]", absence)
	}
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
	// recorded as not configured. SLO definitions and runbooks are read in
	// full: the Sloth SLO and the ShopPaymentQueueBacklog runbook. So are
	// the datasources of every dashboard and Grafana rule query. The lab's
	// Prometheus has a retention.size, so its storage has no warning. The
	// snapshot predates the overflow seed: overflow series were looked for
	// and none found (S6 item 5).
	srcs, err := st.Sources(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 8 || l.probes.Load() != 1 {
		t.Fatalf("sources = %+v, probes %d", srcs, l.probes.Load())
	}
	for _, s := range srcs {
		// Every lab dashboard and Grafana rule query is Prometheus or Loki.
		if s.Source == scan.SourceSLOs || s.Source == scan.SourceRunbooks || s.Source == scan.SourceDatasources || s.Source == scan.SourceStorage || s.Source == scan.SourceOverflow {
			// The lab config names no owners, so SLO coverage can't list
			// services without an SLO (S6 item 3).
			gap := s.Gap
			if s.Source == scan.SourceSLOs {
				gap = strings.TrimPrefix(gap, noOwnersGap)
			}
			if !s.Configured || s.Coverage != 1 || gap != "" {
				t.Errorf("source %+v", s)
			}
			continue
		}
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
	mimirRoutes := overflowRoutes(t, lab, "/prometheus", nil)
	mimirRoutes[testutil.MimirLabelValuesPath] = names.ServeHTTP
	mimirSrv := fixtureServer(t, map[string]string{"/prometheus/api/v1/rules": "mimir/rules.json"}, mimirRoutes)

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
	for _, want := range []string{"mimir        1548 metrics", "3 rules", "loki         1 rule (", "scan 1 written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q", want)
		}
	}
	assertCosts(t, db, "mimir", "grafana-cloud-pro", true)

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
		if (c.Kind == graph.ConsumerAlertRule || c.Kind == graph.ConsumerRecordingRule) && !strings.HasPrefix(c.ID, "qlog:") {
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
	// The Mimir log (Nov 11-12, kept: its Grafana panel queries were sent by
	// hand) and the Loki log (Nov 15) both fall in the window to scanTime.
	setNow(t)
	oldNonce := newProbeNonce
	newProbeNonce = func() string { return "0f1e2d3c4b5a6978" }
	t.Cleanup(func() { newProbeNonce = oldNonce })

	l := newLab(t, nil)
	top, err := os.ReadFile(filepath.Join(lab, "mimir/label_values_name.json"))
	if err != nil {
		t.Fatal(err)
	}
	names := &testutil.MimirNames{T: t, Names: testutil.LoadMimirNames(t, filepath.Join(lab, "mimir/label_values_name_by_prefix.json")), Top: top}
	mimirProbes := &atomic.Int32{}
	mimirRoutes := overflowRoutes(t, lab, "/prometheus", nil)
	mimirRoutes[testutil.MimirLabelValuesPath] = names.ServeHTTP
	mimirRoutes["/prometheus/api/v1/query"] = probeSink(t, "", mimirProbes)
	mimirSrv := fixtureServer(t, map[string]string{"/prometheus/api/v1/rules": "mimir/rules.json"}, mimirRoutes)

	db := filepath.Join(t.TempDir(), "tvt.db")
	o := labOptions(l, db)
	o.Prom, o.Mimir, o.LabelTop = "", mimirSrv.URL, 0
	o.MimirQueryLogs = stringList{filepath.Join(lab, "mimir/query.log")}
	o.LokiQueryLogs = stringList{l.lokiLog}
	var out bytes.Buffer
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	t.Log("\n" + out.String())
	if mimirProbes.Load() != 1 || l.probes.Load() != 0 || l.lokiProbes.Load() != 1 {
		t.Errorf("probes: mimir %d, prometheus %d, loki %d; want 1, 0 and 1", mimirProbes.Load(), l.probes.Load(), l.lokiProbes.Load())
	}
	if !strings.Contains(out.String(), "loki log") || !strings.Contains(out.String(), "(probe found)") {
		t.Errorf("output lacks the Loki probe:\n%s", out.String())
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
		t.Errorf("loki query log %+v, want proven", s)
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
		if strings.HasPrefix(r.SignalID, `loki:stream:{app="shop", component="api"`) && c.Kind == graph.ConsumerAdhocQuery &&
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
	if !strings.Contains(out.String(), "prom log     97 queries, coverage 0% (gap: tvt's probe query never appeared") || probes.Load() != 1 {
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

// Loki's log (S5 item 5): a Loki that answers the marker but never logs it
// leaves its query log unproven, coverage 0, with its queries still read;
// a failed probe request is a gap naming the failure.
func TestScanLokiQueryLogUnproven(t *testing.T) {
	setNow(t)
	for name, sink := range map[string]http.HandlerFunc{
		"never logged": lokiProbeSink(t, "", &atomic.Int32{}),
		"probe fails":  func(w http.ResponseWriter, r *http.Request) { http.Error(w, "query not allowed", http.StatusForbidden) },
	} {
		t.Run(name, func(t *testing.T) {
			l := newLab(t, map[string]http.HandlerFunc{"loki:/loki/api/v1/query": sink})
			db := filepath.Join(t.TempDir(), "tvt.db")
			o := labOptions(l, db)
			o.LokiQueryLogs = stringList{l.lokiLog}
			o.ProbeWait = 300 * time.Millisecond
			var out bytes.Buffer
			if err := runScan(context.Background(), o, env, &out); err != nil {
				t.Fatalf("scan: %v\n%s", err, out.String())
			}
			want := "tvt's probe query never appeared"
			if name == "probe fails" {
				want = "probe query failed: loki: /loki/api/v1/query: HTTP 403"
			}
			var line string
			for _, ln := range strings.Split(out.String(), "\n") {
				if strings.HasPrefix(ln, "loki log") {
					line = ln
				}
			}
			if !strings.Contains(line, "coverage 0%") || !strings.Contains(line, want) {
				t.Errorf("loki log line %q, want coverage 0%% and %q", line, want)
			}
			g := readGraph(t, db)
			found := false
			for _, r := range g.refs {
				if strings.Contains(g.consumers[r.ConsumerID].Expr, `"route=/healthz"`) {
					found = true
				}
			}
			if !found {
				t.Error("an unproven Loki log's queries were dropped")
			}
		})
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

// A window shorter than window_days recommends nothing: every candidate is
// scored with confidence 0 and the scan says why. A second scan reads the
// first's history: signals it already saw keep an unknown age.
func TestScanScoresWindowAndHistory(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	db := filepath.Join(t.TempDir(), "tvt.db")
	o := labOptions(l, db)
	o.Window = 14
	var out bytes.Buffer
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "none recommendable: the window is shorter than window_days (30)") {
		t.Errorf("output doesn't say the window is too short:\n%s", out.String())
	}
	for id, r := range assertScores(t, db) {
		if r.Confidence != 0 || r.Held != score.HeldWindow {
			t.Errorf("%s in a 14-day window: %+v", id, r)
		}
	}

	o.Window = 30
	out.Reset()
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("second scan: %v\n%s", err, out.String())
	}
	for id, r := range assertScores(t, db) {
		if r.Held != "" {
			t.Errorf("%s held (%s) on the second scan", id, r.Held)
		}
	}
}

// A dashboard view from an earlier scan protects after the Grafana log
// line has rotated away: rescanning with an empty router log keeps every
// keep_high signal keep_high.
func TestScanKeepsViewsAfterLogRotation(t *testing.T) {
	setNow(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "tvt.db")
	var out bytes.Buffer
	if err := runScan(context.Background(), labOptions(newLab(t, nil), db), env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	before := readGraph(t, db)
	empty := filepath.Join(dir, "grafana.log")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	o := labOptions(newLab(t, nil), db)
	o.GrafanaLogs = stringList{empty}
	out.Reset()
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("rescan: %v\n%s", err, out.String())
	}
	after := readGraph(t, db)
	var kept int
	for id, s := range before.signals {
		if s.Class != string(score.ClassKeepHigh) {
			continue
		}
		kept++
		if c := after.signals[id].Class; c != string(score.ClassKeepHigh) {
			t.Errorf("%s: keep_high before the log rotated, %s after", id, c)
		}
	}
	if kept == 0 {
		t.Fatal("the lab scan has no keep_high signal")
	}
}

// Storage hygiene (S5 item 6) on the lab: its Prometheus caps retention at
// 20 GiB, which fits an unknown volume quietly; on a declared 22 GiB
// volume (91%) it's a warning, stored as a gap and listed in the report's
// header, never on a row or as a saving.
func TestScanStorageHygiene(t *testing.T) {
	setNow(t)
	for volume, want := range map[string]string{
		"":     "retention.size 20 GiB (volume size unknown: storage.volume_size in tvt.yaml)",
		"50Gi": "retention.size 20 GiB, 40% of the 50 GiB volume",
		"22Gi": "retention.size 20 GiB, 91% of the 22 GiB volume (warning)",
	} {
		t.Run("volume "+volume, func(t *testing.T) {
			l := newLab(t, nil)
			dir := t.TempDir()
			db := filepath.Join(dir, "tvt.db")
			o := labOptions(l, db)
			o.Config = filepath.Join(dir, "tvt.yaml")
			cfg := ""
			if volume != "" {
				cfg = "storage:\n  volume_size: " + volume + "\n"
			}
			if err := os.WriteFile(o.Config, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := runScan(context.Background(), o, env, &out); err != nil {
				t.Fatalf("scan: %v\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), "storage      "+want+"\n") {
				t.Errorf("output lacks %q:\n%s", want, out.String())
			}
			in, _, err := loadReport(context.Background(), db)
			if err != nil {
				t.Fatal(err)
			}
			in.Now = scanTime
			r := report.Build(in, 0)
			var gaps []string
			for _, g := range r.Gaps {
				if g.Kind == report.GapStorage {
					gaps = append(gaps, g.Text)
				}
			}
			if volume == "22Gi" {
				if len(gaps) != 1 || !strings.Contains(gaps[0], "prometheus storage: retention.size 20 GiB is 91% of the 22 GiB volume") {
					t.Errorf("storage gaps %q", gaps)
				}
			} else if len(gaps) != 0 {
				t.Errorf("storage gaps %q, want none", gaps)
			}
			for _, rec := range r.Recommendations {
				for _, g := range rec.Evidence.Gaps {
					if strings.Contains(g, "retention.size") {
						t.Errorf("%s lists the storage warning", rec.ID)
					}
				}
			}
		})
	}
}

// TestScanSelectedMonitors (S5 item 8) points tvt.yaml at the pinned
// kube-prometheus-stack render plus the extra monitors: the monitors line
// names the two the chart's Prometheus doesn't select, the report lists
// them as header gaps only, and a scrape pool naming one of them (served
// in the targets API) is reported, never attributed to it.
func TestScanSelectedMonitors(t *testing.T) {
	setNow(t)
	targets := func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile(filepath.Join(lab, "prometheus", "targets.json"))
		if err != nil {
			t.Error(err)
			return
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Error(err)
			return
		}
		data := doc["data"].(map[string]any)
		active := data["activeTargets"].([]any)
		for _, a := range active {
			tg := a.(map[string]any)
			if tg["scrapePool"] == "shop" {
				extra := maps.Clone(tg)
				extra["scrapePool"] = "serviceMonitor/shop/shop/0"
				active = append(active, extra)
				break
			}
		}
		data["activeTargets"] = active
		_ = json.NewEncoder(w).Encode(doc)
	}
	l := newLab(t, map[string]http.HandlerFunc{"/api/v1/targets": targets})
	dir := t.TempDir()
	db := filepath.Join(dir, "tvt.db")
	o := labOptions(l, db)
	o.Config = filepath.Join(dir, "tvt.yaml")
	kps, err := filepath.Abs(filepath.Join("..", "..", "testdata", "kube-prometheus-stack"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("kubernetes:\n  namespace: monitoring\n  manifests:\n    - %s\n    - %s\n",
		filepath.Join(kps, "render.yaml"), filepath.Join(kps, "extra-monitors.yaml"))
	if err := os.WriteFile(o.Config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	const want = "monitors     15 of 17 monitors selected by 1 server, not selected: batch/legacy-jobs, shop/shop\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}
	in, _, err := loadReport(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	in.Now = scanTime
	r := report.Build(in, 0)
	var gaps []string
	for _, g := range r.Gaps {
		if g.Kind == report.GapMonitors {
			gaps = append(gaps, g.Text)
		}
	}
	wantGaps := []string{
		"kubernetes monitors: PodMonitor batch/legacy-jobs is selected by no Prometheus/monitoring/kps-kube-prometheus-stack-prometheus: it scrapes nothing, so no series or saving is attributed to it",
		"kubernetes monitors: ServiceMonitor shop/shop is selected by no Prometheus/monitoring/kps-kube-prometheus-stack-prometheus: it scrapes nothing, so no series or saving is attributed to it",
		"kubernetes monitors: scrape pool serviceMonitor/shop/shop/0 names a monitor the manifests don't show as selected (out of date?): its series aren't attributed to a monitor",
	}
	if !slices.Equal(gaps, wantGaps) {
		t.Errorf("monitor gaps:\n%s\nwant:\n%s", strings.Join(gaps, "\n"), strings.Join(wantGaps, "\n"))
	}
	found := false
	for _, s := range r.Sources {
		if s.Name == scan.SourceMonitors {
			found = true
			if s.Coverage != 1 {
				t.Errorf("monitors coverage %v, want 1", s.Coverage)
			}
		}
	}
	if !found {
		t.Errorf("no %q source", scan.SourceMonitors)
	}
	for _, rec := range r.Recommendations {
		for _, g := range rec.Evidence.Gaps {
			if strings.Contains(g, "kubernetes monitors") {
				t.Errorf("%s lists a monitor gap", rec.ID)
			}
		}
	}

	// A manifest tvt can't read fails the scan before any backend.
	if err := os.WriteFile(o.Config, []byte("kubernetes:\n  manifests: [missing/]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o.DB = filepath.Join(dir, "other.db")
	if err := runScan(context.Background(), o, env, io.Discard); err == nil {
		t.Error("a missing manifest path scanned")
	}
	if _, err := os.Stat(o.DB); !os.IsNotExist(err) {
		t.Error("a failed scan created the database")
	}
}

// streamPatterns asks Loki for exactly each stream (S5 debug pass): a
// selector of the stream's own labels would also match a stream that
// carries one more label and add its lines. Results stay keyed by the
// stream's own selector, which line findings look up.
func TestStreamPatternsExact(t *testing.T) {
	var queries []string
	srv := testutil.NewServer(t, testutil.Routes{"/loki/api/v1/patterns": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("query"))
		fmt.Fprint(w, `{"status":"success","data":[{"pattern":"x","samples":[[1,2]]}]}`)
	})})
	lc := loki.New(srv.URL, "")
	vols := []collect.StreamVolume{
		{Labels: map[string]string{"app": "shop", "level": "warn"}, Bytes: 10},
		{Labels: map[string]string{"app": "shop", "level": "warn", "pod": "a"}, Bytes: 5},
	}
	got, summary := streamPatterns(context.Background(), lc, vols)
	want := []string{`{app="shop", level="warn", pod=""}`, `{app="shop", level="warn", pod="a"}`}
	if !slices.Equal(queries, want) {
		t.Errorf("queries %q, want %q", queries, want)
	}
	if _, ok := got[`{app="shop", level="warn"}`]; !ok || len(got) != 2 || !strings.Contains(summary, "2 patterns in the 2 largest streams") {
		t.Errorf("patterns %v, summary %q", got, summary)
	}
}
