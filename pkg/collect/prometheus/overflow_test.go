package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/opsmastery/tvt/internal/testutil"
	"github.com/opsmastery/tvt/pkg/collect"
)

// overflowServer answers the overflow reader: series per match[], and
// label names per metric. utf8 false answers the quoted UTF-8 match with
// 400, as Prometheus 2 does.
func overflowServer(t *testing.T, series map[string][]map[string]string, labels map[string][]string, utf8 bool) string {
	t.Helper()
	reply := func(w http.ResponseWriter, data any) {
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
	}
	return testutil.NewServer(t, testutil.Routes{
		"/api/v1/series": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m := r.URL.Query().Get("match[]")
			if m == `{"otel.metric.overflow"="true"}` && !utf8 {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error: unexpected string"}`))
				return
			}
			s := series[m]
			if s == nil {
				s = []map[string]string{}
			}
			reply(w, s)
		}),
		"/api/v1/labels": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for name, ls := range labels {
				if r.URL.Query().Get("match[]") == `{__name__="`+name+`"}` {
					reply(w, ls)
					return
				}
			}
			reply(w, []string{})
		}),
	}).URL
}

func TestOverflow(t *testing.T) {
	series := map[string][]map[string]string{
		`{otel_metric_overflow="true"}`: {
			{"__name__": "calls_total", "job": "shop", "otel_metric_overflow": "true"},
			{"__name__": "duration_seconds_bucket", "job": "shop", "le": "0.1", "otel_metric_overflow": "true"},
			{"__name__": "duration_seconds_bucket", "job": "shop", "le": "+Inf", "otel_metric_overflow": "true"},
		},
		`{"otel.metric.overflow"="true"}`: {
			{"__name__": "utf8.calls", "service.name": "pay", "otel.metric.overflow": "true"},
		},
	}
	labels := map[string][]string{
		"calls_total":             {"__name__", "customer_id", "job", "otel_metric_overflow", "span_name"},
		"duration_seconds_bucket": {"__name__", "customer_id", "job", "le", "otel_metric_overflow"},
		"utf8.calls":              {"__name__", "otel.metric.overflow", "service.name", "user.id"},
	}
	got, err := New(overflowServer(t, series, labels, true), "").Overflow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []collect.Overflow{
		{Metric: "calls_total", Series: 1, Kept: []string{"job"}, Labels: []string{"customer_id", "job", "span_name"}},
		{Metric: "duration_seconds_bucket", Series: 2, Kept: []string{"job", "le"}, Labels: []string{"customer_id", "job", "le"}},
		{Metric: "utf8.calls", Series: 1, Kept: []string{"service.name"}, Labels: []string{"service.name", "user.id"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}

	// Prometheus 2 rejects the quoted UTF-8 name: none under it, no error.
	got, err = New(overflowServer(t, series, labels, false), "").Overflow(context.Background())
	if err != nil || len(got) != 2 {
		t.Errorf("Prometheus 2: %+v, %v: want the two translated metrics", got, err)
	}

	// No overflow anywhere: none.
	got, err = New(overflowServer(t, nil, nil, true), "").Overflow(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("none: %+v, %v", got, err)
	}
}

func TestOverflowErrors(t *testing.T) {
	// The translated name failing is an error, whatever the code.
	srv := testutil.NewServer(t, testutil.Routes{"/api/v1/series": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"nope"}`))
	})})
	if _, err := New(srv.URL, "").Overflow(context.Background()); !errors.Is(err, ErrBadRequest) {
		t.Errorf("err %v, want ErrBadRequest from the first match", err)
	}
	// A truncated answer is ErrTruncated, never a partial list.
	srv = testutil.NewServer(t, testutil.Routes{"/api/v1/series": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":[],"warnings":["results truncated due to limit"]}`))
	})})
	if got, err := New(srv.URL, "").Overflow(context.Background()); !errors.Is(err, ErrTruncated) || got != nil {
		t.Errorf("truncated: %v, %v", got, err)
	}
}
