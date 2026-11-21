package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// TestProposeLevelTier: a level_tier recommendation (S10 item 3) goes
// into the OTel Collector pipeline that carries the stream: the hot
// filter in its logs pipeline and the cold copy to pipelines[].cold, with
// the record log events kept hot. Without a cold exporter it's skipped
// with the reason, and the proposal file carries it for the owner's
// confirmation.
func TestProposeLevelTier(t *testing.T) {
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
	const sel = `{service_name="coding-agent"}`
	r := report.Build(in, 0)
	r.Recommendations = []report.Recommendation{{ID: "level_tier:loki:stream:" + sel, Action: report.ActionLevelTier,
		Signal:   report.SignalRef{ID: "loki:stream:" + sel, Kind: "stream", Name: sel, Source: "loki"},
		Pipeline: "otelcol-agent", Proposable: true, EvidenceHash: "abc", Savings: report.Savings{Monthly: 0.4, Currency: "USD"}}}
	dir := t.TempDir()
	rj := filepath.Join(dir, "report.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(rj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	// The capture's config and checkout, with a cold exporter.
	checkout := filepath.Join(dir, "checkout")
	if err := copyTree(filepath.Join(dirs[st.Name], "checkout"), checkout); err != nil {
		t.Fatal(err)
	}
	col := filepath.Join(checkout, "otelcol", "config.yaml")
	src, _ := os.ReadFile(col)
	os.WriteFile(col, []byte(strings.Replace(string(src), "  nop: {}\n", "  nop: {}\n  file/cold:\n    path: /tmp/cold.json\n", 1)), 0o644)
	cfgSrc, _ := os.ReadFile(filepath.Join(dirs[st.Name], "tvt-lab.yaml"))
	run := func(cfgText string) (string, string) {
		cfg := filepath.Join(dir, "tvt-lab.yaml")
		os.WriteFile(cfg, []byte(cfgText), 0o644)
		out := strings.Builder{}
		pd := filepath.Join(dir, "p")
		os.RemoveAll(pd)
		o := proposeOptions{DB: db, Config: cfg, Report: rj, Output: "patch", Dir: pd,
			Checkouts: checkoutFlag{"github.com/Opsmastery-Summit/tvt-lab": checkout}, Validate: "off"}
		if err := runPropose(context.Background(), o, &out); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		patch, _ := os.ReadFile(filepath.Join(pd, report.ManifestDir("github.com/Opsmastery-Summit/tvt-lab")+".patch"))
		return out.String(), string(patch)
	}
	withCold := strings.Replace(string(cfgSrc), "    version: 0.165.0", "    cold: file/cold\n    version: 0.165.0", 1)
	if withCold == string(cfgSrc) {
		t.Fatal("couldn't add cold to the otelcol-agent pipeline")
	}
	out, patch := run(withCold)
	for _, want := range []string{
		"+  filter/tvt-tier-hot:",
		`resource.attributes["service.name"] == "coding-agent" and log.severity_number > 0 and log.severity_number < 13`,
		`and not (log.attributes["event.name"] == "tool_result") and not (log.attributes["event.name"] == "tool_decision")`,
		"+    logs/tvt-tier:",
		"+      exporters: [file/cold]",
		"processors: [filter/tvt-tier-hot, batch]",
		`"recommendation": "level_tier:loki:stream:{service_name=\"coding-agent\"}"`,
		"stream whose lines below warn nothing reads",
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch lacks %q\n%s\n%s", want, patch, out)
		}
	}
	out, _ = run(string(cfgSrc))
	if !strings.Contains(out, "names no cold exporter") {
		t.Errorf("no cold exporter: not skipped with the reason:\n%s", out)
	}
}

// TestProposeLevelTierAlloy: in an Alloy pipeline a level tier is the
// stream's reroute with the level line filter: tvt_hot drops only the
// lines naming a level below warn.
func TestProposeLevelTierAlloy(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "writers-2" {
			st = s
		}
	}
	db := benchDB(t, st, dirs[st.Name])
	in, _, err := readLatest(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	const sel = `{app="shop", component="worker"}`
	r := report.Build(in, 0)
	r.Recommendations = []report.Recommendation{{ID: "level_tier:loki:stream:" + sel, Action: report.ActionLevelTier,
		Signal:   report.SignalRef{ID: "loki:stream:" + sel, Kind: "stream", Name: sel, Source: "loki"},
		Pipeline: "alloy-apps", Proposable: true, EvidenceHash: "abc", Savings: report.Savings{Monthly: 0.1, Currency: "USD"}}}
	dir := t.TempDir()
	rj := filepath.Join(dir, "report.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(rj, b, 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	o := proposeOptions{DB: db, Config: filepath.Join(dirs[st.Name], "tvt-lab.yaml"), Report: rj, Output: "patch", Dir: filepath.Join(dir, "p"),
		Checkouts: checkoutFlag{"github.com/Opsmastery-Summit/tvt-lab": filepath.Join(dirs[st.Name], "checkout")}, Validate: "off"}
	if err := runPropose(context.Background(), o, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	patch, _ := os.ReadFile(filepath.Join(dir, "p", report.ManifestDir("github.com/Opsmastery-Summit/tvt-lab")+".patch"))
	for _, want := range []string{`loki.process "tvt_hot"`, `|~ \"(?i)(?:^|`, `loki.relabel "tvt_cold"`} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch lacks %s\n%s\n%s", want, patch, out.String())
		}
	}
}
