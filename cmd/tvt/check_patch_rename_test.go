package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/check"
	"github.com/opsmastery/tvt/pkg/report"
)

// TestPatchedMovesAndDeletes: in --patch mode, a patch that moves a
// pipeline file outside the manifest's paths, or deletes it, leaves the
// old path gone, and check refuses (S16). A rename used to be skipped
// whole, so the file looked unchanged and check passed.
func TestPatchedMovesAndDeletes(t *testing.T) {
	const cfg = "receivers:\n  otlp:\n    protocols:\n      http:\nexporters:\n  debug:\nservice:\n  pipelines:\n    traces:\n      receivers: [otlp]\n      exporters: [debug]\n"
	now := time.Now()
	m := report.Manifest{Version: 2, Repo: "r", GeneratedAt: now,
		PipelineDefs: []report.PipelineDef{{Name: "agent", Kind: "otelcol", Files: []string{"otelcol/config.yaml"}}},
		Signals: []report.ProtectedSignal{{ID: "tempo:span:checkout", Kind: "span",
			Name: "checkout", Pipeline: "agent", Class: "keep_high"}}}
	for name, change := range map[string][]string{
		"moved":          {"mv", "otelcol/config.yaml", "otelcol/old-config.yaml"},
		"moved and edit": {"mv", "otelcol/config.yaml", "elsewhere/config.yaml"},
		"deleted":        {"rm", "-q", "otelcol/config.yaml"},
	} {
		t.Run(name, func(t *testing.T) {
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
			git("init", "-q")
			if err := os.MkdirAll(filepath.Join(repo, "otelcol"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(repo, "elsewhere"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "otelcol/config.yaml"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", "-A")
			git("commit", "-q", "-m", "base")
			git(change...)
			if name == "moved and edit" {
				if err := os.WriteFile(filepath.Join(repo, "elsewhere/config.yaml"), []byte(cfg+"# moved\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				git("add", "-A")
			}
			patch := filepath.Join(t.TempDir(), "change.patch")
			if err := os.WriteFile(patch, git("diff", "--cached", "-M", "--binary"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("reset", "-q", "--hard")

			head, cleanup, err := patched(repo, patch, m)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if _, err := head("otelcol/config.yaml"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the old path is still there after the patch (err %v)", err)
			}
			r := check.Run(check.Input{Manifest: m, Base: dirFiles(repo), Head: head, Now: now, SkipConfirmation: true})
			if r.Pass() {
				t.Errorf("check passed a patch that removes the pipeline file")
			}
		})
	}
}
