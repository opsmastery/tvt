package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

var fixtures = filepath.Join(testutil.Lab, "prometheus")

// fixtureServer serves recorded lab responses by API path.
func fixtureServer(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	routes := testutil.Routes{}
	for path, name := range files {
		routes[path] = testutil.File(t, "lab/prometheus/"+name)
	}
	return testutil.NewServer(t, routes)
}

func TestMetricCardinalityFixture(t *testing.T) {
	srv := fixtureServer(t, map[string]string{"/api/v1/status/tsdb": "tsdb.json"})
	got, err := New(srv.URL, "").MetricCardinality(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var raw struct {
		Data tsdbStatus `json:"data"`
	}
	b, _ := os.ReadFile(filepath.Join(fixtures, "tsdb.json"))
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(raw.Data.SeriesCountByMetricName) {
		t.Fatalf("got %d metrics, fixture has %d", len(got), len(raw.Data.SeriesCountByMetricName))
	}
	var sum int64
	byName := map[string]int64{}
	for i, m := range got {
		sum += m.ActiveSeries
		byName[m.Metric] = m.ActiveSeries
		if i > 0 && got[i-1].ActiveSeries < m.ActiveSeries {
			t.Fatalf("not sorted by series at %d", i)
		}
	}
	if sum != raw.Data.HeadStats.NumSeries {
		t.Errorf("series sum %d != headStats.numSeries %d (fixture truncated?)", sum, raw.Data.HeadStats.NumSeries)
	}
	// Seeded waste from tvt-lab/ground-truth.yaml: the feature-flag counter
	// (2,000 user_ids x flags x variants) is the largest metric in the lab.
	if got[0].Metric != "shop_feature_flag_evaluations_created" && got[0].Metric != "shop_feature_flag_evaluations_total" {
		t.Errorf("largest metric = %s, want a shop_feature_flag_evaluations series", got[0].Metric)
	}
	for _, m := range []string{"shop_cart_operations_total", "shop_http_requests_total", "shop_payment_queue_depth"} {
		if byName[m] == 0 {
			t.Errorf("%s missing", m)
		}
	}
}

// tsdbServer fakes /api/v1/status/tsdb for n metrics with one series each,
// honouring limit, and records the limits requested.
func tsdbServer(t *testing.T, n int, limits *[]int) *httptest.Server {
	t.Helper()
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		*limits = append(*limits, limit)
		var rows []string
		for i := 0; i < n && i < limit; i++ {
			rows = append(rows, fmt.Sprintf(`{"name":"m%d","value":1}`, i))
		}
		fmt.Fprintf(w, `{"status":"success","data":{"headStats":{"numSeries":%d},"seriesCountByMetricName":[%s]}}`,
			n, strings.Join(rows, ","))
	}))
	return srv
}

func TestMetricCardinalityRaisesLimitUntilComplete(t *testing.T) {
	var limits []int
	srv := tsdbServer(t, 25000, &limits)
	got, err := New(srv.URL, "").MetricCardinality(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 25000 {
		t.Errorf("got %d metrics, want 25000", len(got))
	}
	if want := []int{10000, 20000, 40000}; fmt.Sprint(limits) != fmt.Sprint(want) {
		t.Errorf("limits requested = %v, want %v", limits, want)
	}
}

func TestMetricCardinalityTruncated(t *testing.T) {
	old := tsdbMaxLimit
	tsdbMaxLimit = 20000
	t.Cleanup(func() { tsdbMaxLimit = old })

	var limits []int
	srv := tsdbServer(t, 50000, &limits)
	_, err := New(srv.URL, "").MetricCardinality(context.Background())
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestLabelCardinality(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	var gotQuery map[string][]string
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		fmt.Fprint(w, `{"status":"success","data":[
			{"__name__":"cart","op":"add","user_id":"u1","job":"shop"},
			{"__name__":"cart","op":"add","user_id":"u2","job":"shop"},
			{"__name__":"cart","op":"view","user_id":"u2","job":"shop"},
			{"__name__":"cart","op":"view","job":"shop"}
		]}`)
	}))

	c := New(srv.URL, "")
	c.Now = func() time.Time { return now }
	got, err := c.LabelCardinality(context.Background(), "cart")
	if err != nil {
		t.Fatal(err)
	}
	want := []collect.LabelCardinality{
		{Metric: "cart", Label: "op", Values: 2, ActiveSeries: 4},
		{Metric: "cart", Label: "user_id", Values: 2, ActiveSeries: 3},
		{Metric: "cart", Label: "job", Values: 1, ActiveSeries: 4},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if m := gotQuery["match[]"]; len(m) != 1 || m[0] != `{__name__="cart"}` {
		t.Errorf("match[] = %v", m)
	}
	if gotQuery["end"][0] != strconv.FormatInt(now.Unix(), 10) ||
		gotQuery["start"][0] != strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10) {
		t.Errorf("start/end = %v/%v", gotQuery["start"], gotQuery["end"])
	}
	if _, ok := gotQuery["limit"]; ok {
		t.Error("limit sent although SeriesLimit is 0")
	}
}

func TestLabelCardinalityTruncatedFixture(t *testing.T) {
	// Recorded with limit=200 against 5,000+ series, so Prometheus warned.
	srv := fixtureServer(t, map[string]string{"/api/v1/series": "series_cart_ops.json"})
	c := New(srv.URL, "")
	c.SeriesLimit = 200
	_, err := c.LabelCardinality(context.Background(), "shop_cart_operations_total")
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestRulesFixture(t *testing.T) {
	srv := fixtureServer(t, map[string]string{"/api/v1/rules": "rules.json"})
	got, err := New(srv.URL, "").Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]collect.RuleKind{}
	for _, r := range got {
		kinds[r.Name] = r.Kind
		if r.Source != "prometheus" || r.Language != "promql" || r.Expr == "" ||
			r.Namespace != "/etc/prometheus/rules/shop.yml" || r.Interval != 30*time.Second {
			t.Errorf("bad rule %+v", r)
		}
	}
	want := map[string]collect.RuleKind{
		"ShopHighLatency":         collect.RuleAlert,
		"ShopHighErrorRate":       collect.RuleAlert,
		"ShopPaymentQueueBacklog": collect.RuleAlert,
	}
	for name, kind := range want {
		if kinds[name] != kind {
			t.Errorf("%s: kind %q, want %q", name, kinds[name], kind)
		}
	}
	if kinds["route:shop_http_request_duration_seconds:p99_5m"] != collect.RuleRecord {
		t.Errorf("recording rule feeding ShopHighLatency missing: %v", kinds)
	}
}

func TestRulesUnknownTypeFails(t *testing.T) {
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"groups":[{"name":"g","file":"f","rules":[{"name":"x","query":"up","type":"mystery"}]}]}}`)
	}))
	if _, err := New(srv.URL, "").Rules(context.Background()); err == nil {
		t.Fatal("unknown rule type accepted")
	}
}

func TestTokenSentAndNeverInErrors(t *testing.T) {
	const token = "s3cret-token"
	var auth, ua string
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, ua = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"status":"error","errorType":"unauthorized","error":"bad credentials"}`)
	}))

	_, err := New(srv.URL, token).Rules(context.Background())
	if err == nil {
		t.Fatal("want error on 401")
	}
	if auth != "Bearer "+token {
		t.Errorf("Authorization = %q", auth)
	}
	if !strings.HasPrefix(ua, "tvt/") {
		t.Errorf("User-Agent = %q", ua)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaks token: %v", err)
	}
	if !strings.Contains(err.Error(), "unauthorized: bad credentials") {
		t.Errorf("error lacks API message: %v", err)
	}
}

func TestBaseURLPrefix(t *testing.T) {
	// Mimir serves the Prometheus API under /prometheus.
	var path string
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		fmt.Fprint(w, `{"status":"success","data":{"groups":[]}}`)
	}))
	c := New(srv.URL+"/prometheus", "")
	c.Source = "mimir"
	if _, err := c.Rules(context.Background()); err != nil {
		t.Fatal(err)
	}
	if path != "/prometheus/api/v1/rules" {
		t.Errorf("path = %s", path)
	}
}

func TestMimirHasNoTSDBStatus(t *testing.T) {
	srv := testutil.Serve(t, http.NotFoundHandler()) // what Mimir 2.16 does
	c := New(srv.URL+"/prometheus", "")
	c.Source = "mimir"
	_, err := c.MetricCardinality(context.Background())
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "pkg/collect/mimir") {
		t.Fatalf("err = %v, want ErrNotFound with a hint to use pkg/collect/mimir", err)
	}
}
