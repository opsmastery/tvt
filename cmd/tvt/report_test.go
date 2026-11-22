package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
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

	// Evidence kept: the top-50 report tallies the full cut per service,
	// and every ground-truth hard keep and dormant_confirm metric is kept.
	var cut, kept, protected int
	for _, svc := range r.Services {
		cut += svc.Cut.Signals
		kept += svc.HardKeep.Signals + svc.KeepHigh.Signals + svc.Dormant.Signals
	}
	for _, s := range g.signals {
		if s.Kind != "label" && (s.Class == string(score.ClassHardKeep) || s.Class == string(score.ClassKeepHigh) || s.Class == string(score.ClassDormant)) {
			protected++
		}
	}
	if cut != len(all.Recommendations) || kept != protected || protected == 0 {
		t.Errorf("services: cut %d of %d recommendations, kept %d of %d protected", cut, len(all.Recommendations), kept, protected)
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

// A season the lab window (Oct 10 - Nov 9) misses: the shop's unread
// signals are dormant, never recommended, and the header lists the gap.
func TestReportLabSeasons(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	dir := t.TempDir()
	db := filepath.Join(dir, "tvt.db")
	o := labOptions(l, db)
	o.Config = filepath.Join(dir, "tvt.yaml")
	body := fmt.Sprintf(`pricecard: grafana-cloud
slos:
  paths: [%q]
postmortems:
  paths: [%q]
runbooks:
  fetch: false
  mirrors:
    - url_prefix: https://github.com/Opsmastery-Summit/tvt-lab/blob/main/
      path: %q
ownership:
  owners:
    - name: shop
      metrics: ["shop_feature_flag_.*"]
seasons:
  - name: spring sale
    months: [3, 4]
    services: [shop]
`, filepath.Join(lab, "slos"), filepath.Join(lab, "postmortems"), lab)
	if err := os.WriteFile(o.Config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	if err := runReport(context.Background(), reportOptions{DB: db, JSON: filepath.Join(dir, "r.json"), Top: 0}, &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "r.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r report.Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	var gap bool
	for _, g := range r.Gaps {
		gap = gap || g.Kind == report.GapSeason && strings.Contains(g.Text, "spring sale: runs in Mar, Apr")
	}
	if !gap {
		t.Errorf("no season gap in %+v", r.Gaps)
	}
	for _, rec := range r.Recommendations {
		if strings.HasPrefix(rec.Signal.Name, "shop_feature_flag_") || strings.HasPrefix(rec.Signal.Parent, "shop_feature_flag_") {
			t.Errorf("%s recommended despite its missed season", rec.ID)
		}
	}
	var confirm bool
	for _, c := range r.Confirm {
		if c.Signal.ID == "prom:metric:shop_feature_flag_evaluations_total" {
			confirm = c.Reason == "seasonal reader: spring sale falls outside the window" && c.Owner == "shop"
		}
	}
	if !confirm {
		t.Error("shop_feature_flag_evaluations_total isn't a seasonal confirm row")
	}
}

// S4 item 8: tvt report --price-card on the grafana-cloud lab scan gives
// the report a self-hosted scan gives, row for row: same recommendations,
// savings, disk and priorities. Only the what-if marker differs.
func TestReportPriceCardMatchesRescan(t *testing.T) {
	setNow(t)
	dir := t.TempDir()
	scanReport := func(config, card string) report.Report {
		t.Helper()
		l := newLab(t, nil)
		db := filepath.Join(dir, config+".db")
		o := labOptions(l, db)
		o.Config = filepath.Join(lab, config)
		var out bytes.Buffer
		if err := runScan(context.Background(), o, env, &out); err != nil {
			t.Fatalf("scan %s: %v\n%s", config, err, out.String())
		}
		out.Reset()
		if err := runReport(context.Background(), reportOptions{DB: db, JSON: "-", PriceCard: card}, &out); err != nil {
			t.Fatal(err)
		}
		var r report.Report
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	selfCard := filepath.Join(lab, "pricecard-selfhosted.yaml")
	whatIf := scanReport("tvt-lab.yaml", selfCard)
	rescan := scanReport("tvt-lab-selfhosted.yaml", "")
	vendor := scanReport("tvt-lab.yaml", "")
	if whatIf.Scan.RepricedFrom != vendor.Scan.PriceCard || whatIf.Scan.PriceCard != rescan.Scan.PriceCard || rescan.Scan.RepricedFrom != "" {
		t.Errorf("scan: what-if %+v, rescan %+v", whatIf.Scan, rescan.Scan)
	}
	if len(whatIf.Recommendations) == 0 || len(whatIf.Recommendations) != len(rescan.Recommendations) {
		t.Fatalf("%d what-if recommendations, %d rescanned", len(whatIf.Recommendations), len(rescan.Recommendations))
	}
	vendorSaving := map[string]float64{}
	for _, v := range vendor.Recommendations {
		vendorSaving[v.ID] = v.Savings.Monthly
	}
	var moved bool
	for i, w := range whatIf.Recommendations {
		r := rescan.Recommendations[i]
		if w.ID != r.ID || w.Savings.DiskBytes != r.Savings.DiskBytes || math.Abs(w.Savings.Monthly-r.Savings.Monthly) > 1e-9 ||
			math.Abs(w.Score.Priority-r.Score.Priority) > 1e-9 || w.Savings.Currency != r.Savings.Currency {
			t.Errorf("row %d: what-if %s %+v %+v, rescan %s %+v %+v", i, w.ID, w.Savings, w.Score, r.ID, r.Savings, r.Score)
		}
		moved = moved || w.Savings.Monthly != vendorSaving[w.ID]
	}
	if !moved {
		t.Error("repricing changed no saving")
	}
	if math.Abs(whatIf.Totals.MetricSavings-rescan.Totals.MetricSavings) > 1e-6 {
		t.Errorf("totals: what-if %v, rescan %v", whatIf.Totals.MetricSavings, rescan.Totals.MetricSavings)
	}

	// The page says it's a what-if; a card that doesn't load fails.
	var page bytes.Buffer
	b, _ := json.Marshal(whatIf)
	if err := report.RenderHTMLFromJSON(&page, b); err != nil || !strings.Contains(page.String(), "What-if pricing") {
		t.Errorf("HTML what-if banner missing: %v", err)
	}
	if err := runReport(context.Background(), reportOptions{DB: filepath.Join(dir, "tvt-lab.yaml.db"), JSON: "-", PriceCard: "no-such-preset"}, &page); err == nil {
		t.Error("an unknown price card didn't fail")
	}
}

// TestWriteFileAtomicDevice: --json /dev/null works (the temporary file
// went into /dev, which can't take one).
func TestWriteFileAtomicDevice(t *testing.T) {
	if _, err := os.Stat(os.DevNull); err != nil {
		t.Skip("no", os.DevNull)
	}
	if err := writeFileAtomic(os.DevNull, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "r.json")
	if err := writeFileAtomic(p, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "{}" {
		t.Errorf("wrote %q", b)
	}
}
