package prometheus

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
)

var scrapeFixtures = map[string]string{
	"/api/v1/targets":          "targets.json",
	"/api/v1/targets/metadata": "targets_metadata.json",
	"/api/v1/status/flags":     "flags.json",
}

// The lab: every job at the 10s global interval except alertmanager (15s,
// tvt-lab's S3 lower-resolution seed); exemplar storage off.
func TestScrapeFixture(t *testing.T) {
	got, err := New(fixtureServer(t, scrapeFixtures).URL, "").Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Pools, "shop") || len(got.Pools) != len(got.JobIntervals) {
		t.Errorf("pools = %v", got.Pools)
	}
	if got.JobIntervals["alertmanager"] != 15*time.Second || got.JobIntervals["shop"] != 10*time.Second || len(got.JobIntervals) != 6 {
		t.Errorf("job intervals %v", got.JobIntervals)
	}
	for series, want := range map[string]struct {
		family, typ string
		interval    time.Duration
	}{
		"shop_http_requests_total":                  {"shop_http_requests", "counter", 10 * time.Second},
		"shop_http_request_duration_seconds_bucket": {"shop_http_request_duration_seconds", "histogram", 10 * time.Second},
		"node_cpu_seconds_total":                    {"node_cpu_seconds_total", "counter", 10 * time.Second},
		"alertmanager_alerts":                       {"alertmanager_alerts", "gauge", 15 * time.Second},
	} {
		f, ok := got.Family(series)
		if !ok || f != want.family || got.Types[f] != want.typ || got.Intervals[f] != want.interval {
			t.Errorf("%s: family %q (%v) type %q interval %v; want %+v", series, f, ok, got.Types[f], got.Intervals[f], want)
		}
	}
	if _, ok := got.Family("route:shop_http_request_duration_seconds:p99_5m"); ok {
		t.Error("a recording rule's output has no target metadata")
	}
	if got.Exemplars {
		t.Error("exemplars on; the lab runs without exemplar-storage")
	}
}

func TestScrapeExemplarsAndErrors(t *testing.T) {
	files := map[string]string{"/api/v1/targets": "targets.json", "/api/v1/targets/metadata": "targets_metadata.json"}
	routes := testutil.Routes{}
	for p, n := range files {
		routes[p] = testutil.File(t, "lab/prometheus/"+n)
	}
	routes["/api/v1/status/flags"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"enable-feature":"native-histograms, exemplar-storage"}}`))
	})
	got, err := New(testutil.NewServer(t, routes).URL, "").Scrape(context.Background())
	if err != nil || !got.Exemplars {
		t.Fatalf("exemplars = %v, %v", got.Exemplars, err)
	}

	routes["/api/v1/targets"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"activeTargets":[{"labels":{"job":"x"},"scrapeInterval":"soon"}]}}`))
	})
	if _, err := New(testutil.NewServer(t, routes).URL, "").Scrape(context.Background()); err == nil || !strings.Contains(err.Error(), "scrape interval") {
		t.Errorf("bad interval: %v", err)
	}
}

// Metadata is read one job at a time, from a few targets each: unfiltered,
// the API returns every target's copy of every metric.
func TestScrapeMetadataPerJob(t *testing.T) {
	var asked []string
	routes := testutil.Routes{
		"/api/v1/targets":      testutil.File(t, "lab/prometheus/targets.json"),
		"/api/v1/status/flags": testutil.File(t, "lab/prometheus/flags.json"),
		"/api/v1/targets/metadata": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("limit") != "5" || !strings.HasPrefix(q.Get("match_target"), `{job="`) {
				t.Errorf("unbounded metadata request: %s", r.URL.RawQuery)
			}
			asked = append(asked, q.Get("match_target"))
			w.Write([]byte(`{"status":"success","data":[{"target":{"job":"shop"},"metric":"shop_x","type":"gauge"}]}`))
		}),
	}
	got, err := New(testutil.NewServer(t, routes).URL, "").Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 6 || asked[0] != `{job="alertmanager"}` {
		t.Errorf("metadata requests %q, want one per job in order", asked)
	}
	if got.Intervals["shop_x"] != 10*time.Second {
		t.Errorf("shop_x interval %v", got.Intervals["shop_x"])
	}
}
