package loki

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/internal/testutil"
	"github.com/opsmastery/tvt/pkg/collect"
)

// The lab fixture: two hours of metrics.go lines. The viewer's panels 6
// and 8 (limited and metric queries) every round and the on-call /healthz
// query every third round are usage; querier and ruler lines aren't.
func TestLokiQueryLogFixture(t *testing.T) {
	ql := &QueryLog{Paths: []string{filepath.Join(testutil.Lab, "loki", "query.log")}}
	got, err := ql.Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	byExpr := map[string]int{}
	for i, q := range got {
		if q.Source != "loki" || q.Language != "logql" || q.At.IsZero() {
			t.Errorf("bad query %+v", q)
		}
		if i > 0 && q.At.Before(got[i-1].At) {
			t.Fatalf("not sorted at %d", i)
		}
		byExpr[q.Expr]++
	}
	if len(got) != 54 || len(byExpr) != 3 {
		t.Errorf("got %d queries, %d distinct: %v", len(got), len(byExpr), byExpr)
	}
	// Since tvt-lab's route_cold fix, panel 8 and the on-call query name
	// the levels they read, so neither reads the debug stream.
	for _, e := range []string{
		`{app="shop", level=~"info|warn|error"}`,
		`sum by (level) (rate({app="shop", component="api", level=~"info|warn|error"}[1m]))`,
		`{app="shop", component="api", level=~"warn|error"} |= "route=/healthz" != "status=200"`,
	} {
		if byExpr[e] == 0 {
			t.Errorf("missing %s", e)
		}
	}
	last := got[len(got)-1].At
	cov, err := (&QueryLog{Paths: ql.Paths, Now: func() time.Time { return last }}).Coverage(context.Background(), last.Add(-time.Hour))
	if err != nil || cov < 0.99 {
		t.Errorf("Coverage over the last hour = %v, %v; want 1 (ruler heartbeat every minute)", cov, err)
	}
}

func TestLokiQueryLogFilters(t *testing.T) {
	line := func(extra string) string {
		return `level=info ts=2026-11-11T10:00:00Z caller=metrics.go:237 ` + extra
	}
	content := strings.Join([]string{
		line(`component=frontend query="{a=\"tvt\"}" query_type=limited source=tvt`),
		line(`component=frontend query="{a=\"probe\"}" query_type=limited source=tvt-probe`),
		line(`component=querier query="{a=\"sub\"}" query_type=limited`),
		line(`component=ruler query="sum(rate({a=\"r\"}[1m]))" query_type=metric`),
		line(`component=frontend query="{a=\"vol\"}" query_type=volume`),
		`level=info ts=2026-11-11T10:00:00Z caller=other.go:1 component=frontend query="{a=\"x\"}" query_type=limited`,
		`2026-11-11T10:00:01Z stderr F ` + line(`component=frontend query="{a=\"user\"} |= \"x\"" query_type=filter source=grafana-explore`),
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "loki.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (&QueryLog{Paths: []string{path}}).Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Expr != `{a="user"} |= "x"` || got[0].Client != "grafana-explore" {
		t.Errorf("queries = %+v, want only the user query", got)
	}
}

// The probe (S5 item 5): Loki logs the marker as a frontend metric query,
// as the lab's Loki 3.5 did on Nov 15 (line trimmed). ProbeSeen finds it
// only from the frontend and only for its own nonce, and Queries drops it
// even when a proxy stripped the X-Query-Tags header.
func TestLokiQueryLogProbe(t *testing.T) {
	marker := `count_over_time({tvt_query_log_probe=\"feedc0de12345678\"}[1m])`
	content := strings.Join([]string{
		`2026-11-14T22:21:28.711827152-07:00 stderr F level=info ts=2026-11-15T05:21:28.676763132Z caller=metrics.go:409 org_id=fake latency=fast query_type=stats query="{tvt_query_log_probe=\"feedc0de12345678\"}" source=`,
		`2026-11-14T22:21:28.711827152-07:00 stderr F level=info ts=2026-11-15T05:21:28.67913801Z caller=metrics.go:237 component=querier org_id=fake latency=fast query="` + marker + `" query_type=metric range_type=instant status=200 source=tvt-probe`,
		`2026-11-14T22:21:28.711827152-07:00 stderr F level=info ts=2026-11-15T05:21:28.679709516Z caller=metrics.go:237 component=frontend org_id=fake latency=fast query="` + marker + `" query_type=metric range_type=instant status=200 source=tvt-probe`,
		`level=info ts=2026-11-15T05:22:00Z caller=metrics.go:237 component=frontend query="count_over_time({tvt_query_log_probe=\"0000000000000001\"}[1m])" query_type=metric`,
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "loki.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ql := &QueryLog{Paths: []string{path}}
	ctx := context.Background()
	for nonce, want := range map[string]bool{"feedc0de12345678": true, "0000000000000001": true, "0f1e2d3c4b5a6978": false} {
		if seen, err := ql.ProbeSeen(ctx, nonce); err != nil || seen != want {
			t.Errorf("ProbeSeen(%s) = %v, %v; want %v", nonce, seen, err, want)
		}
	}
	// Only the querier saw this one: not proof the frontend logs.
	querierOnly := filepath.Join(t.TempDir(), "loki.log")
	os.WriteFile(querierOnly, []byte(strings.Split(content, "\n")[1]+"\n"), 0o600)
	if seen, err := (&QueryLog{Paths: []string{querierOnly}}).ProbeSeen(ctx, "feedc0de12345678"); err != nil || seen {
		t.Errorf("querier-only marker: ProbeSeen = %v, %v; want false", seen, err)
	}
	got, err := ql.Queries(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("queries = %+v, want none: every marker is tvt's", got)
	}
}

func TestLokiProbeSendsTheTaggedMarker(t *testing.T) {
	var got *http.Request
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	if err := New(srv.URL, "").Probe(context.Background(), "ab12"); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.URL.Path != "/loki/api/v1/query" || got.URL.Query().Get("query") != collect.LokiProbeQuery("ab12") ||
		got.Header.Get("X-Query-Tags") != collect.ProbeTag {
		t.Errorf("probe request = %+v", got)
	}
}

// TestLokiRuleEvaluations: component=ruler lines are the ruler's own
// evaluations with their bytes (S6 item 1); frontend lines carry bytes and
// whether they were instant.
func TestLokiRuleEvaluations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loki.log")
	lines := `level=info ts=2026-11-15T20:52:25Z caller=metrics.go:237 component=ruler evaluation_mode=local org_id=fake query="sum(count_over_time({app=\"shop\"} |= \"x\" | logfmt [5m]))" query_type=metric range_type=instant total_bytes=6.9kB
level=info ts=2026-11-15T20:52:26Z caller=metrics.go:237 component=frontend org_id=fake query="sum(count_over_time({app=\"shop\"} | logfmt [5m]))" query_type=metric range_type=instant total_bytes=1.2MB
level=info ts=2026-11-15T20:52:27Z caller=metrics.go:237 component=frontend org_id=fake query="{app=\"shop\"}" query_type=limited range_type=range total_bytes=552B
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	ql := &QueryLog{Paths: []string{path}}
	evals, err := ql.RuleEvaluations(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evals) != 1 || evals[0].Bytes != 6900 || evals[0].Source != "loki" {
		t.Errorf("evaluations = %+v", evals)
	}
	qs, err := ql.Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 || !qs[0].Instant || qs[0].Bytes != 1200000 || qs[1].Instant || qs[1].Bytes != 552 {
		t.Errorf("queries = %+v", qs)
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"552B": 552, "6.9kB": 6900, "3.2MB": 3200000, "1GB": 1e9, "0B": 0, "": 0, "x": 0, "-1kB": 0, "NaNMB": 0, "1e30EB": 0, "12": 0} {
		if got := ParseBytes(in); got != want {
			t.Errorf("ParseBytes(%q) = %d, want %d", in, got, want)
		}
	}
}
