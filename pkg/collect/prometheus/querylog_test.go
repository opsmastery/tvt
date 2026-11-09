package prometheus

import (
	"compress/gzip"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueriesFixture(t *testing.T) {
	ql := &QueryLog{Paths: []string{filepath.Join(fixtures, "query.log")}}
	got, err := ql.Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// The recorded 500 lines are 430 rule evaluations and 70 API queries.
	if len(got) != 70 {
		t.Fatalf("got %d queries, want 70", len(got))
	}
	clients := map[string]int{}
	for i, q := range got {
		clients[q.Client]++
		if q.Source != "prometheus" || q.Language != "promql" || q.Expr == "" || q.At.IsZero() {
			t.Errorf("bad query %+v", q)
		}
		if i > 0 && q.At.Before(got[i-1].At) {
			t.Fatalf("not sorted at %d", i)
		}
	}
	// 10.89.0.6 is Grafana (dashboard panels); 10.89.0.9 is the viewer's
	// direct "Explore" queries.
	if clients["10.89.0.6"] != 67 || clients["10.89.0.9"] != 3 {
		t.Errorf("clients = %v", clients)
	}
}

func TestQueriesSince(t *testing.T) {
	ql := &QueryLog{Paths: []string{filepath.Join(fixtures, "query.log")}}
	since := time.Date(2026, 11, 9, 1, 40, 0, 0, time.UTC)
	got, err := ql.Queries(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) >= 70 {
		t.Fatalf("got %d queries after %s", len(got), since)
	}
	for _, q := range got {
		if q.At.Before(since) {
			t.Errorf("query at %s before since", q.At)
		}
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if strings.HasSuffix(name, ".gz") {
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		gz.Write([]byte(content))
		gz.Close()
		f.Close()
		return p
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func apiLine(at time.Time, query string) string {
	return fmt.Sprintf(`{"time":%q,"params":{"query":%q,"start":%q,"end":%q,"step":0},"httpRequest":{"clientIP":"10.0.0.1","method":"GET","path":"/api/v1/query"}}`,
		at.Format(time.RFC3339Nano), query, at.Format(time.RFC3339), at.Format(time.RFC3339))
}

func ruleLine(at time.Time) string {
	return fmt.Sprintf(`{"time":%q,"params":{"query":"up == 0"},"ruleGroup":{"file":"r.yml","name":"g"}}`,
		at.Format(time.RFC3339Nano))
}

func TestQueriesPrometheus2Format(t *testing.T) {
	dir := t.TempDir()
	// Prometheus 2.x (go-kit logger) wrote "ts" instead of "time".
	p := writeFile(t, dir, "query.log",
		`{"httpRequest":{"clientIP":"10.0.0.2","method":"POST","path":"/api/v1/query_range"},"params":{"end":"2024-05-01T10:00:00.000Z","query":"rate(http_requests_total[5m])","start":"2024-05-01T09:00:00.000Z","step":15},"stats":{},"ts":"2024-05-01T10:00:00.123Z"}`+"\n")
	got, err := (&QueryLog{Paths: []string{p}}).Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Expr != "rate(http_requests_total[5m])" || got[0].Client != "10.0.0.2" ||
		!got[0].At.Equal(time.Date(2024, 5, 1, 10, 0, 0, 123e6, time.UTC)) {
		t.Errorf("got %+v", got)
	}
}

func TestQueriesRotatedGzipAndPartialLine(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	writeFile(t, dir, "query.log.1.gz", apiLine(t0, "a")+"\n"+ruleLine(t0)+"\n")
	// The live file ends in a partial line still being written.
	writeFile(t, dir, "query.log", apiLine(t0.Add(time.Minute), "b")+"\n"+`{"time":"2026-11-09T10:02:00Z","par`)
	got, err := (&QueryLog{Paths: []string{filepath.Join(dir, "query.log*")}}).Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Expr != "a" || got[1].Expr != "b" {
		t.Errorf("got %+v", got)
	}
}

func TestQueryLogErrors(t *testing.T) {
	dir := t.TempDir()
	notLog := writeFile(t, dir, "access.log", "GET / 200\nGET /x 404\n")
	// JSON with timestamps but no query: an application log, not a query log.
	appLog := writeFile(t, dir, "app.log",
		`{"time":"2026-11-09T10:00:00Z","level":"INFO","msg":"started"}`+"\n"+
			`{"time":"2026-11-09T10:00:01Z","level":"INFO","msg":"listening"}`+"\n")
	cases := map[string]*QueryLog{
		"no paths":     {},
		"missing file": {Paths: []string{filepath.Join(dir, "nope.log")}},
		"not a log":    {Paths: []string{notLog}},
		"app log":      {Paths: []string{appLog}},
	}
	for name, ql := range cases {
		if _, err := ql.Queries(context.Background(), time.Time{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// An empty file is a valid log with no queries yet.
	empty := writeFile(t, dir, "empty.log", "")
	if got, err := (&QueryLog{Paths: []string{empty}}).Queries(context.Background(), time.Time{}); err != nil || len(got) != 0 {
		t.Errorf("empty file: %v, %v", got, err)
	}
}

func TestCoverageWithHeartbeatAndGap(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	since := now.Add(-10 * time.Hour)
	var b strings.Builder
	// Rule evaluations every 30s, except a 3-hour gap (host asleep).
	for at := since; at.Before(now); at = at.Add(30 * time.Second) {
		if at.After(since.Add(4*time.Hour)) && at.Before(since.Add(7*time.Hour)) {
			continue
		}
		b.WriteString(ruleLine(at) + "\n")
	}
	p := writeFile(t, t.TempDir(), "query.log", b.String())
	ql := &QueryLog{Paths: []string{p}, Now: func() time.Time { return now }}
	got, err := ql.Coverage(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-0.7) > 0.02 {
		t.Errorf("coverage = %.3f, want ~0.70", got)
	}
}

func TestCoverageWithoutHeartbeat(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	since := now.Add(-30 * 24 * time.Hour)
	// No rules; the log starts 6 days before now, with sparse API queries.
	start := now.Add(-6 * 24 * time.Hour)
	p := writeFile(t, t.TempDir(), "query.log",
		apiLine(start, "a")+"\n"+apiLine(now.Add(-time.Hour), "b")+"\n")
	ql := &QueryLog{Paths: []string{p}, Now: func() time.Time { return now }}
	got, err := ql.Coverage(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-0.2) > 0.001 {
		t.Errorf("coverage = %.3f, want 0.2 (6 of 30 days)", got)
	}
}

func TestCoverageLogPredatesWindowWithoutHeartbeat(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	since := now.Add(-30 * 24 * time.Hour)
	// No rules; the log started 40 days ago and has one recent query.
	p := writeFile(t, t.TempDir(), "query.log",
		apiLine(now.Add(-40*24*time.Hour), "a")+"\n"+apiLine(now.Add(-time.Hour), "b")+"\n")
	ql := &QueryLog{Paths: []string{p}, Now: func() time.Time { return now }}
	got, err := ql.Coverage(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("coverage = %.3f, want 1 (log starts before the window)", got)
	}
}

func TestCoverageHeartbeatStartsMidWindow(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	since := now.Add(-10 * time.Hour)
	// Old API lines before the window must not count as heartbeat buckets;
	// rule evaluations only cover the last 5 hours.
	var b strings.Builder
	b.WriteString(apiLine(since.Add(-24*time.Hour), "old") + "\n")
	for at := now.Add(-5 * time.Hour); at.Before(now); at = at.Add(30 * time.Second) {
		b.WriteString(ruleLine(at) + "\n")
	}
	p := writeFile(t, t.TempDir(), "query.log", b.String())
	ql := &QueryLog{Paths: []string{p}, Now: func() time.Time { return now }}
	got, err := ql.Coverage(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-0.5) > 0.02 {
		t.Errorf("coverage = %.3f, want ~0.50", got)
	}
}

func TestCoverageEdgeCases(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	p := writeFile(t, t.TempDir(), "query.log", "")
	ql := &QueryLog{Paths: []string{p}, Now: func() time.Time { return now }}
	if got, err := ql.Coverage(context.Background(), now.Add(-time.Hour)); err != nil || got != 0 {
		t.Errorf("empty log: %v, %v", got, err)
	}
	if _, err := ql.Coverage(context.Background(), now); err == nil {
		t.Error("since == now accepted")
	}
}

func TestQueriesCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ql := &QueryLog{Paths: []string{filepath.Join(fixtures, "query.log")}}
	if _, err := ql.Queries(ctx, time.Time{}); err == nil {
		t.Error("cancelled context ignored")
	}
}
