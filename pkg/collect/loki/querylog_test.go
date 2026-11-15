package loki

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
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
