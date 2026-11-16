package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
)

// labelServer answers label values and, per match[]={l="v"}, metric names,
// recording each request's window.
func labelServer(t *testing.T, values map[string]map[string][]string, starts *[]string) string {
	t.Helper()
	reply := func(w http.ResponseWriter, data any) {
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
	}
	routes := testutil.Routes{}
	for l, byValue := range values {
		routes["/api/v1/label/"+l+"/values"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*starts = append(*starts, r.URL.Query().Get("start"))
			var vs []string
			for v := range byValue {
				vs = append(vs, v)
			}
			reply(w, vs)
		})
	}
	routes["/api/v1/label/__name__/values"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*starts = append(*starts, r.URL.Query().Get("start"))
		m := r.URL.Query().Get("match[]")
		for l, byValue := range values {
			for v, names := range byValue {
				if m == "{"+l+"="+strconv.Quote(v)+"}" {
					reply(w, names)
					return
				}
			}
		}
		t.Errorf("unexpected match[] %q", m)
		reply(w, []string{})
	})
	return testutil.NewServer(t, routes).URL
}

func TestLabelMetrics(t *testing.T) {
	values := map[string]map[string][]string{
		"job": {"shop": {"shop_http_requests_total", "up"}, "node": {"up", "node_load1"}},
		"app": {},
	}
	var starts []string
	now := time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)
	c := New(labelServer(t, values, &starts), "")
	c.Now = func() time.Time { return now }
	c.SeriesLookback = time.Hour
	got, err := c.LabelMetrics(context.Background(), []string{"job", "app", "__name__", "job"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string][]string{
		"job": {"node": {"node_load1", "up"}, "shop": {"shop_http_requests_total", "up"}},
		"app": {},
	}
	if !reflect.DeepEqual(map[string]map[string][]string(got), want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// job once (repeated name skipped), app once, a name lookup per job
	// value; __name__ itself is never a handled label.
	if len(starts) != 4 {
		t.Errorf("%d requests, want 4", len(starts))
	}
	for _, s := range starts {
		if s != strconv.FormatInt(now.Add(-time.Hour).Unix(), 10) {
			t.Errorf("start %s, want the lookback window", s)
		}
	}
}

func TestLabelMetricsTruncated(t *testing.T) {
	old := LabelValuesLimit
	LabelValuesLimit = 1
	t.Cleanup(func() { LabelValuesLimit = old })
	var starts []string
	got, err := New(labelServer(t, map[string]map[string][]string{"job": {"a": {"x"}, "b": {"y"}}}, &starts), "").
		LabelMetrics(context.Background(), []string{"job"})
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err %v, want ErrTruncated", err)
	}
	if !reflect.DeepEqual(map[string]map[string][]string(got), map[string]map[string][]string{"job": {"a": {"x"}}}) {
		t.Errorf("got %v: want what was read, the first value", got)
	}
}

func TestLabelMetricsError(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Routes{"/api/v1/label/job/values": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})})
	if got, err := New(srv.URL, "").LabelMetrics(context.Background(), []string{"job"}); err == nil || got != nil {
		t.Errorf("got %v, %v: want an error and nothing", got, err)
	}
}

func TestSelectorMetrics(t *testing.T) {
	now := time.Date(2026, 11, 16, 22, 0, 0, 0, time.UTC)
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/label/__name__/values" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("match[]") {
		case `{asserts_metric_request="total"}`:
			fmt.Fprint(w, `{"status":"success","data":["b_total","a_total"]}`)
		case `{team="none"}`:
			fmt.Fprint(w, `{"status":"success","data":[]}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"vector selector must contain at least one non-empty matcher"}`)
		}
	}))
	c := New(srv.URL, "")
	c.Now = func() time.Time { return now }
	got, err := c.SelectorMetrics(context.Background(), []string{`{asserts_metric_request="total"}`, `{team="none"}`, `{x=~".*"}`})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{`{asserts_metric_request="total"}`: {"a_total", "b_total"}, `{team="none"}`: {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (a refused selector is left out)", got, want)
	}
}
