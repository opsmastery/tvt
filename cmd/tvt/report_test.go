package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
)

// TestReportLab: the JSON report of a lab scan recommends only candidates,
// lists its gaps on every row, and recommends every ground-truth drop that
// is a candidate.
func TestReportLab(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	dir := t.TempDir()
	db := filepath.Join(dir, "tvt.db")
	var out bytes.Buffer
	if err := runScan(context.Background(), labOptions(l, db), env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	g := readGraph(t, db)
	gt := loadGroundTruth(t)

	read := func(top int) report.Report {
		t.Helper()
		path := filepath.Join(dir, "report.json")
		out.Reset()
		if err := runReport(context.Background(), reportOptions{DB: db, JSON: path, Top: top}, &out); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var r report.Report
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	r := read(50)
	t.Log(out.String())
	if r.Version != report.Version || len(r.Recommendations) != 50 || !strings.Contains(out.String(), "50 recommendations") {
		t.Fatalf("top 50: version %d, %d recommendations\n%s", r.Version, len(r.Recommendations), out.String())
	}
	if len(r.Gaps) == 0 {
		t.Error("the lab fixture has gaps (no loki query log) but the report lists none")
	}

	all := read(0)
	recommended := map[string]bool{}
	for _, rec := range all.Recommendations {
		recommended[rec.Signal.ID] = true
		s, ok := g.signals[rec.Signal.ID]
		if !ok || s.Class != string(score.ClassCandidate) {
			t.Errorf("%s (%s) recommended", rec.Signal.ID, s.Class)
		}
		if rec.Claim == "" || rec.CheapestCheck == "" || rec.CheckMinutes <= 0 || rec.Evidence.Gaps == nil || rec.Status != report.StatusOpen {
			t.Errorf("incomplete row %+v", rec)
		}
		if len(rec.Evidence.Gaps) == 0 {
			t.Errorf("%s lists no gaps though the fixture's Grafana log is partial", rec.ID)
		}
		if e, ok := gt.Metrics[rec.Signal.Name]; ok && rec.Signal.Kind == "metric" && (e.Class == "hard_keep" || e.Class == "keep_high") {
			t.Errorf("ground-truth %s %s recommended", e.Class, rec.Signal.Name)
		}
	}
	var drops int
	for name, e := range gt.Metrics {
		id := "prom:metric:" + name
		if e.Class != "drop_metric" || strings.Contains(name, "*") || g.signals[id].Class != string(score.ClassCandidate) {
			continue
		}
		drops++
		if !recommended[id] {
			t.Errorf("ground-truth drop %s isn't recommended", name)
		}
	}
	if drops == 0 {
		t.Error("no ground-truth drop is a candidate")
	}

	if err := runReport(context.Background(), reportOptions{DB: filepath.Join(dir, "missing.db"), JSON: "-"}, &out); err == nil {
		t.Error("a missing database didn't fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); err == nil {
		t.Error("report created a database")
	}
}
