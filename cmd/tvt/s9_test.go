package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/check"
	"github.com/opsmastery/tvt/pkg/config"
	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/report"
)

// TestS9DoneWhenOffline is S9's done-when on the writers-2 benchmark, with
// no lab: tvt's own proposal is refused until its owners confirm, the
// confirmations clear the confirmation rule, and a new reader of one
// confirmed signal makes that confirmation stale (check refuses again) and
// reopens its row in the report. The live run on the lab (task #55) still
// opens a real draft PR.
func TestS9DoneWhenOffline(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "writers-2" {
			st = s
		}
	}
	dir := dirs[st.Name]
	db, r, _ := scanStateDB(t, st, dir)
	in, _, err := readLatest(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	in.LabelDrops = true // as scanStateDB's report
	cfg, err := config.Load(filepath.Join(dir, "tvt-lab.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const repo = "github.com/opsmastery/tvt-lab"
	checkout := filepath.Join(dir, "checkout")

	// 1. tvt propose: the patch carries .tvt/proposal.json.
	rj := filepath.Join(t.TempDir(), "report.json")
	b, _ := json.Marshal(r)
	os.WriteFile(rj, b, 0o644)
	out := t.TempDir()
	var log strings.Builder
	o := proposeOptions{DB: db, Config: filepath.Join(dir, "tvt-lab.yaml"), Report: rj, Output: "patch", Dir: out,
		Checkouts: checkoutFlag{repo: checkout}, Validate: "off"}
	if err := runPropose(context.Background(), o, &log); err != nil {
		t.Fatalf("propose: %v\n%s", err, log.String())
	}
	patch := filepath.Join(out, report.ManifestDir(repo)+".patch")
	scratch := t.TempDir()
	if err := os.CopyFS(scratch, os.DirFS(checkout)); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("git", "-C", scratch, "apply", patch).CombinedOutput(); err != nil {
		t.Fatalf("git apply: %v\n%s", err, b)
	}
	pb, err := os.ReadFile(filepath.Join(scratch, report.ProposalPath))
	if err != nil {
		t.Fatalf("the patch has no %s: %v", report.ProposalPath, err)
	}
	var proposal report.Proposal
	if err := json.Unmarshal(pb, &proposal); err != nil {
		t.Fatal(err)
	}
	var ids []string
	byPipeline := map[string]string{}
	for p, cs := range proposal.Pipelines {
		for _, c := range cs {
			ids = append(ids, c.Recommendation)
			if c.Evidence == "" {
				t.Errorf("%s has no evidence hash in the proposal", c.Recommendation)
			}
			if byPipeline[p] == "" && strings.HasPrefix(c.Recommendation, "drop:prom:metric:") {
				byPipeline[p] = c.Signal
			}
		}
	}
	if len(ids) == 0 {
		t.Fatal("the proposal carries no recommendations")
	}

	// The pipeline repo: the checkout as it stands, with its manifest.
	repoDir := t.TempDir()
	if err := os.CopyFS(repoDir, os.DirFS(checkout)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	writeManifest := func(in report.Input) {
		t.Helper()
		m := buildManifests(cfg, in)[repo]
		m.GeneratedAt = now
		m.Gaps = nil // the scan's open gaps are check's own rule, tested elsewhere
		os.MkdirAll(filepath.Join(repoDir, ".tvt"), 0o755)
		b, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(repoDir, ".tvt", "protect.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	judge := func() map[string][]check.Finding {
		t.Helper()
		res, err := runCheck(checkOptions{Manifest: ".tvt/protect.json", Repo: repoDir, Patch: patch, MaxAge: 7 * 24 * time.Hour, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		by := map[string][]check.Finding{}
		for _, f := range res.Findings {
			by[f.Rule] = append(by[f.Rule], f)
		}
		return by
	}
	allowed := func(by map[string][]check.Finding, rules ...string) {
		t.Helper()
		for rule, fs := range by {
			ok := rule == check.RuleLeastLossy // S10's less lossy actions clear these
			for _, r := range rules {
				ok = ok || rule == r
			}
			if !ok {
				t.Errorf("unexpected %s: %v", rule, fs[0])
			}
		}
	}

	// 2. Before anyone confirms: every proposed recommendation is unconfirmed.
	writeManifest(in)
	by := judge()
	if n := len(by[check.RuleUnconfirmed]); n != len(ids) {
		t.Errorf("%d unconfirmed findings, want one per proposed recommendation (%d)", n, len(ids))
	}
	allowed(by, check.RuleUnconfirmed)
	t.Logf("%d recommendations proposed, %d unconfirmed, %d held back by least_lossy", len(ids), len(by[check.RuleUnconfirmed]), len(by[check.RuleLeastLossy]))

	// 3. The owner confirms them all, at the evidence the report showed.
	var cout strings.Builder
	if err := runConfirm(confirmOptions{Report: rj, Dir: repoDir, Owner: "@jasonvance", Now: now}, ids, &cout); err != nil {
		t.Fatalf("confirm: %v\n%s", err, cout.String())
	}
	by = judge()
	if fs := append(by[check.RuleUnconfirmed], by[check.RuleStaleConfirm]...); len(fs) > 0 {
		t.Errorf("after confirming, still %d confirmation findings, first: %v", len(fs), fs[0])
	}
	allowed(by)

	// 4. A new reader of one confirmed signal: a panel on a new dashboard.
	sig := byPipeline["prometheus-scrape"]
	if sig == "" {
		t.Fatal("no Prometheus drop in the proposal")
	}
	in2 := in
	in2.Consumers = append(append([]graph.Consumer{}, in.Consumers...), graph.Consumer{ID: "panel:1:s9-reader:1:0", Kind: graph.ConsumerDashboardPanel, Name: "S9 reader / panel"})
	in2.References = append(append([]graph.Reference{}, in.References...), graph.Reference{SignalID: sig, ConsumerID: "panel:1:s9-reader:1:0", Evidence: "observed"})
	writeManifest(in2)
	by = judge()
	stale := by[check.RuleStaleConfirm]
	if len(stale) != 1 || stale[0].Signal != sig || !strings.Contains(stale[0].Reason, "panel:1:s9-reader:1:0") {
		t.Errorf("stale confirmations %v, want one for %s naming the new reader", stale, sig)
	}
	allowed(by, check.RuleStaleConfirm)
	t.Logf("a new reader of %s: %s", sig, stale[0].Reason)

	// 5. The report reopens that row and says why; the others stay confirmed.
	db2, err := os.ReadFile(filepath.Join(repoDir, report.DecisionsPath))
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := *cfg
	cfg2.Pipelines = append([]config.Pipeline{}, cfg.Pipelines...)
	for i := range cfg2.Pipelines {
		cfg2.Pipelines[i].Checkout = repoDir
	}
	if in2.Decisions, err = repoDecisions(&cfg2); err != nil || len(in2.Decisions) != 1 {
		t.Fatalf("decisions: %v %v\n%s", err, in2.Decisions, db2)
	}
	rep := report.Build(in2, 0)
	reopened, confirmed := 0, 0
	for _, d := range rep.Decisions {
		switch d.State {
		case report.DecisionReopened:
			reopened++
			if d.Signal != sig || !strings.Contains(d.Why, "evidence changed") {
				t.Errorf("reopened: %+v", d)
			}
		case report.DecisionCurrent:
			confirmed++
		}
	}
	if reopened != 1 || confirmed != len(ids)-1 {
		t.Errorf("%d reopened, %d current, want 1 and %d", reopened, confirmed, len(ids)-1)
	}
	for _, rec := range rep.Recommendations {
		if rec.Signal.ID == sig && (rec.Status != report.StatusOpen || rec.Review == "") {
			t.Errorf("the reopened row: status %s, review %q", rec.Status, rec.Review)
		}
	}
}
