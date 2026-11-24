package prometheus

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/internal/testutil"
)

// The lab's own /metrics: 45d retention, a 20GB size cap, ~0.62 bytes per
// compacted sample.
func TestStorageFixture(t *testing.T) {
	got, err := New(fixtureServer(t, map[string]string{"/metrics": "metrics.txt"}).URL, "").Storage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.BytesPerSample-0.616) > 0.01 {
		t.Errorf("bytes per sample %v", got.BytesPerSample)
	}
	if got.HeadSeries != 60239 || got.HeadBytes < 2e8 || got.BlocksBytes < 2e9 {
		t.Errorf("head %d series %d bytes, blocks %d", got.HeadSeries, got.HeadBytes, got.BlocksBytes)
	}
	if got.RetentionTime != 45*24*time.Hour || got.RetentionBytes != 21474836480 {
		t.Errorf("retention %v / %d", got.RetentionTime, got.RetentionBytes)
	}
}

func TestStorageErrors(t *testing.T) {
	for name, body := range map[string]string{
		"not prometheus": "# TYPE up gauge\nup 1\n",
		"garbage":        "{not exposition",
	} {
		srv := testutil.NewServer(t, testutil.Routes{"/metrics": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		})})
		if _, err := New(srv.URL, "").Storage(context.Background()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// Before the first compaction there is no ratio; that's not an error.
	srv := testutil.NewServer(t, testutil.Routes{"/metrics": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Join([]string{"# TYPE prometheus_tsdb_head_series gauge", "prometheus_tsdb_head_series 10", ""}, "\n")))
	})})
	got, err := New(srv.URL, "").Storage(context.Background())
	if err != nil || got.BytesPerSample != 0 || got.HeadSeries != 10 {
		t.Errorf("fresh server: %+v %v", got, err)
	}
}
