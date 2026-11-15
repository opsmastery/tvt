package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// TestSLOCoverage (S6 item 3) scans the slo-coverage state and checks the
// report's SLO coverage against ground truth's slo_coverage: each list is
// exact, so anything else tvt lists is wrong. The burn-rate alert pages
// but isn't listed, and its 1h window isn't a long_range_window finding.
func TestSLOCoverage(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "slo-coverage" {
			st = s
		}
	}
	if st.Name == "" {
		t.Fatal("no slo-coverage state in benchmark.yaml")
	}
	_, _, gt := scanState(t, st, dirs[st.Name])
	want := gt.SLOCoverage
	if len(want.Services) == 0 || len(want.BurnRate) == 0 {
		t.Fatal("ground truth has no slo_coverage")
	}
	db := benchDB(t, st, dirs[st.Name])
	in, _, err := loadReport(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	c := report.Build(in, 50).SLOCoverage
	names := func(rows []report.SLOFinding) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Name)
			if r.Detail == "" || r.Fix == "" {
				t.Errorf("%s: detail %q, fix %q", r.Name, r.Detail, r.Fix)
			}
		}
		return slices.Sorted(slices.Values(out))
	}
	check := func(what string, got []report.SLOFinding, want []string) {
		t.Helper()
		if g, w := names(got), slices.Sorted(slices.Values(want)); !slices.Equal(g, w) {
			t.Errorf("%s: %v, want %v", what, g, w)
		}
		for _, r := range got {
			t.Logf("%s: %s: %s", what, r.Name, r.Detail)
		}
	}
	check("services without an SLO", c.ServicesWithoutSLO, want.ServicesWithoutSLO)
	check("paging alerts not on burn rate", c.PagingNotBurnRate, want.PagingNotBurnRate)
	check("SLOs without an alert", c.SLOsWithoutAlert, want.SLOsWithoutAlert)
	for _, r := range c.PagingNotBurnRate {
		if slices.Contains(want.BurnRate, r.Name) {
			t.Errorf("burn-rate alert %s listed as not on burn rate", r.Name)
		}
	}
	_, findings := ruleResults(t, db)
	for _, f := range findings {
		for _, b := range want.BurnRate {
			if f.Kind == graph.RuleLongRange && strings.HasSuffix(f.ConsumerID, "/"+b) {
				t.Errorf("burn-rate alert %s: %s", b, f.Detail)
			}
		}
	}
}
