package mimir

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

func labQueryLog() *QueryLog {
	return &QueryLog{Paths: []string{filepath.Join(testutil.Lab, "mimir", "query.log")},
		Now: func() time.Time { return time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC) }}
}

// The lab fixture: 496 query stats lines, nearly all cardinality calls
// (tvt and curl), two Grafana panel queries through the datasource proxy
// with dashboard headers, and one tvt probe.
func TestMimirQueryLogFixture(t *testing.T) {
	got, err := labQueryLog().Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d queries, want 2: %+v", len(got), got)
	}
	want := []collect.LoggedQuery{
		{Source: "mimir", Expr: "sum by (op) (rate(shop_cart_operations_total[5m]))", Language: "promql",
			Actor: "admin", Client: "Grafana/12.1.0", DashboardUID: "shop-overview", PanelID: 4, OrgID: 1},
		{Source: "mimir", Expr: "sum by (mode) (rate(node_cpu_seconds_total[1m]))", Language: "promql",
			Actor: "admin", Client: "Grafana/12.1.0", DashboardUID: "shop-overview", PanelID: 5, OrgID: 1},
	}
	for i := range want {
		g := got[i]
		if g.At.IsZero() {
			t.Errorf("query %d has no time", i)
		}
		g.At = time.Time{}
		if g != want[i] {
			t.Errorf("query %d\n got  %+v\n want %+v", i, g, want[i])
		}
	}
	seen, err := labQueryLog().ProbeSeen(context.Background(), "0f1e2d3c4b5a6978")
	if err != nil || !seen {
		t.Errorf("ProbeSeen = %v, %v", seen, err)
	}
	if cov, err := labQueryLog().Coverage(context.Background(), time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)); err != nil || cov <= 0 || cov > 1 {
		t.Errorf("Coverage = %v, %v", cov, err)
	}
}

// Remote-ruler evaluations, tvt's own requests, metadata calls and other
// components don't count; CRI container lines are unwrapped.
func TestMimirQueryLogFilters(t *testing.T) {
	line := func(extra string) string {
		return `ts=2026-11-11T10:00:00Z caller=handler.go:385 level=info user=anonymous msg="query stats" component=query-frontend method=GET ` + extra
	}
	content := strings.Join([]string{
		line(`path=/prometheus/api/v1/query user_agent=mimir/2.16.0 param_query="rule_expr"`),
		line(`path=/prometheus/api/v1/query user_agent=tvt/v1 param_query="tvt_self"`),
		line(`path=/prometheus/api/v1/query user_agent=curl/8 header_x_query_tags="Source=tvt" param_query="tagged"`),
		line(`path=/prometheus/api/v1/series user_agent=curl/8 param_query="series_call"`),
		`ts=2026-11-11T10:00:00Z caller=x.go:1 level=info msg="query stats" component=querier path=/prometheus/api/v1/query param_query="querier"`,
		`2026-11-11T10:00:01.5+00:00 stderr F ` + line(`path=/prometheus/api/v1/query_range user_agent=python param_query="up{job=\"x\"}"`),
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "mimir.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (&QueryLog{Paths: []string{path}}).Queries(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Expr != `up{job="x"}` || got[0].Client != "python" || got[0].DashboardUID != "" {
		t.Errorf("queries = %+v, want only the python query", got)
	}
}

func TestMimirQueryLogNotAMimirLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.log")
	if err := os.WriteFile(path, []byte(`{"json":"line"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&QueryLog{Paths: []string{path}}).Queries(context.Background(), time.Time{}); err == nil {
		t.Error("a non-Mimir file was accepted")
	}
}
