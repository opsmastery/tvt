package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/vcs"
)

// TestProposePR runs tvt propose --output pr on the writers state against
// a local Git origin and a fake GitHub API (S7 item 3): tvt pushes its
// branch with the same change as the patch and opens one draft PR for the
// lab repo with the recommendations and validation in its body; a rerun
// updates the PR instead of opening another.
func TestProposePR(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com"} {
		t.Setenv(k, v)
	}
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "writers" {
			st = s
		}
	}
	db := benchDB(t, st, dirs[st.Name])
	cfg := filepath.Join(dirs[st.Name], "tvt-lab.yaml")
	const repo = "github.com/Opsmastery-Summit/tvt-lab"

	// A Git origin holding the vendored checkout, and a clone of it.
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	tmp := t.TempDir()
	origin, seed, clone := filepath.Join(tmp, "origin.git"), filepath.Join(tmp, "seed"), filepath.Join(tmp, "clone")
	git(tmp, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	if err := os.CopyFS(seed, os.DirFS(filepath.Join(dirs[st.Name], "checkout"))); err != nil {
		t.Fatal(err)
	}
	git(seed, "init", "--quiet", "--initial-branch=main")
	git(seed, "add", "-A")
	git(seed, "commit", "--quiet", "-m", "seed")
	git(seed, "push", "--quiet", origin, "main")
	git(tmp, "clone", "--quiet", origin, clone)
	// The proposal's branches are named by scan (vcs.ChangeBranch).
	changeBranch := func() string {
		for _, b := range strings.Fields(git(origin, "for-each-ref", "--format=%(refname:short)", "refs/heads/tvt/")) {
			if !strings.HasSuffix(b, vcs.RevertSuffix) {
				return b
			}
		}
		t.Fatal("no tvt change branch pushed")
		return ""
	}

	var mu sync.Mutex
	var reqs []string
	open := map[string]bool{} // by head branch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		reqs = append(reqs, r.Method+" "+r.URL.Path+" "+string(b))
		switch {
		case r.Method == "GET":
			if open[strings.TrimPrefix(r.URL.Query().Get("head"), "Opsmastery-Summit:")] {
				w.Write([]byte(`[{"number":1,"html_url":"https://github.com/Opsmastery-Summit/tvt-lab/pull/1"}]`))
				return
			}
			w.Write([]byte(`[]`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			open[body["head"].(string)] = true
			w.WriteHeader(201)
			w.Write([]byte(`{"number":1,"html_url":"https://github.com/Opsmastery-Summit/tvt-lab/pull/1"}`))
		default:
			w.Write([]byte(`{"html_url":"https://github.com/Opsmastery-Summit/tvt-lab/pull/1"}`))
		}
	}))
	defer srv.Close()

	run := func() string {
		t.Helper()
		dir := t.TempDir()
		var out strings.Builder
		o := proposeOptions{DB: db, Config: cfg, Output: "pr", Dir: dir, Checkouts: checkoutFlag{repo: clone}, Validate: "off"}
		o.GitHub.BaseURL, o.GitHub.Token = srv.URL, "tok"
		if err := runPropose(context.Background(), o, &out); err != nil {
			t.Fatalf("propose: %v\n%s", err, out.String())
		}
		patch, err := os.ReadFile(filepath.Join(dir, report.ManifestDir(repo)+".patch"))
		if err != nil {
			t.Fatal(err)
		}
		// The pushed branch carries the patch's change.
		if got := git(origin, "show", changeBranch()+":prometheus/scrape/lab.yml"); !strings.Contains(got, "tvt:begin prometheus-scrape/") {
			t.Errorf("pushed scrape config has no tvt block:\n%s", got)
		}
		if !strings.Contains(string(patch), "tvt:begin prometheus-scrape/") {
			t.Error("patch has no tvt block")
		}
		return out.String()
	}
	out := run()
	if !strings.Contains(out, "draft PR opened for "+repo) {
		t.Errorf("no PR opened:\n%s", out)
	}
	all := strings.Join(reqs, "\n")
	for _, want := range []string{"POST /repos/Opsmastery-Summit/tvt-lab/pulls", `"draft":true`, `"head":"tvt/propose-scan-`, `"base":"main"`, "tvt:begin", "stays a draft",
		// The revert PR (S10 item 6), based on the change's branch.
		`-revert"`, `"base":"tvt/propose-scan-`, "Undoes tvt's telemetry changes"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in the API requests:\n%s", want, all)
		}
	}
	if !strings.Contains(out, "revert PR opened for "+repo) {
		t.Errorf("no revert PR opened:\n%s", out)
	}
	if got := git(origin, "show", changeBranch()+vcs.RevertSuffix+":prometheus/scrape/lab.yml"); strings.Contains(got, "tvt:begin") {
		t.Errorf("the revert branch still has tvt's block:\n%s", got)
	}
	out = run()
	if !strings.Contains(out, "draft PR updated for "+repo) || !strings.Contains(out, "revert PR updated for "+repo) || strings.Count(strings.Join(reqs, "\n"), "POST /repos/Opsmastery-Summit/tvt-lab/pulls ") != 2 {
		t.Errorf("rerun didn't update the PR:\n%s\n%s", out, strings.Join(reqs, "\n"))
	}
	if n := git(origin, "rev-list", "--count", "main.."+changeBranch()); n != "1" {
		t.Errorf("rerun stacked %s commits", n)
	}
}

// TestPRBodyFits: a first scan's thousand drops still fit GitHub's 65,536
// character limit on a PR description (the live lab run got a 422 without
// this), and the body says how many rows it left out.
func TestPRBodyFits(t *testing.T) {
	const repo = "github.com/o/r"
	var written []proposal
	for i := range 2000 {
		written = append(written, proposal{Repo: repo, Rec: report.Recommendation{
			ID:    "drop:prom:metric:go_godebug_non_default_behavior_" + strings.Repeat("x", i%40) + "_events_total",
			Claim: "In the 30 days to 2026-11-17, nothing read it: no panel, rule, alert or logged query; dropping it saves 0.01 USD a month."}})
	}
	b := prBody(repo, 1, written, []string{repo + " a.yml: valid (promtool)"}, nil)
	if len(b) > 65536 || !strings.Contains(b, "2000 recommendations") || !strings.Contains(b, "more aren't listed") || !strings.Contains(b, "a.yml: valid") {
		t.Errorf("body %d characters:\n%s", len(b), b[len(b)-400:])
	}
}
