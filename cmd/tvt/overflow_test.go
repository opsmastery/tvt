package main

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
)

// TestOverflow (S6 item 5) scans the overflow state and checks the
// findings against ground truth's overflow: exactly the listed metrics,
// each with its overflow series count and the capped labels, and the
// viewer's customer lookup as the one reader filtering on a capped label.
// The report marks rows on those metrics as a cost floor.
func TestOverflow(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "overflow" {
			st = s
		}
	}
	if st.Name == "" {
		t.Fatal("no overflow state in benchmark.yaml")
	}
	_, _, gt := scanState(t, st, dirs[st.Name])
	want := gt.Overflow
	if len(want.Metrics) == 0 || len(want.ReadersFiltering) == 0 {
		t.Fatal("ground truth has no overflow")
	}
	db := benchDB(t, st, dirs[st.Name])
	ctx := context.Background()
	s, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := s.OverflowFindings(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	in, _, err := loadReport(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	// Each reader's query text, from tvt why's readers.
	query := map[string]string{}
	for _, f := range findings {
		w, err := report.Explain(in, f.Signal)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range w.Readers {
			query[r.Consumer] = r.Expr
		}
	}

	got := map[string]graph.OverflowFinding{}
	for _, f := range findings {
		got[strings.TrimPrefix(f.Signal, "prom:metric:")] = f
	}
	if g, w := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want.Metrics)); !slices.Equal(g, w) {
		t.Fatalf("overflow metrics %v, want %v", g, w)
	}
	for name, w := range want.Metrics {
		f := got[name]
		if f.Series != w.Series {
			t.Errorf("%s: %d overflow series, want %d", name, f.Series, w.Series)
		}
		if !slices.Equal(f.Capped, want.Capped) {
			t.Errorf("%s: capped %v, want %v", name, f.Capped, want.Capped)
		}
	}
	// Readers: exactly ground truth's, matched by the consumer's query.
	for name := range want.Metrics {
		var wantReaders []string
		for _, r := range want.ReadersFiltering {
			if r.Metric == name {
				wantReaders = append(wantReaders, r.Query+"\t"+strings.Join(r.Labels, ","))
			}
		}
		var gotReaders []string
		for _, r := range got[name].Readers {
			c, ls, _ := strings.Cut(r, "\t")
			gotReaders = append(gotReaders, query[c]+"\t"+ls)
		}
		slices.Sort(wantReaders)
		slices.Sort(gotReaders)
		if !slices.Equal(gotReaders, wantReaders) {
			t.Errorf("%s: readers on capped labels %q, want %q", name, gotReaders, wantReaders)
		}
	}

	// The report: one overflow row per metric; every row on one is a floor.
	r := report.Build(in, 0)
	if r.Totals.Overflow != len(want.Metrics) {
		t.Errorf("report overflow total %d, want %d", r.Totals.Overflow, len(want.Metrics))
	}
	for _, rec := range r.Recommendations {
		name := strings.TrimPrefix(rec.Signal.ID, "prom:metric:")
		if _, ok := want.Metrics[name]; ok != rec.CostFloor {
			t.Errorf("%s: cost floor %v", rec.Signal.ID, rec.CostFloor)
		}
	}
	for _, c := range r.Confirm {
		name := strings.TrimPrefix(c.Signal.ID, "prom:metric:")
		if _, ok := want.Metrics[name]; ok != c.CostFloor {
			t.Errorf("confirm %s: cost floor %v", c.Signal.ID, c.CostFloor)
		}
	}
	var src graph.SourceCoverage
	for _, sc := range in.Sources {
		if sc.Source == scan.SourceOverflow {
			src = sc
		}
	}
	if !src.Configured || src.Coverage != 1 || strings.Count(src.Gap, "overflow series") != len(want.Metrics) {
		t.Errorf("overflow source %+v", src)
	}
	t.Logf("overflow source gap:\n%s", src.Gap)
}
