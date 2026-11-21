package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/config"
)

// TestReadMerges: a pipeline checkout holding a committed
// .tvt/proposal.json (a merged tvt PR) gives one merge per recommendation,
// at the file's commit time; no proposal gives none, and an uncommitted
// one is reported (S11 item 1).
func TestReadMerges(t *testing.T) {
	dir := t.TempDir()
	git := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), append([]string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}, env...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(nil, "init", "-q", "-b", "main")
	pipes := []config.Pipeline{{Name: "p", Repo: "github.com/o/pipes", Checkout: dir}, {Name: "q", Repo: "github.com/o/pipes", Checkout: dir}}
	ctx := context.Background()
	if ms, errs := readMerges(ctx, pipes); len(ms) != 0 || len(errs) != 0 {
		t.Fatalf("no proposal: %v, %v", ms, errs)
	}
	os.MkdirAll(filepath.Join(dir, ".tvt"), 0o755)
	os.WriteFile(filepath.Join(dir, ".tvt", "proposal.json"), []byte(`{"scan": 7, "pipelines": {"p": [
		{"recommendation": "drop:prom:metric:a", "signal": "prom:metric:a", "evidence": "x", "expected": {"monthly_saving": 2.5}},
		{"recommendation": "level_tier:loki:stream:{service_name=\"s\"}", "signal": "loki:stream:{service_name=\"s\"}", "evidence": "y", "expected": {}}]}}`), 0o644)
	if _, errs := readMerges(ctx, pipes); len(errs) != 1 || !strings.Contains(errs[0], "aren't in the ledger") {
		t.Errorf("uncommitted: %v", errs)
	}
	at := time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC)
	git(nil, "add", ".")
	git([]string{"GIT_COMMITTER_DATE=" + at.Format(time.RFC3339), "GIT_AUTHOR_DATE=" + at.Format(time.RFC3339)}, "commit", "-q", "-m", "merge tvt/propose")
	ms, errs := readMerges(ctx, pipes)
	if len(errs) != 0 || len(ms) != 2 {
		t.Fatalf("merges %+v, errs %v", ms, errs)
	}
	if m := ms[0]; m.Repo != "github.com/o/pipes" || m.ProposalScan != 7 || m.Recommendation != "drop:prom:metric:a" || !m.MergedAt.Equal(at) || m.Expected != 2.5 {
		t.Errorf("merge %+v", m)
	}

	// The checkout on the PR's branch, with a newer proposal that hasn't
	// merged: with no origin HEAD, tvt refuses to guess; with one, only
	// the default branch's proposal counts.
	git(nil, "checkout", "-q", "-b", "tvt/propose")
	os.WriteFile(filepath.Join(dir, ".tvt", "proposal.json"), []byte(`{"scan": 9, "pipelines": {"p": [
		{"recommendation": "drop:prom:metric:b", "signal": "prom:metric:b", "evidence": "z", "expected": {"monthly_saving": 9}}]}}`), 0o644)
	git(nil, "commit", "-q", "-am", "tvt: propose")
	if ms, errs := readMerges(ctx, pipes); len(ms) != 0 || len(errs) != 1 || !strings.Contains(errs[0], "tvt/propose") {
		t.Errorf("on the PR branch with no origin HEAD: merges %+v, errs %v", ms, errs)
	}
	git(nil, "update-ref", "refs/remotes/origin/main", "tvt/propose~1")
	git(nil, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	ms, errs = readMerges(ctx, pipes)
	if len(errs) != 0 || len(ms) != 2 || ms[0].ProposalScan != 7 {
		t.Errorf("on the PR branch with origin HEAD: merges %+v, errs %v", ms, errs)
	}

	// The PR merged with a merge commit an hour after its branch committed
	// the proposal: the merge time counts.
	mergedAt := at.Add(26 * time.Hour)
	dates := []string{"GIT_COMMITTER_DATE=" + mergedAt.Format(time.RFC3339), "GIT_AUTHOR_DATE=" + mergedAt.Format(time.RFC3339)}
	git(nil, "checkout", "-q", "main")
	git(dates, "merge", "-q", "--no-ff", "-m", "Merge tvt/propose", "tvt/propose")
	git(nil, "update-ref", "refs/remotes/origin/main", "main")
	ms, errs = readMerges(ctx, pipes)
	if len(errs) != 0 || len(ms) != 1 || ms[0].ProposalScan != 9 || !ms[0].MergedAt.Equal(mergedAt) {
		t.Errorf("merge commit: merges %+v, errs %v; want scan 9 merged at %v", ms, errs, mergedAt)
	}
}
