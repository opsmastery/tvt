package prometheus

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
)

// FuzzQueryLogLine: the line parser never panics, and accepts a line only
// with a timestamp and query text.
func FuzzQueryLogLine(f *testing.F) {
	for _, l := range testutil.Lines(f, "lab/prometheus/query.log", 40) {
		f.Add(l)
	}
	for _, g := range testutil.Garbage {
		f.Add(g)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		l, ok := parseQueryLogLine(raw)
		if ok && (l.at().IsZero() || l.Params.Query == "") {
			t.Errorf("accepted %q without a timestamp or query: %+v", raw, l)
		}
	})
}

// FuzzQueryLogFile: reading any file never panics; a non-empty file with no
// query log entry is an error, never an empty result that would make every
// metric look unqueried; queries carry their text; coverage stays in 0..1.
func FuzzQueryLogFile(f *testing.F) {
	f.Add(bytes.Join(testutil.Lines(f, "lab/prometheus/query.log", 20), []byte("\n")))
	for _, g := range testutil.Garbage {
		f.Add(g)
	}
	now := time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "query.log")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		var lines, entries int
		for _, l := range bytes.Split(data, []byte("\n")) {
			if l = bytes.TrimSpace(l); len(l) > 0 {
				lines++
				if _, ok := parseQueryLogLine(l); ok {
					entries++
				}
			}
		}
		ql := &QueryLog{Paths: []string{path}, Now: func() time.Time { return now }}
		qs, err := ql.Queries(context.Background(), time.Time{})
		if lines > 0 && entries == 0 {
			if err == nil {
				t.Fatalf("garbage read as an empty query log: %d queries", len(qs))
			}
			return
		}
		if err != nil {
			t.Fatalf("Queries: %v", err)
		}
		for _, q := range qs {
			if q.Expr == "" || q.At.IsZero() {
				t.Errorf("query without text or time: %+v", q)
			}
		}
		cov, err := ql.Coverage(context.Background(), now.Add(-30*24*time.Hour))
		if err != nil || math.IsNaN(cov) || cov < 0 || cov > 1 {
			t.Errorf("Coverage = %v, %v", cov, err)
		}
	})
}
