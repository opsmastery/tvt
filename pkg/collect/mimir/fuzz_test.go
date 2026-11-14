package mimir

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

// FuzzQueryLogFile: any file reads without a panic, every query it yields
// has text and a time, and coverage stays in [0, 1].
func FuzzQueryLogFile(f *testing.F) {
	var seed [][]byte
	for _, l := range bytes.Split(testutil.ReadFile(f, "lab/mimir/query.log"), []byte("\n")) {
		if bytes.Contains(l, []byte("param_query")) && len(seed) < 10 {
			seed = append(seed, l)
		}
	}
	f.Add(bytes.Join(seed, []byte("\n")))
	f.Add(bytes.Join(testutil.Lines(f, "lab/mimir/query.log", 10), []byte("\n")))
	for _, g := range testutil.Garbage {
		f.Add(g)
	}
	now := time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "mimir.log")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		ql := &QueryLog{Paths: []string{path}, Now: func() time.Time { return now }}
		if qs, err := ql.Queries(ctx, time.Time{}); err == nil {
			for _, q := range qs {
				if q.Expr == "" || q.At.IsZero() {
					t.Errorf("query without text or time: %+v", q)
				}
			}
		}
		if cov, err := ql.Coverage(ctx, now.Add(-30*24*time.Hour)); err == nil && (math.IsNaN(cov) || cov < 0 || cov > 1) {
			t.Errorf("Coverage = %v", cov)
		}
		_, _ = ql.ProbeSeen(ctx, "0123456789abcdef")
	})
}
