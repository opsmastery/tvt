package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opsmastery/tvt/internal/httpx"
)

// TestScanFailsOnUnavailableBackend: a backend that keeps failing fails
// the scan after the retries, and nothing is written; one that recovers
// within the retries doesn't (S6). The scan used to record "stream volume
// unavailable" and write a graph without the streams.
func TestScanFailsOnUnavailableBackend(t *testing.T) {
	setNow(t)
	retry := httpx.RetryPolicy{Retries: 2, Base: time.Millisecond, Max: 2 * time.Millisecond}
	t.Cleanup(func() { httpx.SetRetry(httpx.DefaultRetry); httpx.ResetFailures() })

	t.Run("keeps failing", func(t *testing.T) {
		var calls atomic.Int32
		l := newLab(t, map[string]http.HandlerFunc{
			"loki:/loki/api/v1/index/volume": func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "ingester down", http.StatusServiceUnavailable)
			},
		})
		db := filepath.Join(t.TempDir(), "tvt.db")
		o := labOptions(l, db)
		o.Retry = retry
		var out bytes.Buffer
		err := runScan(context.Background(), o, env, &out)
		if err == nil || !strings.Contains(err.Error(), "unavailable after 3 attempts") || !strings.Contains(err.Error(), "nothing was written") {
			t.Fatalf("err = %v, want the scan to fail after 3 attempts\n%s", err, out.String())
		}
		if calls.Load() != 3 {
			t.Errorf("volume asked %d times, want 3 (fails fast after)", calls.Load())
		}
		if _, err := os.Stat(db); !os.IsNotExist(err) {
			t.Errorf("the database was written (stat err %v)", err)
		}
	})

	// A required endpoint refused (a token that lost a permission, a wrong
	// URL) fails the scan too; an optional one (scrape intervals, a cost
	// input) is a gap.
	t.Run("required refused", func(t *testing.T) {
		l := newLab(t, map[string]http.HandlerFunc{
			"loki:/loki/api/v1/index/volume": func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "permission denied", http.StatusForbidden)
			},
		})
		db := filepath.Join(t.TempDir(), "tvt.db")
		o := labOptions(l, db)
		o.Retry = retry
		var out bytes.Buffer
		err := runScan(context.Background(), o, env, &out)
		if err == nil || !strings.Contains(err.Error(), "/loki/api/v1/index/volume refused: HTTP 403: permission denied") || !strings.Contains(err.Error(), "nothing was written") {
			t.Fatalf("err = %v, want the scan to fail on the refusal\n%s", err, out.String())
		}
		if _, err := os.Stat(db); !os.IsNotExist(err) {
			t.Errorf("the database was written (stat err %v)", err)
		}
	})

	t.Run("optional refused", func(t *testing.T) {
		l := newLab(t, map[string]http.HandlerFunc{
			"/api/v1/targets": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		})
		db := filepath.Join(t.TempDir(), "tvt.db")
		o := labOptions(l, db)
		o.Retry = retry
		var out bytes.Buffer
		if err := runScan(context.Background(), o, env, &out); err != nil {
			t.Fatalf("an optional 404 failed the scan: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "scrape intervals unavailable") {
			t.Errorf("no gap recorded for the scrape intervals:\n%s", out.String())
		}
	})

	t.Run("recovers", func(t *testing.T) {
		var calls atomic.Int32
		volume, err := os.ReadFile(filepath.Join(lab, "loki/volume_by_stream.json"))
		if err != nil {
			t.Fatal(err)
		}
		l := newLab(t, map[string]http.HandlerFunc{
			"loki:/loki/api/v1/index/volume": func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					http.Error(w, "busy", http.StatusTooManyRequests)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(volume)
			},
		})
		db := filepath.Join(t.TempDir(), "tvt.db")
		o := labOptions(l, db)
		o.Retry = retry
		var out bytes.Buffer
		if err := runScan(context.Background(), o, env, &out); err != nil {
			t.Fatalf("scan: %v\n%s", err, out.String())
		}
		if calls.Load() < 2 {
			t.Errorf("volume asked %d times, want a retry", calls.Load())
		}
	})
}
