package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// TestProtectionManifest (S7 item 1, ADR 0003 and 0007) writes the writers
// state's manifests and checks them against its ground truth: one manifest
// for tvt-lab (all four lab pipelines are in that repo), every protected
// metric and stream of a mapped pipeline in it with its protectors, nothing
// ground truth drops or reroutes, no query text, and the same bytes on a
// second run.
func TestProtectionManifest(t *testing.T) {
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
	_, _, gt := scanState(t, st, dirs[st.Name])
	db := benchDB(t, st, dirs[st.Name])
	cfg := filepath.Join(dirs[st.Name], "tvt-lab.yaml")

	write := func() []byte {
		t.Helper()
		dir := t.TempDir()
		var out strings.Builder
		o := reportOptions{DB: db, Config: cfg, JSON: filepath.Join(dir, "r.json"), Top: 0, ManifestDir: dir}
		if err := runReport(context.Background(), o, &out); err != nil {
			t.Fatalf("report: %v", err)
		}
		t.Log(strings.SplitN(out.String(), "\n", 2)[0])
		path := filepath.Join(dir, report.ManifestDir("github.com/Opsmastery-Summit/tvt-lab"), ".tvt", "protect.json")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("manifest: %v", err)
		}
		return b
	}
	b := write()
	if again := write(); !bytes.Equal(b, again) {
		t.Error("a second run wrote a different manifest: output must be deterministic")
	}
	var m report.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != report.ManifestVersion || m.Repo != "github.com/Opsmastery-Summit/tvt-lab" {
		t.Errorf("version %d repo %q", m.Version, m.Repo)
	}
	if want := []string{"alloy-apps", "fluentbit-shop-debug", "otelcol-agent", "prometheus-scrape"}; !slices.Equal(m.Pipelines, want) {
		t.Errorf("pipelines %v, want %v", m.Pipelines, want)
	}
	if bytes.Contains(b, []byte(`"expr"`)) || bytes.Contains(b, []byte("rate(")) {
		t.Error("manifest holds query text (ADR 0003: names only)")
	}

	byName := map[string]report.ProtectedSignal{}
	for _, s := range m.Signals {
		if s.Kind == "metric" || s.Kind == "stream" {
			byName[s.Name] = s
		}
	}
	ruleOutput := map[string]bool{}
	if n, ok := gt.Pipelines["rule_outputs"]; ok {
		var outs []string
		if err := n.Decode(&outs); err != nil {
			t.Fatal(err)
		}
		for _, r := range outs {
			ruleOutput[r] = true
		}
	}
	protected := map[string]string{"hard_keep": "hard_keep", "keep_high": "keep_high", "dormant_confirm": "dormant"}
	check := func(kind string, entries map[string]gtEntry) {
		t.Helper()
		for name, e := range entries {
			if strings.Contains(name, "*") || ruleOutput[name] {
				continue
			}
			s, in := byName[name]
			want, isProtected := protected[e.Class]
			switch {
			case isProtected && !in:
				t.Errorf("%s %s (%s) is not in the manifest", kind, name, e.Class)
			case isProtected && s.Class != want:
				t.Errorf("%s %s: class %s, want %s", kind, name, s.Class, want)
			case isProtected && s.Class == "hard_keep" && len(s.Protectors) == 0:
				t.Errorf("%s %s: hard keep with no protectors", kind, name)
			case !isProtected && in:
				t.Errorf("%s %s (%s) is in the manifest; only protected signals belong there", kind, name, e.Class)
			}
		}
	}
	check("metric", gt.Metrics)
	// Stream signals are selectors, so a protected stream is covered when a
	// protected stream signal in the manifest selects it. Unmapped streams
	// (the reconciler's) are in no manifest.
	unmapped := map[string]bool{}
	if n, ok := gt.Pipelines["unmapped"]; ok {
		var u gtPipeline
		if err := n.Decode(&u); err != nil {
			t.Fatal(err)
		}
		for _, s := range u.Streams {
			unmapped[s] = true
		}
	}
	for key, e := range gt.Streams {
		if _, isProtected := protected[e.Class]; !isProtected || unmapped[key] {
			continue
		}
		labels := streamLabels(t, key)
		covered := false
		for _, s := range m.Signals {
			if s.Kind == "stream" && selectorMatches(t, s.Name, labels) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("stream %s (%s): no protected stream signal in the manifest selects it", key, e.Class)
		}
	}
	if s := byName["shop_search_requests_total"]; s.Class != "keep_high" || s.Pipeline != "alloy-apps" {
		t.Errorf("renamed metric shop_search_requests_total: %+v, want keep_high in alloy-apps", s)
	}
	for _, s := range m.Signals {
		if s.Pipeline == "" || !slices.Contains(m.Pipelines, s.Pipeline) {
			t.Errorf("%s: pipeline %q not one of the repo's", s.ID, s.Pipeline)
		}
	}
}
