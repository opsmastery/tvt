package prometheus

import (
	"compress/gzip"
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

func TestQueriesFixture(t *testing.T) {
	ql := &QueryLog{Paths: []string{filepath.Join(fixtures, "query.log")}}
	got, err := ql.Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// The recorded 500 lines (Nov 15) are 403 rule evaluations and 97 API
	// queries.
	if len(got) != 97 {
		t.Fatalf("got %d queries, want 97", len(got))
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
	// 10.89.0.17 is Grafana (dashboard panels and the Grafana-managed
	// rule); 10.89.0.19 is the viewer's direct "Explore" queries; 10.89.0.23
	// is the agent container (sweep and finance agents).
	if clients["10.89.0.17"] != 89 || clients["10.89.0.19"] != 2 || clients["10.89.0.23"] != 6 {
		t.Errorf("clients = %v", clients)
	}
}

func TestQueriesSince(t *testing.T) {
	ql := &QueryLog{Paths: []string{filepath.Join(fixtures, "query.log")}}
	since := time.Date(2026, 11, 15, 1, 48, 0, 0, time.UTC)
	got, err := ql.Queries(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) >= 97 {
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

// The marker is the scan's own query: it proves the log, never counts as
// usage, and only its own nonce counts.
func TestQueryLogProbe(t *testing.T) {
	dir := t.TempDir()
	marker := strings.ReplaceAll(collect.ProbeQuery("ab12"), `"`, `\"`)
	path := writeFile(t, dir, "query.log", strings.Join([]string{
		`{"httpRequest":{"clientIP":"10.0.0.1","path":"/api/v1/query"},"params":{"query":"` + marker + `"},"time":"2026-11-11T10:00:00Z"}`,
		`{"httpRequest":{"clientIP":"10.0.0.2","path":"/api/v1/query"},"params":{"query":"up"},"time":"2026-11-11T10:00:01Z"}`,
	}, "\n")+"\n")
	ql := &QueryLog{Paths: []string{path}}
	got, err := ql.Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Expr != "up" {
		t.Errorf("queries = %+v, want only up", got)
	}
	for nonce, want := range map[string]bool{"ab12": true, "cd34": false} {
		seen, err := ql.ProbeSeen(context.Background(), nonce)
		if err != nil || seen != want {
			t.Errorf("ProbeSeen(%s) = %v, %v; want %v", nonce, seen, err, want)
		}
	}
}

func TestProbeSendsTheTaggedMarker(t *testing.T) {
	var got *http.Request
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	c := New(srv.URL+"/prometheus", "")
	if err := c.Probe(context.Background(), "ab12"); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.URL.Path != "/prometheus/api/v1/query" || got.URL.Query().Get("query") != collect.ProbeQuery("ab12") ||
		got.Header.Get("X-Query-Tags") != collect.ProbeTag {
		t.Errorf("probe request = %+v", got)
	}
}

// TestRuleEvaluations reads the ruler's own entries (S6 item 1): group,
// query and queryable samples, never returned as usage; API queries carry
// their samples and whether they were instant.
func TestRuleEvaluations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "query.log")
	lines := `{"time":"2026-11-15T20:30:51Z","params":{"end":"2026-11-15T20:30:51.782Z","query":"shop_payment_queue_depth > 100","start":"2026-11-15T20:30:51.782Z","step":0},"stats":{"samples":{"totalQueryableSamples":4,"peakSamples":4}},"ruleGroup":{"file":"/etc/prometheus/rules/shop.yml","name":"shop-alerts"}}
{"time":"2026-11-15T20:30:52Z","params":{"end":"2026-11-15T20:30:52Z","query":"max(shop_order_export_lag_seconds)","start":"2026-11-15T20:30:52Z","step":0},"stats":{"samples":{"totalQueryableSamples":1}},"httpRequest":{"clientIP":"10.89.0.29","method":"POST","path":"/api/v1/query"}}
{"time":"2026-11-15T20:30:53Z","params":{"end":"2026-11-15T20:30:53Z","query":"rate(x[5m])","start":"2026-11-15T14:30:53Z","step":60},"stats":{"samples":{"totalQueryableSamples":7200}},"httpRequest":{"clientIP":"10.89.0.29","path":"/api/v1/query_range"}}
{"time":"2026-11-15T20:30:54Z","params":{"query":"up"},"ruleGroup":{"name":"no-stats"}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	ql := &QueryLog{Paths: []string{path}}
	evals, err := ql.RuleEvaluations(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evals) != 2 || evals[0].Group != "shop-alerts" || evals[0].Samples != 4 || evals[0].Expr != "shop_payment_queue_depth > 100" || evals[1].Samples != 0 {
		t.Errorf("evaluations = %+v", evals)
	}
	qs, err := ql.Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 || !qs[0].Instant || qs[0].Samples != 1 || qs[1].Instant || qs[1].Samples != 7200 {
		t.Errorf("queries = %+v", qs)
	}
}

// TestRuleEvaluationsFixture: the lab's ruler logs every group's evaluations.
func TestRuleEvaluationsFixture(t *testing.T) {
	evals, err := (&QueryLog{Paths: []string{filepath.Join(fixtures, "query.log")}}).RuleEvaluations(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]int{}
	for _, e := range evals {
		groups[e.Group]++
		if e.Samples < 0 {
			t.Errorf("negative samples %+v", e)
		}
	}
	if groups["shop-alerts"] == 0 || groups["shop-recording"] == 0 {
		t.Errorf("groups = %v", groups)
	}
}
