package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/check"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"go.yaml.in/yaml/v3"
)

const labRepo = "github.com/Opsmastery-Summit/tvt-lab"

// labManifest is tvt-lab's manifest from the writers-2 benchmark state
// (the newest capture), with pipeline defs from its tvt-lab.yaml. That
// capture predates tvt-lab's S8 seeds, so the metric the count connector
// makes is added as the next scan will find it: a hard keep that
// ShopErrorLogBurst reads.
// It is built once (a benchmark DB and a report take seconds, minutes
// under -race) and each caller gets its own copy to change.
func labManifest(t *testing.T) report.Manifest {
	t.Helper()
	labOnce.Do(func() { labJSON, labErr = buildLabManifest(t) })
	if labErr != nil {
		t.Fatal(labErr)
	}
	var m report.Manifest
	if err := json.Unmarshal(labJSON, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

var (
	labOnce sync.Once
	labJSON []byte
	labErr  error
)

func buildLabManifest(t *testing.T) ([]byte, error) {
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
		return nil, err
	}
	cfg, err := config.Load(filepath.Join(dirs[st.Name], "tvt-lab.yaml"))
	if err != nil {
		return nil, err
	}
	m, ok := buildManifests(cfg, in)[labRepo]
	if !ok {
		return nil, fmt.Errorf("no manifest for %s", labRepo)
	}
	m.Signals = append(m.Signals, report.ProtectedSignal{ID: "prom:metric:shop_error_log_lines_total", Kind: "metric",
		Name: "shop_error_log_lines_total", Pipeline: "otelcol-agent", Class: "hard_keep",
		Protectors: []report.Protector{{Kind: "alert_rule", Name: "ShopErrorLogBurst", Evidence: "declared", Exact: true}}})
	return json.Marshal(m)
}

type checkExpect struct {
	Check map[string]struct {
		File    string   `yaml:"file"`
		Item    int      `yaml:"item"`
		Expect  string   `yaml:"expect"`
		Reasons []string `yaml:"reasons"`
	} `yaml:"check"`
}

// The rules each S8 item's refusals must come from (the reasons in
// ground truth are prose; the rule is what check can be held to).
var itemRules = map[int][]string{
	5:  {check.RuleScrape},
	7:  {check.RuleAttrUnset},
	8:  {check.RuleFailClosed},
	9:  {check.RulePipelineRead},
	10: {check.RulePipelineRead},
	13: {check.RuleRecord},
}

// TestCheckLabFixtures is S8's done-when on tvt-lab's fixtures: every
// unsafe change in ground truth is refused for the right reason, and the
// safe ones pass. Safety is judged without the owner-confirmation rule
// (TestCheckConfirmation covers it).
func TestCheckLabFixtures(t *testing.T) {
	m := labManifest(t)
	m.GeneratedAt = time.Now()
	var exp checkExpect
	b, err := os.ReadFile("../../testdata/check/expect.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, &exp); err != nil {
		t.Fatal(err)
	}
	if len(exp.Check) != 11 {
		t.Fatalf("%d fixtures, want tvt-lab's 11", len(exp.Check))
	}
	lab := "../../testdata/check/lab"
	names := make([]string, 0, len(exp.Check))
	for n := range exp.Check {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		c := exp.Check[name]
		t.Run(name, func(t *testing.T) {
			head, cleanup, err := patched(lab, filepath.Join("../../testdata/check/fixtures", filepath.Base(c.File)), m)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			r := check.Run(check.Input{Manifest: m, Base: dirFiles(lab), Head: head, Now: time.Now(), SkipConfirmation: true,
				Objectives: map[string]time.Duration{"critical": 15 * time.Minute, "warning": time.Hour}})
			var lines []string
			for _, f := range r.Findings {
				lines = append(lines, f.String())
			}
			switch c.Expect {
			case "accept":
				for _, f := range r.Findings {
					if f.Rule != check.RuleGap {
						t.Errorf("refused a safe change:\n  %s", strings.Join(lines, "\n  "))
						break
					}
				}
			case "refuse":
				want := itemRules[c.Item]
				ok := false
				for _, f := range r.Findings {
					for _, w := range want {
						if f.Rule == w {
							ok = true
						}
					}
				}
				if !ok {
					t.Errorf("want a %v finding (item %d: %s); got:\n  %s", want, c.Item, strings.Join(c.Reasons, "; "), strings.Join(lines, "\n  "))
				}
			}
			t.Logf("%s:\n  %s", c.Expect, strings.Join(lines, "\n  "))
		})
	}
}

// TestCheckUnchanged: a change that changes nothing passes, even with
// tvt-lab's existing experimental statement and connectors in place.
func TestCheckUnchanged(t *testing.T) {
	m := labManifest(t)
	m.GeneratedAt = time.Now()
	lab := dirFiles("../../testdata/check/lab")
	r := check.Run(check.Input{Manifest: m, Base: lab, Head: lab, Now: time.Now()})
	if !r.Pass() {
		t.Errorf("refused no change: %v", r.Findings)
	}
}

// TestCheckConfirmation: a drop nothing protects still needs its owner's
// confirmation, and one recorded in the manifest is enough.
func TestCheckConfirmation(t *testing.T) {
	m := labManifest(t)
	m.GeneratedAt = time.Now()
	m.Gaps = nil
	lab := "../../testdata/check/lab"
	head, cleanup, err := patched(lab, "../../testdata/check/fixtures/log-drop-after-experimental.patch", m)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	in := check.Input{Manifest: m, Base: dirFiles(lab), Head: head, Now: time.Now()}
	r := check.Run(in)
	unconfirmed := 0
	for _, f := range r.Findings {
		if f.Rule == check.RuleUnconfirmed {
			unconfirmed++
		}
	}
	if unconfirmed == 0 {
		t.Errorf("no unconfirmed_drop finding: %v", r.Findings)
	}
}

// TestCheckStale: an old manifest fails the check whatever the change.
func TestCheckStale(t *testing.T) {
	m := labManifest(t)
	lab := dirFiles("../../testdata/check/lab")
	r := check.Run(check.Input{Manifest: m, Base: lab, Head: lab, Now: m.GeneratedAt.Add(8 * 24 * time.Hour)})
	if r.Pass() || r.Findings[0].Rule != check.RuleStaleManifest {
		t.Errorf("an 8-day-old manifest passed: %v", r.Findings)
	}
}

// TestCheckProposePatch: tvt's own proposal for writers-2 drops only
// unread data, so it's safe; and a hand edit that drops a hard keep is
// refused.
func TestCheckProposePatch(t *testing.T) {
	m := labManifest(t)
	m.GeneratedAt = time.Now()
	_, dirs := loadBenchmark(t)
	checkout := filepath.Join(dirs["writers-2"], "checkout")
	patch := os.Getenv("TVT_WRITERS2_PATCH")
	if patch == "" {
		t.Skip("set TVT_WRITERS2_PATCH to the patch TestWriters2 writes")
	}
	head, cleanup, err := patched(checkout, patch, m)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	r := check.Run(check.Input{Manifest: m, Base: dirFiles(checkout), Head: head, Now: time.Now(), SkipConfirmation: true})
	for _, f := range r.Findings {
		if f.Rule != check.RuleGap {
			t.Errorf("tvt's own patch refused: %s", f)
		}
	}
}

func TestCheckHardKeepDrop(t *testing.T) {
	m := labManifest(t)
	m.GeneratedAt = time.Now()
	_, dirs := loadBenchmark(t)
	checkout := filepath.Join(dirs["writers-2"], "checkout")
	src, err := os.ReadFile(filepath.Join(checkout, "alloy/config.alloy"))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(src), `prometheus.relabel "apps" {`, `prometheus.relabel "apps" {
  rule {
    source_labels = ["__name__"]
    regex         = "shop_http_requests_total|shop_payment_queue_depth"
    action        = "drop"
  }`, 1)
	if edited == string(src) {
		t.Fatal("no prometheus.relabel \"apps\" in the writers-2 checkout")
	}
	head := func(p string) ([]byte, error) {
		if p == "alloy/config.alloy" {
			return []byte(edited), nil
		}
		return dirFiles(checkout)(p)
	}
	r := check.Run(check.Input{Manifest: m, Base: dirFiles(checkout), Head: head, Now: time.Now(), SkipConfirmation: true})
	got := map[string]bool{}
	for _, f := range r.Findings {
		if f.Rule == check.RuleProtected {
			got[f.Signal] = true
		}
	}
	for _, want := range []string{"prom:metric:shop_http_requests_total", "prom:metric:shop_payment_queue_depth"} {
		if !got[want] {
			t.Errorf("%s not refused: %v", want, r.Findings)
		}
	}
}

// TestCheckCLI runs tvt check end to end on a copy of the lab with its
// manifest: a refused fixture exits 4 with the finding, a safe one exits 0.
func TestCheckCLI(t *testing.T) {
	m := labManifest(t)
	m.GeneratedAt = time.Now()
	m.Gaps = nil
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../testdata/check/lab")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".tvt"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, ".tvt", "protect.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	fx, _ := filepath.Abs("../../testdata/check/fixtures")
	for _, c := range []struct {
		fixture string
		code    int
		want    string
	}{
		{"span-delete-tool-call-id.patch", exitRefused, "deletes the outcome field gen_ai.tool.call.id"},
		{"span-cap-tool-parameters.patch", exitOK, "PASS"},
	} {
		var out, errb strings.Builder
		code := run([]string{"check", "--repo", dir, "--patch", filepath.Join(fx, c.fixture), "--skip-confirmation"}, &out, &errb)
		if code != c.code || !strings.Contains(out.String(), c.want) {
			t.Errorf("%s: exit %d, want %d with %q:\n%s%s", c.fixture, code, c.code, c.want, out.String(), errb.String())
		}
	}
}
