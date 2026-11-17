package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/config"
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

// TestProposeFluentBit: the scan raises the shop's unread debug stream
// (ground truth: route_cold, pipeline fluentbit-shop-debug) as a reroute,
// and tvt propose writes it as a rewrite_tag to the cold output in
// fluent-bit.yaml (S7 item 3). The writers state's tvt-lab.yaml names no
// cold output, so the test's config adds one.
func TestProposeFluentBit(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "writers" {
			st = s
		}
	}
	db := benchDB(t, st, dirs[st.Name])
	in, _, err := readLatest(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	r := report.Build(in, 0)
	const debugID = `reroute:loki:stream:{app="shop", component="api", level="debug"}`
	found := false
	for _, rec := range r.Recommendations {
		if rec.ID == debugID {
			found = true
			if !rec.Proposable || rec.Pipeline != "fluentbit-shop-debug" {
				t.Errorf("debug stream: proposable %v, pipeline %q", rec.Proposable, rec.Pipeline)
			}
		}
	}
	if !found {
		t.Fatalf("the scan didn't raise %s", debugID)
	}
	dir := t.TempDir()
	rj := filepath.Join(dir, "report.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(rj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	cfg := filepath.Join(dir, "tvt.yaml")
	if err := os.WriteFile(cfg, []byte(`pipelines:
  - name: fluentbit-shop-debug
    kind: fluentbit
    repo: `+repo+`
    files: [fluent-bit/fluent-bit.yaml]
    handles: ['{app="shop", component="api", level="debug"}']
    cold: cold
`), 0o644); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	if err := os.CopyFS(checkout, os.DirFS(filepath.Join(dirs[st.Name], "checkout"))); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		t.Helper()
		var out strings.Builder
		o := proposeOptions{DB: db, Config: cfg, Report: rj, Output: "patch", Dir: filepath.Join(dir, "p"), Checkouts: checkoutFlag{repo: checkout}, Validate: "off"}
		if err := runPropose(context.Background(), o, &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	// The lab's config has no cold output: skipped, with the reason.
	if out := run(); !strings.Contains(out, `no output aliased "cold"`) {
		t.Errorf("no cold output: not skipped with the reason\n%s", out)
	}
	cold, err := os.ReadFile("../../pkg/propose/testdata/fluent-bit-cold.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "fluent-bit", "fluent-bit.yaml"), cold, 0o644); err != nil {
		t.Fatal(err)
	}
	out := run()
	patch, err := os.ReadFile(filepath.Join(dir, "p", report.ManifestDir(repo)+".patch"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"a/fluent-bit/fluent-bit.yaml", "+    - name: rewrite_tag", "+      rule: $log ^.*$ tvt.cold.shop.debug false"} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch lacks %q:\n%s", want, patch)
		}
	}
}

// TestProposeMonitors: drops for metrics of the kube-prometheus-stack
// fixture's jobs go into the right ServiceMonitor endpoints; a job no
// monitor resolves to (the apiserver's Service isn't rendered) is skipped
// until tvt.yaml maps it.
func TestProposeMonitors(t *testing.T) {
	root := t.TempDir()
	r, err := os.ReadFile("../../testdata/kube-prometheus-stack/render.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "render.yaml"), r, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := func(name string) report.Recommendation {
		return report.Recommendation{ID: "drop:prom:metric:" + name, Action: report.ActionDrop,
			Signal: report.SignalRef{ID: "prom:metric:" + name, Kind: "metric", Name: name}, Proposable: true}
	}
	recs := []report.Recommendation{rec("node_softnet_times_squeezed_total"), rec("apiserver_storage_objects")}
	jobs := map[string][]string{
		"prom:metric:node_softnet_times_squeezed_total": {"node-exporter"},
		"prom:metric:apiserver_storage_objects":         {"apiserver"},
	}
	p := config.Pipeline{Name: "kps", Kind: "prometheus-operator", Files: []string{"render.yaml"}}
	ok := func(string, string, []byte) error { return nil }
	written, skipped, diff := proposeMonitors(p, recs, jobs, root, 4, ok)
	if len(written) != 1 || written[0].Rec.Signal.Name != "node_softnet_times_squeezed_total" {
		t.Errorf("written %+v", written)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Note, "job apiserver maps to no monitor") || !strings.Contains(skipped[0].Note, "monitor_jobs") {
		t.Errorf("skipped %+v", skipped)
	}
	if !strings.Contains(diff, `+          regex: "node_softnet_times_squeezed_total"`) && !strings.Contains(diff, `regex: "node_softnet_times_squeezed_total"`) {
		t.Errorf("diff:\n%s", diff)
	}
	if !strings.Contains(diff, "tvt:begin kps/monitoring/kps-prometheus-node-exporter scan=4") {
		t.Errorf("block id:\n%s", diff)
	}
	// Mapped by hand, the apiserver job goes in too.
	p.MonitorJobs = map[string]string{"apiserver": "ServiceMonitor/monitoring/kps-kube-prometheus-stack-apiserver"}
	written, skipped, diff = proposeMonitors(p, recs, jobs, root, 4, ok)
	if len(written) != 2 || len(skipped) != 0 || !strings.Contains(diff, "tvt:begin kps/monitoring/kps-kube-prometheus-stack-apiserver") {
		t.Errorf("mapped: written %d skipped %+v\n%s", len(written), skipped, diff)
	}
}
