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
func TestPropose(t *testing.T) { testPropose(t, "off") }

// TestProposeValidate is TestPropose with each pipeline's real validator
// (S7 item 5), in containers when the binaries aren't on PATH. It pulls
// images, so it runs only with TVT_TEST_VALIDATORS=1.
func TestProposeValidate(t *testing.T) {
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		t.Skip("set TVT_TEST_VALIDATORS=1 to run the pipelines' validators")
	}
	testPropose(t, "require")
}

func testPropose(t *testing.T, validation string) {
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
		o := proposeOptions{DB: db, Config: cfg, Output: "patch", Dir: dir, Checkouts: checkoutFlag{repo: checkout}, Validate: validation}
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
	if validation == "require" {
		for _, f := range []string{"prometheus/scrape/lab.yml", "alloy/config.alloy", "otelcol/config.yaml"} {
			if !strings.Contains(out, "validation: "+repo+" "+f+": valid (") {
				t.Errorf("%s wasn't validated:\n%s", f, out)
			}
		}
		if strings.Contains(out, "rejected") || strings.Contains(out, "not validated") {
			t.Errorf("a validator refused or didn't run:\n%s", out)
		}
		return
	}
	if _, again := run(); string(again) != string(patch) {
		t.Error("a second run wrote a different patch: output must be deterministic")
	}
	t.Logf("patch:\n%s", patch)
	for _, want := range []string{"a/prometheus/scrape/lab.yml", "a/alloy/config.alloy", "tvt:begin prometheus-scrape/", "tvt:begin alloy-apps scan=", "a/otelcol/config.yaml", "tvt:begin otelcol-agent scan=", `metric.unit == "USD"`} {
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

// TestProposeValidationFails: a validator that rejects the changed config
// keeps it out of the patch (S7 item 5), and a pipeline with no validator
// available is written but marked NOT VALIDATED.
func TestProposeValidationFails(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "promtool"), []byte("#!/bin/sh\necho 'FAILED: parsing YAML file: nope'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "writers" {
			st = s
		}
	}
	db := benchDB(t, st, dirs[st.Name])
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	dir := t.TempDir()
	var out strings.Builder
	o := proposeOptions{DB: db, Config: filepath.Join(dirs[st.Name], "tvt-lab.yaml"), Output: "patch", Dir: dir,
		Checkouts: checkoutFlag{repo: filepath.Join(dirs[st.Name], "checkout")}, Validate: "auto", Runtime: "none"}
	if err := runPropose(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "promtool rejected the changed config: FAILED: parsing YAML file: nope") {
		t.Errorf("no rejection:\n%s", s)
	}
	if !strings.Contains(s, "validation: "+repo+" alloy/config.alloy: NOT VALIDATED: no alloy on PATH and no container runtime") {
		t.Errorf("alloy not marked unvalidated:\n%s", s)
	}
	patch, err := os.ReadFile(filepath.Join(dir, report.ManifestDir(repo)+".patch"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(patch), "a/prometheus/scrape/lab.yml") {
		t.Error("the rejected Prometheus config is in the patch")
	}
	if !strings.Contains(string(patch), "a/alloy/config.alloy") || !strings.Contains(string(patch), "NOT VALIDATED") {
		t.Error("the patch should carry the unvalidated Alloy change, marked")
	}
}

// TestProposeBuildManifest: with an ocb manifest next to the collector
// config that lacks the filter processor, the patch adds it (S7 item 4);
// without one, the patch says otelcol-contrib is assumed.
func TestProposeBuildManifest(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "writers" {
			st = s
		}
	}
	db := benchDB(t, st, dirs[st.Name])
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	checkout := t.TempDir()
	if err := os.CopyFS(checkout, os.DirFS(filepath.Join(dirs[st.Name], "checkout"))); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile("../../pkg/propose/testdata/builder-config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	run := func() (string, string) {
		t.Helper()
		dir := t.TempDir()
		var out strings.Builder
		o := proposeOptions{DB: db, Config: filepath.Join(dirs[st.Name], "tvt-lab.yaml"), Output: "patch", Dir: dir,
			Checkouts: checkoutFlag{repo: checkout}, Validate: "off"}
		if err := runPropose(context.Background(), o, &out); err != nil {
			t.Fatal(err)
		}
		patch, err := os.ReadFile(filepath.Join(dir, report.ManifestDir(repo)+".patch"))
		if err != nil {
			t.Fatal(err)
		}
		return out.String(), string(patch)
	}
	out, patch := run()
	if !strings.Contains(out, "build: "+repo+" no ocb builder manifest") || !strings.Contains(patch, "assumed otelcol-contrib") {
		t.Errorf("no manifest: not noted\n%s", out)
	}
	if err := os.WriteFile(filepath.Join(checkout, "otelcol", "builder-config.yaml"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	out, patch = run()
	if !strings.Contains(out, "build: "+repo+" otelcol/builder-config.yaml: adds the filter processor v0.165.0") {
		t.Errorf("manifest: not noted\n%s", out)
	}
	if !strings.Contains(patch, "a/otelcol/builder-config.yaml") || !strings.Contains(patch, "+  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor v0.165.0") {
		t.Errorf("manifest not patched:\n%s", patch)
	}
}
