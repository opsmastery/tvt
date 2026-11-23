package loki

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opsmastery/tvt/internal/testutil"
)

// FuzzQueryLogFile: any file reads without a panic, every query it yields
// is LogQL with text and a time, oldest first, and coverage stays in [0, 1].
func FuzzQueryLogFile(f *testing.F) {
	f.Add(bytes.Join(testutil.Lines(f, "lab/loki/query.log", 20), []byte("\n")))
	for _, g := range testutil.Garbage {
		f.Add(g)
	}
	now := time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "loki.log")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		ql := &QueryLog{Paths: []string{path}, Now: func() time.Time { return now }}
		if qs, err := ql.Queries(ctx, time.Time{}); err == nil {
			for i, q := range qs {
				if q.Expr == "" || q.At.IsZero() || q.Language != "logql" {
					t.Errorf("bad query %+v", q)
				}
				if i > 0 && q.At.Before(qs[i-1].At) {
					t.Errorf("not sorted at %d", i)
				}
			}
		}
		if cov, err := ql.Coverage(ctx, now.Add(-30*24*time.Hour)); err == nil && (math.IsNaN(cov) || cov < 0 || cov > 1) {
			t.Errorf("Coverage = %v", cov)
		}
	})
}
