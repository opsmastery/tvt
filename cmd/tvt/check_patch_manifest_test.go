package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/report"
)

// TestCheckPatchWithManifest: --patch judges a patch that also changes the
// manifest as --base judges a PR. A move with the manifest regenerated
// passes (S17); a drop that also removes the signal from the manifest is
// refused against the manifest before the patch (S15).
func TestCheckPatchWithManifest(t *testing.T) {
	const cfg = "receivers:\n  otlp:\n    protocols:\n      http:\nprocessors:\n  batch:\nexporters:\n  debug:\nservice:\n  pipelines:\n    metrics:\n      receivers: [otlp]\n      processors: [batch]\n      exporters: [debug]\n"
	const drop = "receivers:\n  otlp:\n    protocols:\n      http:\nprocessors:\n  filter/x:\n    metrics:\n      metric:\n        - 'name == \"checkout_total\"'\nexporters:\n  debug:\nservice:\n  pipelines:\n    metrics:\n      receivers: [otlp]\n      processors: [filter/x]\n      exporters: [debug]\n"
	now := time.Now().UTC()
	manifest := func(file string, signals bool) []byte {
		m := report.Manifest{Version: 2, Repo: "r", GeneratedAt: now,
			PipelineDefs: []report.PipelineDef{{Name: "agent", Kind: "otelcol", Files: []string{file}}},
			Signals:      []report.ProtectedSignal{}}
		if signals {
			m.Signals = []report.ProtectedSignal{{ID: "prom:metric:checkout_total", Kind: "metric", Name: "checkout_total", Pipeline: "agent", Class: "hard_keep"}}
		}
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, repo string, write func(string, []byte), git func(...string) []byte)
		pass   bool
		reason string
	}{
		{"move with the manifest regenerated", func(t *testing.T, repo string, write func(string, []byte), git func(...string) []byte) {
			git("mv", "otelcol/config.yaml", "otelcol/agent.yaml")
			write(".tvt/protect.json", manifest("otelcol/agent.yaml", true))
		}, true, ""},
		{"move, manifest unchanged", func(t *testing.T, repo string, write func(string, []byte), git func(...string) []byte) {
			git("mv", "otelcol/config.yaml", "otelcol/agent.yaml")
		}, false, ""},
		{"drop and unprotect in one patch", func(t *testing.T, repo string, write func(string, []byte), git func(...string) []byte) {
			write("otelcol/config.yaml", []byte(drop))
			write(".tvt/protect.json", manifest("otelcol/config.yaml", false))
		}, false, "protected in the manifest before this change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			git := func(args ...string) []byte {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = repo
				cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
					"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return out
			}
			write := func(p string, b []byte) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, p)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, p), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			git("init", "-q")
			write("otelcol/config.yaml", []byte(cfg))
			write(".tvt/protect.json", manifest("otelcol/config.yaml", true))
			git("add", "-A")
			git("commit", "-q", "-m", "base")
			tc.change(t, repo, write, git)
			git("add", "-A")
			patch := filepath.Join(t.TempDir(), "change.patch")
			if err := os.WriteFile(patch, git("diff", "--cached", "-M"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("reset", "-q", "--hard")

			r, err := runCheck(checkOptions{Manifest: ".tvt/protect.json", Repo: repo, Patch: patch, MaxAge: time.Hour, Unconfirmed: true, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if r.Pass() != tc.pass {
				t.Fatalf("pass = %v, want %v: %v", r.Pass(), tc.pass, r.Findings)
			}
			if tc.reason != "" {
				found := false
				for _, f := range r.Findings {
					if strings.Contains(f.Reason, tc.reason) {
						found = true
					}
				}
				if !found {
					t.Errorf("no finding says %q: %v", tc.reason, r.Findings)
				}
			}
		})
	}
}
