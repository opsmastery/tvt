package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	if !strings.Contains(out, "build: "+repo+" otelcol/builder-config.yaml: adds filterprocessor v0.165.0") {
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
	for _, want := range []string{"a/fluent-bit/fluent-bit.yaml", "+    - name: rewrite_tag", "+      rule: $log ^(?!.*(?i:error|exception|traceback|critical|fatal)).*$ tvt.cold.shop.debug false"} {
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

// TestProposeHelmValues: with helm_values, the drops go into the team's
// kube-prometheus-stack overlay (the rendered chart only resolves jobs),
// and a new key carries the chart's default rules.
func TestProposeHelmValues(t *testing.T) {
	root := t.TempDir()
	for src, dst := range map[string]string{
		"../../testdata/kube-prometheus-stack/render.yaml": "render.yaml",
		"../../testdata/kube-prometheus-stack/values.yaml": "chart-values.yaml",
		"../../pkg/propose/testdata/kps-values.yaml":       "values.yaml",
	} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := report.Recommendation{ID: "drop:prom:metric:node_softnet_times_squeezed_total", Action: report.ActionDrop,
		Signal: report.SignalRef{ID: "prom:metric:node_softnet_times_squeezed_total", Kind: "metric", Name: "node_softnet_times_squeezed_total"}, Proposable: true}
	jobs := map[string][]string{rec.Signal.ID: {"node-exporter"}}
	p := config.Pipeline{Name: "kps", Kind: "prometheus-operator", Files: []string{"render.yaml"},
		HelmValues: &config.HelmValues{File: "values.yaml", Defaults: "chart-values.yaml"}}
	ok := func(string, string, []byte) error { return nil }
	written, skipped, diff := proposeMonitors(p, []report.Recommendation{rec}, jobs, root, 6, ok)
	if len(written) != 1 || len(skipped) != 0 {
		t.Fatalf("written %d skipped %+v", len(written), skipped)
	}
	if !strings.Contains(diff, "a/values.yaml") || !strings.Contains(diff, "+prometheus-node-exporter:") || strings.Contains(diff, "a/render.yaml") {
		t.Errorf("diff:\n%s", diff)
	}
	p.HelmValues.Defaults = ""
	if _, skipped, _ = proposeMonitors(p, []report.Recommendation{rec}, jobs, root, 6, ok); len(skipped) != 1 || !strings.Contains(skipped[0].Note, "helm_values.defaults") {
		t.Errorf("no defaults: %+v", skipped)
	}
}

// TestProposeDeltaLabels: the coding agent's session_id and user_email
// (ground truth genai_agent.labels: aggregate in the ordered pipeline)
// become a transform and an interval ahead of delta_to_cumulative in the
// lab's collector config (S7 item 8). The writers state drops both
// metrics outright (a label removal on a dropped metric is never raised),
// so the reviewed report keeps the metrics and carries the label rows.
func TestProposeDeltaLabels(t *testing.T) {
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
	recs := r.Recommendations[:0]
	for _, rec := range r.Recommendations {
		if !strings.HasPrefix(rec.Signal.Name, "coding_agent_") {
			recs = append(recs, rec)
		}
	}
	for _, m := range []string{"coding_agent_token_usage_total", "coding_agent_cost_usage_USD_total"} {
		for _, l := range []string{"session_id", "user_email"} {
			recs = append(recs, report.Recommendation{ID: "label_removal:prom:label:" + m + ":" + l, Action: report.ActionLabelRemoval,
				Signal:   report.SignalRef{ID: "prom:label:" + m + ":" + l, Kind: "label", Name: l, Parent: m, Source: "prom"},
				Pipeline: "otelcol-agent", Proposable: true, Removal: "aggregate"})
		}
	}
	// The exporter's scope label, which M7 found proposed as a no-op delete.
	recs = append(recs, report.Recommendation{ID: "label_removal:prom:label:coding_agent_token_usage_total:otel_scope_name", Action: report.ActionLabelRemoval,
		Signal:   report.SignalRef{ID: "prom:label:coding_agent_token_usage_total:otel_scope_name", Kind: "label", Name: "otel_scope_name", Parent: "coding_agent_token_usage_total", Source: "prom"},
		Pipeline: "otelcol-agent", Proposable: true, Removal: "aggregate"})
	r.Recommendations = recs
	dir := t.TempDir()
	rj := filepath.Join(dir, "report.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(rj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	var out strings.Builder
	o := proposeOptions{DB: db, Config: filepath.Join(dirs[st.Name], "tvt-lab.yaml"), Report: rj, Output: "patch", Dir: filepath.Join(dir, "p"),
		Checkouts: checkoutFlag{repo: filepath.Join(dirs[st.Name], "checkout")}, Validate: "off"}
	if err := runPropose(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(filepath.Join(dir, "p", report.ManifestDir(repo)+".patch"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{
		"+  transform/tvt_aggregate:",
		`delete_matching_keys(datapoint.attributes, "^session[^A-Za-z0-9:]+id$")`,
		`delete_matching_keys(datapoint.attributes, "^user[^A-Za-z0-9:]+email$")`,
		"+  interval/tvt:",
		"processors: [transform/tvt_aggregate, interval/tvt, delta_to_cumulative, batch]",
	} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch lacks %q", want)
		}
	}
	if strings.Contains(string(patch), "otel[^A-Za-z0-9:]+scope") {
		t.Error("the patch deletes otel_scope_name, which the exporter adds from the scope")
	}
	if !strings.Contains(out.String(), "skipped label_removal:prom:label:coding_agent_token_usage_total:otel_scope_name: otel_scope_name isn't a datapoint attribute") {
		t.Errorf("otel_scope_name wasn't skipped with the reason:\n%s", out.String())
	}
	for _, l := range []string{"session_id", "user_email"} {
		if strings.Contains(out.String(), "skipped label_removal:prom:label:coding_agent_token_usage_total:"+l) {
			t.Errorf("%s was skipped:\n%s", l, out.String())
		}
	}
}

// TestProposeLabelDrops: a label removal where no series merge becomes a
// relabel rule emptying the label on that metric in the Alloy pipeline;
// one that merges series is skipped with the reason.
func TestProposeLabelDrops(t *testing.T) {
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
	const sig = "prom:label:shop_feature_flag_evaluations_total:flag"
	found := false
	for _, pt := range in.PipelineTargets {
		if pt.Subject == sig {
			found = true
			if pt.Pipeline != "alloy-apps" || len(pt.Jobs) == 0 {
				t.Fatalf("%s: target %+v", sig, pt)
			}
		}
	}
	if !found {
		t.Fatalf("no pipeline target for %s", sig)
	}
	r := report.Build(in, 0)
	label := func(metric, l, removal string) report.Recommendation {
		return report.Recommendation{ID: "label_removal:prom:label:" + metric + ":" + l, Action: report.ActionLabelRemoval,
			Signal:   report.SignalRef{ID: "prom:label:" + metric + ":" + l, Kind: "label", Name: l, Parent: metric, Source: "prom"},
			Pipeline: "alloy-apps", Proposable: true, Removal: removal, Merges: 12}
	}
	r.Recommendations = []report.Recommendation{
		label("shop_feature_flag_evaluations_total", "flag", "labeldrop"),
		label("shop_cart_operations_total", "user_id", "aggregate"),
	}
	dir := t.TempDir()
	rj := filepath.Join(dir, "report.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(rj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	var out strings.Builder
	o := proposeOptions{DB: db, Config: filepath.Join(dirs[st.Name], "tvt-lab.yaml"), Report: rj, Output: "patch", Dir: filepath.Join(dir, "p"),
		Checkouts: checkoutFlag{repo: filepath.Join(dirs[st.Name], "checkout")}, Validate: "off"}
	if err := runPropose(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(filepath.Join(dir, "p", report.ManifestDir(repo)+".patch"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(string(patch), `+    target_label  = "flag"`) || !strings.Contains(string(patch), `+    regex         = "shop_feature_flag_evaluations_total"`) {
		t.Errorf("patch:\n%s", patch)
	}
	if !strings.Contains(out.String(), "removing user_id merges 12 series, which a relabel rule can't sum") {
		t.Errorf("the merging removal wasn't skipped with the reason:\n%s", out.String())
	}
}

// TestProposeAlloyRoutes: a stream reroute on an Alloy pipeline splits
// the stream to the cold component, in the same pass as the pipeline's
// metric drops (the routes keep the drop block).
func TestProposeAlloyRoutes(t *testing.T) {
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
	sel := `{app="shop", component="api", level="info"}`
	r.Recommendations = append(r.Recommendations, report.Recommendation{ID: "reroute:loki:stream:" + sel, Action: report.ActionReroute,
		Signal:   report.SignalRef{ID: "loki:stream:" + sel, Kind: "stream", Name: sel, Source: "loki"},
		Pipeline: "alloy-apps", Proposable: true})
	dir := t.TempDir()
	rj := filepath.Join(dir, "report.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(rj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfgSrc, err := os.ReadFile(filepath.Join(dirs[st.Name], "tvt-lab.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfgText := strings.Replace(string(cfgSrc), "    files: [alloy/config.alloy]\n", "    files: [alloy/config.alloy]\n    cold: loki.write.cold\n", 1)
	cfg := filepath.Join(dirs[st.Name], "tvt-lab.alloyroutes-test.yaml")
	if err := os.WriteFile(cfg, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(cfg) })
	checkout := t.TempDir()
	if err := os.CopyFS(checkout, os.DirFS(filepath.Join(dirs[st.Name], "checkout"))); err != nil {
		t.Fatal(err)
	}
	ap := filepath.Join(checkout, "alloy", "config.alloy")
	a, _ := os.ReadFile(ap)
	a = []byte(strings.Replace(string(a), "loki.write \"loki\" {", "loki.write \"cold\" {\n  endpoint {\n    url = \"http://loki-cold:3100/loki/api/v1/push\"\n  }\n}\n\nloki.write \"loki\" {", 1))
	if err := os.WriteFile(ap, a, 0o644); err != nil {
		t.Fatal(err)
	}
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	var out strings.Builder
	o := proposeOptions{DB: db, Config: cfg, Report: rj, Output: "patch", Dir: filepath.Join(dir, "p"), Checkouts: checkoutFlag{repo: checkout}, Validate: "off"}
	if err := runPropose(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(filepath.Join(dir, "p", report.ManifestDir(repo)+".patch"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`+loki.relabel "tvt_cold" {`, `+  forward_to = [loki.write.cold.receiver]`, "tvt:begin alloy-apps scan="} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch lacks %q", want)
		}
	}
	if strings.Contains(out.String(), "skipped reroute:loki:stream:"+sel) {
		t.Errorf("the reroute was skipped:\n%s", out.String())
	}
}

// TestProposeAlloySample: an unread stream on an Alloy pipeline with no
// cold tier is sampled when tiers.sample_one_in is set (S10 item 4), and
// skipped when it isn't. The claimed saving scales by 1 - 1/n, and check
// passes the change.
func TestProposeAlloySample(t *testing.T) {
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
	sel := `{app="shop", component="api", level="info"}`
	r.Recommendations = append(r.Recommendations, report.Recommendation{ID: "reroute:loki:stream:" + sel, Action: report.ActionReroute,
		Signal:  report.SignalRef{ID: "loki:stream:" + sel, Kind: "stream", Name: sel, Source: "loki"},
		Savings: report.Savings{Monthly: 10, Currency: "USD"}, Expected: report.Expected{MonthlySaving: 10},
		Pipeline: "alloy-apps", Proposable: true})
	dir := t.TempDir()
	rj := filepath.Join(dir, "report.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(rj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfgSrc, err := os.ReadFile(filepath.Join(dirs[st.Name], "tvt-lab.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	run := func(t *testing.T, cfgText string) (string, string) {
		cfg := filepath.Join(dirs[st.Name], "tvt-lab.sample-test.yaml")
		if err := os.WriteFile(cfg, []byte(cfgText), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(cfg) })
		checkout := t.TempDir()
		if err := os.CopyFS(checkout, os.DirFS(filepath.Join(dirs[st.Name], "checkout"))); err != nil {
			t.Fatal(err)
		}
		pd := filepath.Join(t.TempDir(), "p")
		var out strings.Builder
		o := proposeOptions{DB: db, Config: cfg, Report: rj, Output: "patch", Dir: pd, Checkouts: checkoutFlag{repo: checkout}, Validate: "off"}
		if err := runPropose(context.Background(), o, &out); err != nil {
			t.Fatal(err)
		}
		patch, _ := os.ReadFile(filepath.Join(pd, report.ManifestDir(repo)+".patch"))
		return out.String(), string(patch)
	}
	t.Run("off", func(t *testing.T) {
		out, _ := run(t, string(cfgSrc))
		if !strings.Contains(out, "skipped reroute:loki:stream:"+sel) || !strings.Contains(out, "tiers.sample_one_in is off") {
			t.Errorf("not skipped with the reason:\n%s", out)
		}
	})
	t.Run("on", func(t *testing.T) {
		out, patch := run(t, string(cfgSrc)+"\ntiers:\n  sample_one_in: 10\n")
		if strings.Contains(out, "skipped reroute:loki:stream:"+sel) {
			t.Fatalf("the sample was skipped:\n%s", out)
		}
		for _, want := range []string{"+    stage.sampling {", "+      rate = 0.1", "sampled 1 in 10"} {
			if !strings.Contains(patch, want) {
				t.Errorf("patch lacks %q", want)
			}
		}
		if strings.Contains(patch, "tvt_cold") {
			t.Error("a sample patch routes to a cold tier")
		}
	})
}

func TestSampledRec(t *testing.T) {
	rec := report.Recommendation{Signal: report.SignalRef{Name: `{app="x"}`}, Savings: report.Savings{Monthly: 10},
		Expected: report.Expected{MonthlySaving: 10, HotBytesPerDayFall: 1000, SharePct: 20, Statements: []string{"moves to cold"}}}
	got := sampledRec(rec, 4)
	if got.Savings.Monthly != 7.5 || got.Expected.MonthlySaving != 7.5 || got.Expected.HotBytesPerDayFall != 750 || got.Expected.SharePct != 15 {
		t.Errorf("%+v", got)
	}
	if strings.Contains(strings.Join(got.Expected.Statements, " "), "cold storage instead") || !strings.Contains(got.Expected.Statements[0], "1 line in 4") {
		t.Errorf("statements %q", got.Expected.Statements)
	}
}

// TestProposeFluentBitClassic: a fluentbit pipeline whose file is classic
// .conf gets the rewrite_tag as a [FILTER] section.
func TestProposeFluentBitClassic(t *testing.T) {
	root := t.TempDir()
	b, err := os.ReadFile("../../pkg/propose/testdata/fluent-bit-cold.conf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fluent-bit.conf"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	sel := `{app="shop", component="api", level="debug"}`
	rec := report.Recommendation{ID: "reroute:loki:stream:" + sel, Action: report.ActionReroute,
		Signal: report.SignalRef{ID: "loki:stream:" + sel, Kind: "stream", Name: sel}, Proposable: true}
	p := config.Pipeline{Name: "fb", Kind: "fluentbit", Files: []string{"fluent-bit.conf"}, Cold: "cold"}
	written, skipped, diff := proposeFluentBit(p, []report.Recommendation{rec}, root, 3, func(string, string, []byte) error { return nil })
	if len(written) != 1 || len(skipped) != 0 || !strings.Contains(diff, "+[FILTER]") || !strings.Contains(diff, "+    Rule         $log ^(?!.*(?i:error|exception|traceback|critical|fatal)).*$ tvt.cold.shop.debug false") {
		t.Errorf("written %d skipped %+v\n%s", len(written), skipped, diff)
	}
}

// TestProposeFluentBitHelm: a fluentbit pipeline with helm_values gets the
// reroute in the overlay's config.filters, starting from the chart's
// default filters.
func TestProposeFluentBitHelm(t *testing.T) {
	root := t.TempDir()
	for src, dst := range map[string]string{
		"../../pkg/propose/testdata/fluent-bit-values.yaml": "values.yaml",
		"../../testdata/fluent-bit-chart/values.yaml":       "chart-values.yaml",
	} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(root, dst), b, 0o644)
	}
	sel := `{app="shop", component="api", level="debug"}`
	rec := report.Recommendation{ID: "reroute:loki:stream:" + sel, Action: report.ActionReroute,
		Signal: report.SignalRef{ID: "loki:stream:" + sel, Kind: "stream", Name: sel}, Proposable: true}
	p := config.Pipeline{Name: "fb", Kind: "fluentbit", Cold: "cold", HelmValues: &config.HelmValues{File: "values.yaml", Defaults: "chart-values.yaml"}}
	written, skipped, diff := proposeFluentBitHelm(p, []report.Recommendation{rec}, root, 3, func(string, string, []byte) error { return nil })
	if len(written) != 1 || len(skipped) != 0 || !strings.Contains(diff, "+        Name kubernetes") || !strings.Contains(diff, "+        Rule         $log ^(?!.*(?i:error|exception|traceback|critical|fatal)).*$ tvt.cold.shop.debug false") {
		t.Errorf("written %d skipped %+v\n%s", len(written), skipped, diff)
	}
}

// TestProposeFluentOperator: a fluentbit pipeline whose file holds Fluent
// Operator resources gets a rewriteTag ClusterFilter.
func TestProposeFluentOperator(t *testing.T) {
	root := t.TempDir()
	b, err := os.ReadFile("../../pkg/propose/testdata/fluent-operator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "logging.yaml"), b, 0o644)
	sel := `{app="shop", component="api", level="debug"}`
	rec := report.Recommendation{ID: "reroute:loki:stream:" + sel, Action: report.ActionReroute,
		Signal: report.SignalRef{ID: "loki:stream:" + sel, Kind: "stream", Name: sel}, Proposable: true}
	p := config.Pipeline{Name: "fo", Kind: "fluentbit", Files: []string{"logging.yaml"}, Cold: "cold"}
	written, skipped, diff := proposeFluentBit(p, []report.Recommendation{rec}, root, 3, func(string, string, []byte) error { return nil })
	if len(written) != 1 || len(skipped) != 0 || !strings.Contains(diff, "+kind: ClusterFilter") || !strings.Contains(diff, `+          - "$log ^(?!.*(?i:error|exception|traceback|critical|fatal)).*$ tvt.cold.shop.debug false"`) {
		t.Errorf("written %d skipped %+v\n%s", len(written), skipped, diff)
	}
}

// TestProposeOperatorValidate: the monitor and Fluent Operator writers'
// output passes kubeconform with the custom resource schemas, so those
// files are validated, not marked NOT VALIDATED.
func TestProposeOperatorValidate(t *testing.T) {
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		t.Skip("set TVT_TEST_VALIDATORS=1 to run the pipelines' validators")
	}
	v := &validator{o: proposeOptions{Validate: "require"}}
	defer v.cleanup()
	ctx := context.Background()

	root := t.TempDir()
	r, _ := os.ReadFile("../../testdata/kube-prometheus-stack/render.yaml")
	os.WriteFile(filepath.Join(root, "render.yaml"), r, 0o644)
	name := "node_softnet_times_squeezed_total"
	rec := report.Recommendation{ID: "drop:prom:metric:" + name, Action: report.ActionDrop,
		Signal: report.SignalRef{ID: "prom:metric:" + name, Kind: "metric", Name: name}, Proposable: true}
	p := config.Pipeline{Name: "kps", Kind: "prometheus-operator", Files: []string{"render.yaml"}}
	written, skipped, _ := proposeMonitors(p, []report.Recommendation{rec}, map[string][]string{"prom:metric:" + name: {"node-exporter"}}, root, 4, v.check(ctx, p))
	if len(written) != 1 || len(skipped) != 0 {
		t.Errorf("monitors: written %d skipped %+v", len(written), skipped)
	}

	root = t.TempDir()
	b, _ := os.ReadFile("../../pkg/propose/testdata/fluent-operator.yaml")
	os.WriteFile(filepath.Join(root, "logging.yaml"), b, 0o644)
	sel := `{app="shop", component="api", level="debug"}`
	rr := report.Recommendation{ID: "reroute:loki:stream:" + sel, Action: report.ActionReroute,
		Signal: report.SignalRef{ID: "loki:stream:" + sel, Kind: "stream", Name: sel}, Proposable: true}
	fp := config.Pipeline{Name: "fo", Kind: "fluentbit", Files: []string{"logging.yaml"}, Cold: "cold"}
	written, skipped, _ = proposeFluentBit(fp, []report.Recommendation{rr}, root, 3, v.check(ctx, fp))
	if len(written) != 1 || len(skipped) != 0 {
		t.Errorf("fluent operator: written %d skipped %+v", len(written), skipped)
	}
	notes := strings.Join(v.notes, "\n")
	if strings.Count(notes, "valid (") != 2 || !strings.Contains(notes, "not checked") {
		t.Errorf("notes:\n%s", notes)
	}
	t.Log(notes)
}

// TestSelectorLabelsServiceName: a service_name Loki derived from app is
// left out of the labels tvt matches in the pipeline; one set on its own
// stays.
func TestSelectorLabelsServiceName(t *testing.T) {
	for sel, want := range map[string]string{
		`{app="shop", component="api", level="debug", service_name="shop"}`:  "app=shop component=api level=debug",
		`{app="shop", component="api", level="debug", service_name="other"}`: "app=shop component=api level=debug service_name=other",
		`{component="api", service_name="api"}`:                              "component=api",
		`{service_name="checkout", level="info"}`:                            "level=info service_name=checkout",
	} {
		l, err := selectorLabels(sel)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for k, v := range l {
			got = append(got, k+"="+v)
		}
		slices.Sort(got)
		if strings.Join(got, " ") != want {
			t.Errorf("%s: %v, want %s", sel, got, want)
		}
	}
}

// TestCapQueue: S9 item 6's per-owner cap keeps each owner's best rows.
func TestCapQueue(t *testing.T) {
	var recs []report.Recommendation
	for i, o := range []string{"a", "a", "b", "a", "", ""} {
		recs = append(recs, report.Recommendation{ID: string(rune('0' + i)), Owner: o, Proposable: true, Status: report.StatusOpen})
	}
	recs = append(recs, report.Recommendation{ID: "x", Owner: "a"}) // not proposable: never counted or cut
	kept, waiting := capQueue(recs, 1)
	var ids []string
	for _, r := range kept {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "0,2,4,x" || len(waiting) != 3 || !strings.Contains(waiting[0].Note, "queued: a already has 1") {
		t.Errorf("kept %v, waiting %v", ids, waiting)
	}
	if k, w := capQueue(recs, 0); len(k) != len(recs) || w != nil {
		t.Error("0 capped")
	}
}

// TestProposeBucket: a bucket recommendation (S10 item 1) on the coding
// agent's metric becomes a replace_pattern in the Collector's ordered
// pipeline; on the Alloy pipeline, which can't sum the merged series, it
// is skipped with the reason. With TVT_TEST_VALIDATORS=1 the Collector
// file goes through otelcol-contrib validate.
func TestProposeBucket(t *testing.T) {
	validation := "off"
	if os.Getenv("TVT_TEST_VALIDATORS") != "" {
		validation = "require"
	}
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
	bucket := func(pipeline, metric, l string) report.Recommendation {
		return report.Recommendation{ID: "bucket:prom:label:" + metric + ":" + l, Action: report.ActionBucket,
			Signal:   report.SignalRef{ID: "prom:label:" + metric + ":" + l, Kind: "label", Name: l, Parent: metric, Source: "prom"},
			Pipeline: pipeline, Proposable: true, Merges: 40}
	}
	r.Recommendations = []report.Recommendation{
		bucket("otelcol-agent", "coding_agent_token_usage_total", "http_status_code"),
		bucket("alloy-apps", "shop_cart_operations_total", "status_code"),
	}
	dir := t.TempDir()
	rj := filepath.Join(dir, "report.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(rj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	var out strings.Builder
	o := proposeOptions{DB: db, Config: filepath.Join(dirs[st.Name], "tvt-lab.yaml"), Report: rj, Output: "patch", Dir: filepath.Join(dir, "p"),
		Checkouts: checkoutFlag{repo: filepath.Join(dirs[st.Name], "checkout")}, Validate: validation}
	if err := runPropose(context.Background(), o, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	patch, err := os.ReadFile(filepath.Join(dir, "p", report.ManifestDir(repo)+".patch"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{
		"+  transform/tvt_aggregate:",
		`replace_pattern(datapoint.attributes["http_status_code"], "^([0-9])[0-9][0-9]$", "$${1}00")`,
		"+  interval/tvt:",
		"processors: [transform/tvt_aggregate, interval/tvt, delta_to_cumulative, batch]",
	} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch lacks %q\n%s", want, patch)
		}
	}
	if !strings.Contains(out.String(), "skipped bucket:prom:label:shop_cart_operations_total:status_code: bucketing") {
		t.Errorf("the Alloy bucket wasn't skipped with the reason:\n%s", out.String())
	}
	if validation == "require" && (strings.Contains(out.String(), "NOT VALIDATED") || !strings.Contains(out.String(), "valid (")) {
		t.Errorf("not validated:\n%s", out.String())
	}
}

// TestProposeIntervals: the report's longer scrape intervals (S10 item 5)
// go into the scrape configs of the pipeline that carries the job, after
// the drops, and the manifest's scrape limits let check refuse more.
func TestProposeIntervals(t *testing.T) {
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
	var loki report.IntervalChange
	for _, iv := range r.Intervals {
		if iv.Job == "loki" {
			loki = iv
		}
	}
	if loki.ProposedSec != 120 || loki.Pipeline == "" {
		t.Fatalf("loki interval: %+v", loki)
	}
	r.Recommendations = nil
	r.Intervals = []report.IntervalChange{loki,
		{Job: "nope", Pipeline: loki.Pipeline, CurrentSec: 10, ProposedSec: 60, Readers: 1},
		{Job: "elsewhere", CurrentSec: 10, ProposedSec: 60, Readers: 1}}
	dir := t.TempDir()
	rj := filepath.Join(dir, "report.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(rj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	const repo = "github.com/Opsmastery-Summit/tvt-lab"
	var out strings.Builder
	o := proposeOptions{DB: db, Config: filepath.Join(dirs[st.Name], "tvt-lab.yaml"), Report: rj, Output: "patch", Dir: filepath.Join(dir, "p"),
		Checkouts: checkoutFlag{repo: filepath.Join(dirs[st.Name], "checkout")}, Validate: "off"}
	if err := runPropose(context.Background(), o, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	patch, err := os.ReadFile(filepath.Join(dir, "p", report.ManifestDir(repo)+".patch"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{"+    # tvt:begin interval:loki scan=", "+    scrape_interval: 2m"} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch lacks %q:\n%s", want, patch)
		}
	}
	if strings.Contains(string(patch), "interval:loki\"") || strings.Contains(string(patch), `"recommendation": "interval:`) {
		t.Error("the interval is in .tvt/proposal.json, which lists what needs an owner's confirmation")
	}
	for _, want := range []string{"skipped interval:nope: job nope has no scrape config", "skipped interval:elsewhere: the job's metrics come through no single pipeline"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// The manifest carries the limits check enforces: the proposal for a
	// proposed job, the current interval for a held one.
	cfg, err := config.Load(filepath.Join(dirs[st.Name], "tvt-lab.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	limits := map[string]report.ScrapeLimit{}
	for _, l := range buildManifests(cfg, in)[repo].ScrapeLimits {
		limits[l.Job] = l
	}
	if l := limits["loki"]; l.MaxSeconds != 120 {
		t.Errorf("loki limit %+v", l)
	}
	if l := limits["node"]; l.MaxSeconds != 10 || !strings.Contains(l.Why, "$__rate_interval") {
		t.Errorf("node limit %+v", l)
	}
}
