package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
)

// TestWriters2 scores the writer seeds of tvt-lab #35 and #36 on the
// writers-2 state: label rules hold the labels they set, and tvt propose
// writes a change into each pipeline kind the lab runs (Prometheus drops,
// Alloy drops, the clean labeldrop and the worker stream's reroute, the
// Fluent Bit reroute and the collector filter), all in one patch. The
// collector's delta-counter aggregation doesn't come from this scan: nothing
// reads the agent's counters, so tvt drops them whole and raises no label
// removal under them (TestProposeDeltaLabels covers it from a reviewed
// report).
func TestWriters2(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "writers-2" {
			st = s
		}
	}
	if st.Name == "" {
		t.Fatal("no writers-2 state in benchmark.yaml")
	}
	dir := dirs[st.Name]
	db, r, _ := scanStateDB(t, st, dir)
	in, _, err := readLatest(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}

	// Label rules (ground truth rewrites.label_rules): held, metrics mapped.
	heldInstancePod, heldEnv := 0, 0
	for _, pt := range in.PipelineTargets {
		rest, ok := strings.CutPrefix(pt.Subject, "prom:label:")
		if !ok {
			continue
		}
		held := strings.HasPrefix(pt.Note, scan.PipelineUnfollowable)
		switch {
		case strings.HasSuffix(rest, ":instance_pod"):
			if !held || !strings.Contains(pt.Note, "instance_pod") {
				t.Errorf("%s: %+v, want held by the Alloy rule", pt.Subject, pt)
			}
			heldInstancePod++
		case strings.HasSuffix(rest, ":deployment_environment"):
			if !held || !strings.Contains(pt.Note, "attributes/env") {
				t.Errorf("%s: %+v, want held by attributes/env", pt.Subject, pt)
			}
			heldEnv++
		case held && strings.Contains(pt.Note, "sets label"):
			t.Errorf("%s held by a label rule that doesn't set it: %s", pt.Subject, pt.Note)
		}
	}
	t.Logf("label rules hold %d instance_pod and %d deployment_environment labels", heldInstancePod, heldEnv)

	// The recommendations each writer needs.
	want := map[string]func(report.Recommendation) bool{
		"prom:label:shop_cart_operations_total:op_kind": func(rec report.Recommendation) bool {
			return rec.Action == report.ActionLabelRemoval && rec.Removal == "labeldrop" && rec.Proposable && rec.Pipeline == "alloy-apps"
		},
		`loki:stream:{app="shop", component="worker", level="debug"}`: func(rec report.Recommendation) bool {
			return rec.Action == report.ActionReroute && rec.Proposable && rec.Pipeline == "alloy-apps"
		},
		`loki:stream:{app="shop", component="api", level="debug"}`: func(rec report.Recommendation) bool {
			return rec.Action == report.ActionReroute && rec.Proposable && rec.Pipeline == "fluentbit-shop-debug"
		},
	}
	for _, rec := range r.Recommendations {
		if ok, found := want[rec.Signal.ID]; found {
			if !ok(rec) {
				t.Errorf("%s: %+v", rec.Signal.ID, rec)
			}
			delete(want, rec.Signal.ID)
		}
	}
	for id := range want {
		t.Errorf("no recommendation for %s", id)
	}

	// tvt propose with that report (label drops on), validation off: one
	// patch with a change in every pipeline kind.
	rj := filepath.Join(t.TempDir(), "report.json")
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(rj, b, 0o644)
	out := t.TempDir()
	var log strings.Builder
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	o := proposeOptions{DB: db, Config: filepath.Join(dir, "tvt-lab.yaml"), Report: rj, Output: "patch", Dir: out,
		Checkouts: checkoutFlag{repo: filepath.Join(dir, "checkout")}, Validate: "off"}
	if err := runPropose(context.Background(), o, &log); err != nil {
		t.Fatalf("propose: %v\n%s", err, log.String())
	}
	patch, err := os.ReadFile(filepath.Join(out, report.ManifestDir(repo)+".patch"))
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	for what, needle := range map[string]string{
		"Prometheus drop":          "tvt:begin prometheus-scrape/",
		"Alloy drop and labeldrop": `"op_kind"`,
		"Alloy worker reroute":     "tvt_cold",
		"Fluent Bit reroute":       "tvt.cold.shop.debug",
		"collector filter":         "filter/tvt",
	} {
		if !strings.Contains(string(patch), needle) {
			t.Errorf("patch has no %s (%q)", what, needle)
		}
	}
	t.Log(log.String())
	// Manual test M3 applies this patch to the running lab.
	if f := os.Getenv("TVT_WRITERS2_PATCH"); f != "" {
		if err := os.WriteFile(f, patch, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// git takes the patch onto a copy of the checkout.
	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	tmp := t.TempDir()
	if err := os.CopyFS(tmp, os.DirFS(filepath.Join(dir, "checkout"))); err != nil {
		t.Fatal(err)
	}
	pf := filepath.Join(t.TempDir(), "p.patch")
	os.WriteFile(pf, patch, 0o644)
	cmd := exec.Command("git", "apply", "--check", pf)
	cmd.Dir = tmp
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("git apply --check: %v\n%s", err, b)
	}
}
