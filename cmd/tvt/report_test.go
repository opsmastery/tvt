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
		if err := runReport(context.Background(), reportOptions{DB: db, JSON: path, HTML: filepath.Join(dir, "report.html"), Top: top}, &out); err != nil {
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
	page, err := os.ReadFile(filepath.Join(dir, "report.html"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(page), `<tr class="rec"`); n != 50 {
		t.Errorf("HTML has %d rows, want 50", n)
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

	// Dormant: confirm. Every ground-truth dormant_confirm metric is a
	// question for its owner and never a recommendation; every row is a
	// dormant signal.
	confirm := map[string]report.Confirm{}
	for _, c := range all.Confirm {
		confirm[c.Signal.ID] = c
		if g.signals[c.Signal.ID].Class != string(score.ClassDormant) || c.Question == "" || c.Reason == "" {
			t.Errorf("confirm row %+v", c)
		}
	}
	var dormant int
	for name, e := range gt.Metrics {
		if e.Class != "dormant_confirm" {
			continue
		}
		dormant++
		id := "prom:metric:" + name
		if _, ok := confirm[id]; !ok {
			t.Errorf("ground-truth dormant_confirm %s has no confirm row", name)
		}
		if recommended[id] {
			t.Errorf("dormant %s recommended", name)
		}
	}
	if dormant < 23 || !strings.Contains(confirm["prom:metric:node_context_switches_total"].Reason, "postmortem") ||
		!strings.Contains(confirm["prom:metric:node_load15"].Reason, "runbook") {
		t.Errorf("%d dormant_confirm; mention reasons %q, %q", dormant,
			confirm["prom:metric:node_context_switches_total"].Reason, confirm["prom:metric:node_load15"].Reason)
	}

	// Acknowledging the Prometheus query log's partial coverage takes it
	// off every row; a stale acknowledgement is reported.
	cfg := filepath.Join(dir, "tvt.yaml")
	if err := os.WriteFile(cfg, []byte("gaps:\n  acknowledge:\n    - source: prometheus query log\n      reason: the lab rotates its query log\n    - source: slo definitions\n      match: nothing like this\n      reason: stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runReport(context.Background(), reportOptions{DB: db, Config: cfg, JSON: filepath.Join(dir, "acked.json"), Top: 0}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(1 acknowledged), 1 acknowledgements match no gap") {
		t.Errorf("output: %s", out.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "acked.json"))
	if err != nil {
		t.Fatal(err)
	}
	var acked report.Report
	if err := json.Unmarshal(b, &acked); err != nil {
		t.Fatal(err)
	}
	for _, rec := range acked.Recommendations {
		for _, g := range rec.Evidence.Gaps {
			if strings.HasPrefix(g, "prometheus query log") {
				t.Fatalf("%s still shows the acknowledged gap %q", rec.ID, g)
			}
		}
		if rec.Signal.Source == "prom" && len(rec.Evidence.Acknowledged) == 0 {
			t.Fatalf("%s doesn't list the acknowledged gap", rec.ID)
		}
	}

	if err := runReport(context.Background(), reportOptions{DB: filepath.Join(dir, "missing.db"), JSON: "-"}, &out); err == nil {
		t.Error("a missing database didn't fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); err == nil {
		t.Error("report created a database")
	}
}
