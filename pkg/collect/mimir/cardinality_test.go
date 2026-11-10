package mimir

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

const fixtures = "../../../testdata/lab/mimir"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixtures + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// server routes label_values on __name__ to names, other paths to extra,
// and fails the test on anything but a GET.
func server(t *testing.T, names *testutil.MimirNames, extra map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s: tvt must only GET", r.Method, r.URL.Path)
		}
		if h, ok := extra[r.URL.Path]; ok {
			h(w, r)
			return
		}
		if r.URL.Path == testutil.MimirLabelValuesPath && names != nil {
			names.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMetricCardinalityFixture(t *testing.T) {
	names := testutil.LoadMimirNames(t, fixtures+"/label_values_name_by_prefix.json")
	f := &testutil.MimirNames{T: t, Names: names, Top: readFixture(t, "label_values_name.json")}
	got, err := New(server(t, f, nil).URL+"/prometheus", "").MetricCardinality(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The unpartitioned capture is cut at 500 of the lab's names, so the
	// client must have partitioned to find the rest.
	if sel := f.Selectors(); len(sel) < 2 || sel[0] != "" {
		t.Errorf("selectors = %q, want the unpartitioned call and then partitions", sel)
	}
	if len(got) != len(names) {
		t.Fatalf("got %d metrics, want %d", len(got), len(names))
	}
	for i, m := range got {
		if names[m.Metric] != m.ActiveSeries {
			t.Errorf("%s: %d series, want %d", m.Metric, m.ActiveSeries, names[m.Metric])
		}
		if i > 0 && got[i-1].ActiveSeries < m.ActiveSeries {
			t.Errorf("not sorted by series at %d", i)
		}
	}
	if got[0].Metric != "shop_feature_flag_evaluations_created" && got[0].Metric != "shop_feature_flag_evaluations_total" {
		t.Errorf("largest metric = %+v", got[0])
	}
	for _, m := range []string{"shop_cart_operations_total", "shop_payment_queue_depth", "route:shop_http_request_duration_seconds:p99_5m", "node_load15"} {
		if _, ok := names[m]; !ok {
			t.Errorf("lab metric %s missing from the capture", m)
		}
	}
}

func TestMetricCardinalityDeepPartitions(t *testing.T) {
	// "x" is both a metric and the prefix of 30 others; with a limit of 5,
	// "x" itself is only found by the exact-name query.
	names := map[string]int64{"x": 7, "y": 1}
	for i := range 30 {
		names[fmt.Sprintf("x%02d", i)] = int64(i + 1)
	}
	f := &testutil.MimirNames{T: t, Names: names}
	c := New(server(t, f, nil).URL+"/prometheus", "")
	c.Limit = 5
	got, err := c.MetricCardinality(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gotNames := map[string]int64{}
	for _, m := range got {
		gotNames[m.Metric] = m.ActiveSeries
	}
	if !reflect.DeepEqual(gotNames, names) {
		t.Errorf("got %v\nwant %v", gotNames, names)
	}
	exact := false
	for _, s := range f.Selectors() {
		if s == `{__name__="x"}` {
			exact = true
		}
	}
	if !exact {
		t.Errorf("no exact query for the prefix metric; selectors %q", f.Selectors())
	}
}

func TestMetricCardinalityTruncated(t *testing.T) {
	old := maxPrefix
	maxPrefix = 1
	t.Cleanup(func() { maxPrefix = old })
	names := map[string]int64{}
	for i := range 20 {
		names[fmt.Sprintf("ab%02d", i)] = 1
	}
	c := New(server(t, &testutil.MimirNames{T: t, Names: names}, nil).URL+"/prometheus", "")
	c.Limit = 5
	got, err := c.MetricCardinality(context.Background())
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("got %d metrics, err %v; want ErrTruncated", len(got), err)
	}
}

func TestLimitNeverExceedsMax(t *testing.T) {
	names := map[string]int64{"a": 1}
	for _, lim := range []int{0, -1, 501, 10000} {
		c := New(server(t, &testutil.MimirNames{T: t, Names: names}, nil).URL+"/prometheus", "")
		c.Limit = lim
		if _, err := c.MetricCardinality(context.Background()); err != nil {
			t.Errorf("Limit %d: %v", lim, err)
		}
	}
}

// cartOpsServer answers label_names for shop_cart_operations_total with
// labelNames, and label_values with the captured per-label counts, checking
// the requested label names.
func cartOpsServer(t *testing.T, labelNames []byte) *httptest.Server {
	body := readFixture(t, "label_values_cart_ops.json")
	extra := map[string]http.HandlerFunc{}
	extra["/prometheus"+labelNamesPath] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("selector") != `{__name__="shop_cart_operations_total"}` {
			http.Error(w, "unexpected selector", http.StatusBadRequest)
			return
		}
		w.Write(labelNames)
	}
	extra["/prometheus"+labelValuesPath] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got := append([]string(nil), q["label_names[]"]...)
		sort.Strings(got)
		if want := []string{"instance", "job", "op", "user_id"}; !reflect.DeepEqual(got, want) || q.Get("limit") != "0" {
			t.Errorf("label_values: label_names %q limit %q, want %q and 0", got, q.Get("limit"), want)
		}
		w.Write(body)
	}
	return server(t, nil, extra)
}

func TestLabelCardinalityFixture(t *testing.T) {
	srv := cartOpsServer(t, readFixture(t, "label_names_cart_ops.json"))
	got, err := New(srv.URL+"/prometheus", "").LabelCardinality(context.Background(), "shop_cart_operations_total")
	if err != nil {
		t.Fatal(err)
	}
	want := []collect.LabelCardinality{
		{Metric: "shop_cart_operations_total", Label: "user_id", Values: 2000, ActiveSeries: 6000},
		{Metric: "shop_cart_operations_total", Label: "op", Values: 3, ActiveSeries: 6000},
		{Metric: "shop_cart_operations_total", Label: "instance", Values: 1, ActiveSeries: 6000},
		{Metric: "shop_cart_operations_total", Label: "job", Values: 1, ActiveSeries: 6000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels:\n got %+v\nwant %+v", got, want)
	}
}

func TestLabelCardinalityTruncatedNames(t *testing.T) {
	srv := cartOpsServer(t, []byte(`{"label_values_count_total":2006,"label_names_count":5,"cardinality":[{"label_name":"user_id","label_values_count":2000}]}`))
	_, err := New(srv.URL+"/prometheus", "").LabelCardinality(context.Background(), "shop_cart_operations_total")
	if !errors.Is(err, ErrTruncated) {
		t.Errorf("err = %v, want ErrTruncated", err)
	}
}

func TestLabelCardinalityNoSeries(t *testing.T) {
	srv := cartOpsServer(t, []byte(`{"label_values_count_total":0,"label_names_count":0,"cardinality":[]}`))
	got, err := New(srv.URL+"/prometheus", "").LabelCardinality(context.Background(), "shop_cart_operations_total")
	if err != nil || len(got) != 0 {
		t.Errorf("got %+v, %v; want no labels and no error", got, err)
	}
}

func TestErrors(t *testing.T) {
	disabled := readFixture(t, "cardinality_disabled.txt")
	for name, tc := range map[string]struct {
		status int
		body   []byte
		is     error
		want   string
	}{
		"analysis disabled": {400, disabled, ErrDisabled, "limits.cardinality_analysis_enabled: true"},
		"limit too large":   {400, []byte(`{"status":"error","errorType":"bad_data","error":"'limit' param cannot be greater than '500'"}`), nil, "cannot be greater than '500'"},
		"wrong base URL":    {404, []byte("404 page not found"), nil, "/prometheus root"},
		"server error":      {500, []byte("boom"), nil, "HTTP 500: boom"},
		"malformed":         {200, []byte(`{"labels":[`), nil, "decode"},
	} {
		t.Run(name, func(t *testing.T) {
			var auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
				w.Write(tc.body)
			}))
			t.Cleanup(srv.Close)
			c := New(srv.URL+"/prometheus", "secret-token")
			for call, err := range map[string]error{
				"MetricCardinality": func() error { _, err := c.MetricCardinality(context.Background()); return err }(),
				"LabelCardinality":  func() error { _, err := c.LabelCardinality(context.Background(), "m"); return err }(),
			} {
				if err == nil {
					t.Fatalf("%s: no error", call)
				}
				if tc.is != nil && !errors.Is(err, tc.is) {
					t.Errorf("%s: err = %v, want %v", call, err, tc.is)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: err = %v, want it to mention %q", call, err, tc.want)
				}
				if strings.Contains(err.Error(), "secret-token") {
					t.Errorf("%s: error leaks the token: %v", call, err)
				}
			}
			if auth != "Bearer secret-token" {
				t.Errorf("Authorization = %q", auth)
			}
		})
	}
}
