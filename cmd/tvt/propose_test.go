package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// TestPropose runs tvt propose on the writers state (S7 item 3): drops go
// into the lab's Prometheus scrape config and Alloy config, vendored from
// tvt-lab at the capture into checkout/, as one patch git applies.
func TestPropose(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "writers" {
			st = s
		}
	}
	if st.Name == "" {
		t.Fatal("no writers state in benchmark.yaml")
	}
	db := benchDB(t, st, dirs[st.Name])
	cfg := filepath.Join(dirs[st.Name], "tvt-lab.yaml")
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	checkout := filepath.Join(dirs[st.Name], "checkout")

	run := func() (string, []byte) {
		t.Helper()
		dir := t.TempDir()
		var out strings.Builder
		o := proposeOptions{DB: db, Config: cfg, Output: "patch", Dir: dir, Checkouts: checkoutFlag{repo: checkout}}
		if err := runPropose(context.Background(), o, &out); err != nil {
			t.Fatalf("propose: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(dir, report.ManifestDir(repo)+".patch"))
		if err != nil {
			t.Fatalf("patch: %v\n%s", err, out.String())
		}
		return out.String(), b
	}
	out, patch := run()
	t.Log(out)
	if _, again := run(); string(again) != string(patch) {
		t.Error("a second run wrote a different patch: output must be deterministic")
	}
	t.Logf("patch:\n%s", patch)
	for _, want := range []string{"a/prometheus/scrape/lab.yml", "a/alloy/config.alloy", "tvt:begin prometheus-scrape/", "tvt:begin alloy-apps scan="} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch lacks %q", want)
		}
	}
	// git apply takes it, onto a copy of the checkout.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	tmp := t.TempDir()
	if err := os.CopyFS(tmp, os.DirFS(checkout)); err != nil {
		t.Fatal(err)
	}
	pf := filepath.Join(t.TempDir(), "p.patch")
	if err := os.WriteFile(pf, patch, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "apply", "--check", "-v", pf)
	cmd.Dir = tmp
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply --check: %v\n%s", err, b)
	}
}
