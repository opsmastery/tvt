package grafana

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/internal/testutil"
)

// FuzzLogLine: the Grafana log line parser never panics, and accepts a line
// only with logger=.
func FuzzLogLine(f *testing.F) {
	for _, l := range testutil.Lines(f, "lab/grafana/grafana.log", 40) {
		f.Add(l)
	}
	for _, g := range testutil.Garbage {
		f.Add(g)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if _, kv, ok := parseLogLine(raw); ok && kv["logger"] == "" {
			t.Errorf("accepted %q without logger=", raw)
		}
	})
}

// FuzzRouterLogFile: reading any file never panics; a non-empty file with no
// Grafana log line is an error, never an empty result that would make every
// dashboard look unviewed; views name a dashboard and never tvt itself;
// coverage stays in 0..1.
func FuzzRouterLogFile(f *testing.F) {
	f.Add(bytes.Join(testutil.Lines(f, "lab/grafana/grafana.log", 20), []byte("\n")))
	for _, g := range testutil.Garbage {
		f.Add(g)
	}
	now := time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "grafana.log")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		var lines, entries int
		for _, l := range bytes.Split(data, []byte("\n")) {
			if l = bytes.TrimSpace(l); len(l) > 0 {
				lines++
				if _, _, ok := parseLogLine(l); ok {
					entries++
				}
			}
		}
		rl := &RouterLog{Paths: []string{path}, ExcludeUsers: []string{"sa-1-tvt"}, Now: func() time.Time { return now }}
		views, err := rl.DashboardViews(context.Background(), time.Time{})
		if lines > 0 && entries == 0 {
			if err == nil {
				t.Fatalf("garbage read as an empty Grafana log: %d views", len(views))
			}
			return
		}
		if err != nil {
			t.Fatalf("DashboardViews: %v", err)
		}
		for _, v := range views {
			if v.DashboardUID == "" || strings.Contains(v.DashboardUID, "/") || v.User == "sa-1-tvt" {
				t.Errorf("bad view %+v", v)
			}
		}
		cov, err := rl.Coverage(context.Background(), now.Add(-30*24*time.Hour))
		if err != nil || math.IsNaN(cov) || cov < 0 || cov > 1 {
			t.Errorf("Coverage = %v, %v", cov, err)
		}
	})
}
