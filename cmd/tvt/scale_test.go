package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Scale test (S5 item 7). An avalanche-shaped Prometheus (the naming and
// labels of prometheus-community/avalanche) answers tvt's cardinality,
// series and metadata calls for metrics × seriesPer synthetic series; the
// rest of the lab stays as recorded. tvt only ever sees API answers, so
// this exercises the same paths as a real avalanche target, without a
// Prometheus holding millions of series in memory.
//
//	TVT_SCALE_SERIES=2000000 go test ./cmd/tvt -run TestScanScale -v
//
// prints scan time and peak heap for docs/architecture.md. The regression
// check runs on every build at a smaller size and fails when allocation or
// peak heap more than doubles against testdata/bench/scale.json
// (re-record with -update-bench).

// avalanche is a synthetic Prometheus of metrics × seriesPer series.
type avalanche struct {
	metrics, seriesPer int
}

const avalancheLabels = 10

func (a avalanche) name(i int) string { return fmt.Sprintf("avalanche_gauge_metric_mmmmm_0_%d", i) }

// tsdb answers /api/v1/status/tsdb: every metric has seriesPer series, so
// the top `limit` rows are the first metrics.
func (a avalanche) tsdb(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > a.metrics {
		limit = a.metrics
	}
	type row struct {
		Name  string `json:"name"`
		Value int64  `json:"value"`
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			HeadStats struct {
				NumSeries int64 `json:"numSeries"`
			} `json:"headStats"`
			SeriesCountByMetricName []row `json:"seriesCountByMetricName"`
		} `json:"data"`
	}
	resp.Status = "success"
	resp.Data.HeadStats.NumSeries = int64(a.metrics) * int64(a.seriesPer)
	for i := range limit {
		resp.Data.SeriesCountByMetricName = append(resp.Data.SeriesCountByMetricName, row{a.name(i), int64(a.seriesPer)})
	}
	json.NewEncoder(w).Encode(resp)
}

// series streams /api/v1/series for one metric: series_id varies, cycle_id
// is 0, and avalancheLabels constant label_key_kkkkk_N labels.
func (a avalanche) series(w http.ResponseWriter, r *http.Request) {
	m := r.URL.Query().Get("match[]")
	name := strings.TrimSuffix(strings.TrimPrefix(m, `{__name__="`), `"}`)
	bw := bufio.NewWriterSize(w, 64<<10)
	defer bw.Flush()
	bw.WriteString(`{"status":"success","data":[`)
	if strings.HasPrefix(name, "avalanche_") {
		var consts strings.Builder
		for k := range avalancheLabels {
			fmt.Fprintf(&consts, `,"label_key_kkkkk_%d":"label_val_vvvvv_%d"`, k, k)
		}
		for s := range a.seriesPer {
			if s > 0 {
				bw.WriteByte(',')
			}
			fmt.Fprintf(bw, `{"__name__":%q,"series_id":"%d","cycle_id":"0"%s}`, name, s, consts.String())
		}
	}
	bw.WriteString(`]}`)
}

// metadata answers /api/v1/targets/metadata: every metric is a gauge.
func (a avalanche) metadata(w http.ResponseWriter, r *http.Request) {
	bw := bufio.NewWriterSize(w, 64<<10)
	defer bw.Flush()
	bw.WriteString(`{"status":"success","data":[`)
	for i := range a.metrics {
		if i > 0 {
			bw.WriteByte(',')
		}
		fmt.Fprintf(bw, `{"target":{"instance":"avalanche:9001","job":"avalanche"},"metric":%q,"type":"gauge","help":"A tasty metric morsel","unit":""}`, a.name(i))
	}
	bw.WriteString(`]}`)
}

// scaleResult is one scale run.
type scaleResult struct {
	Metrics    int   `json:"metrics"`
	Series     int64 `json:"series"`
	TotalAlloc int64 `json:"total_alloc_bytes"`
	PeakHeap   int64 `json:"peak_heap_bytes"`
	// Elapsed is recorded for reference only: CI machines vary too much
	// to fail on it.
	Elapsed time.Duration `json:"elapsed_ns"`
}

// runScale scans the lab with an avalanche of metrics × seriesPer series
// in place of its Prometheus metrics.
func runScale(t *testing.T, a avalanche) scaleResult {
	t.Helper()
	setNow(t)
	l := newLab(t, map[string]http.HandlerFunc{
		"/api/v1/status/tsdb":      a.tsdb,
		"/api/v1/series":           a.series,
		"/api/v1/targets/metadata": a.metadata,
	})
	o := labOptions(l, filepath.Join(t.TempDir(), "tvt.db"))
	o.LabelTop = 50 // the flag default

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	peak := samplePeakHeap()
	start := time.Now()
	var out strings.Builder
	err := runScan(context.Background(), o, env, &out)
	elapsed := time.Since(start)
	peakHeap := peak()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	want := fmt.Sprintf("prom         %d metrics (%d series)", a.metrics, int64(a.metrics)*int64(a.seriesPer))
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out.String())
	}
	return scaleResult{
		Metrics: a.metrics, Series: int64(a.metrics) * int64(a.seriesPer),
		TotalAlloc: int64(after.TotalAlloc - before.TotalAlloc), PeakHeap: peakHeap, Elapsed: elapsed,
	}
}

// samplePeakHeap samples live heap objects every 10ms until the returned
// func is called, which returns the largest sample.
func samplePeakHeap() func() int64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() int64 {
		metrics.Read(sample)
		return int64(sample[0].Value.Uint64())
	}
	var mu sync.Mutex
	peak := read()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				v := read()
				mu.Lock()
				peak = max(peak, v)
				mu.Unlock()
			}
		}
	}()
	return func() int64 {
		close(done)
		<-stopped
		mu.Lock()
		defer mu.Unlock()
		return max(peak, read())
	}
}

func mib(n int64) string { return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20)) }

// TestScanScale is the measured run, at TVT_SCALE_SERIES series over
// TVT_SCALE_METRICS metrics (default 1000; skipped unless set).
func TestScanScale(t *testing.T) {
	v := os.Getenv("TVT_SCALE_SERIES")
	if v == "" {
		t.Skip("set TVT_SCALE_SERIES (e.g. 2000000) to run the scale test")
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1000 {
		t.Fatalf("TVT_SCALE_SERIES=%q: want at least 1000", v)
	}
	m := 1000
	if v := os.Getenv("TVT_SCALE_METRICS"); v != "" {
		if m, err = strconv.Atoi(v); err != nil || m < 1 || m > n {
			t.Fatalf("TVT_SCALE_METRICS=%q: want 1 to TVT_SCALE_SERIES", v)
		}
	}
	r := runScale(t, avalanche{metrics: m, seriesPer: n / m})
	t.Logf("%d series in %d metrics: scan %s, peak heap %s, allocated %s",
		r.Series, r.Metrics, r.Elapsed.Round(10*time.Millisecond), mib(r.PeakHeap), mib(r.TotalAlloc))
}

// scaleBaseline is the regression check's recorded run.
var scaleBaseline = filepath.Join("..", "..", "testdata", "bench", "scale.json")

// TestScanScaleRegression scans 200,000 synthetic series on every build
// and fails when allocation or peak heap (plus 16 MiB of sampling noise)
// more than doubles against the baseline. Skipped under -race, which inflates both.
func TestScanScaleRegression(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation under -race isn't comparable")
	}
	// Few, large metrics: the top 50 (the --label-top default) stream
	// 100,000 series, so per-series cost dominates.
	r := runScale(t, avalanche{metrics: 100, seriesPer: 2000})
	t.Logf("%d series: scan %s, peak heap %s, allocated %s", r.Series, r.Elapsed.Round(10*time.Millisecond), mib(r.PeakHeap), mib(r.TotalAlloc))
	if *updateBench {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(scaleBaseline, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(scaleBaseline)
	if err != nil {
		t.Fatalf("%v (record one with -update-bench)", err)
	}
	var base scaleResult
	if err := json.Unmarshal(b, &base); err != nil {
		t.Fatal(err)
	}
	if base.Series != r.Series {
		t.Fatalf("baseline is for %d series, this run %d: re-record with -update-bench", base.Series, r.Series)
	}
	if r.TotalAlloc > 2*base.TotalAlloc {
		t.Errorf("allocated %s, more than twice the baseline %s", mib(r.TotalAlloc), mib(base.TotalAlloc))
	}
	// Peak heap is sampled and small at this size: allow 16 MiB of noise.
	if r.PeakHeap > 2*base.PeakHeap+16<<20 {
		t.Errorf("peak heap %s, more than twice the baseline %s", mib(r.PeakHeap), mib(base.PeakHeap))
	}
}
