package vcs

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
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testRepo makes a bare origin with one commit on main holding files, and a
// clone of it; it returns the clone and the origin.
func testRepo(t *testing.T, files map[string]string) (clone, origin string) {
	t.Helper()
	dir := t.TempDir()
	origin = filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	run(t, dir, "git", "init", "--quiet", "--bare", "--initial-branch=main", origin)
	run(t, dir, "git", "init", "--quiet", "--initial-branch=main", seed)
	for f, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(seed, f)), 0o755)
		os.WriteFile(filepath.Join(seed, f), []byte(c), 0o644)
	}
	run(t, seed, "git", "add", "-A")
	run(t, seed, "git", "commit", "--quiet", "-m", "seed")
	run(t, seed, "git", "push", "--quiet", origin, "main")
	clone = filepath.Join(dir, "clone")
	run(t, dir, "git", "clone", "--quiet", origin, clone)
	return clone, origin
}

func TestPush(t *testing.T) {
	clone, origin := testRepo(t, map[string]string{"cfg/a.yml": "a: 1\n"})
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	os.WriteFile(filepath.Join(clone, "local.txt"), []byte("untracked work\n"), 0o644)
	patch := "--- a/cfg/a.yml\n+++ b/cfg/a.yml\n@@ -1 +1,2 @@\n a: 1\n+b: 2\n"
	ctx := context.Background()
	base, err := Push(ctx, Branch{Checkout: clone, Name: "tvt/propose", Patch: patch, Message: "tvt propose: changes from scan 3"})
	if err != nil {
		t.Fatal(err)
	}
	if base != "main" {
		t.Errorf("base %q", base)
	}
	if got := run(t, origin, "git", "show", "tvt/propose:cfg/a.yml"); got != "a: 1\nb: 2" {
		t.Errorf("pushed file %q", got)
	}
	if got := run(t, origin, "git", "log", "-1", "--format=%s", "tvt/propose"); got != "tvt propose: changes from scan 3" {
		t.Errorf("message %q", got)
	}
	// The checkout's own tree is untouched, and a rerun replaces the branch.
	if b, _ := os.ReadFile(filepath.Join(clone, "cfg/a.yml")); string(b) != "a: 1\n" {
		t.Errorf("checkout changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(clone, "local.txt")); err != nil {
		t.Error(err)
	}
	patch2 := "--- a/cfg/a.yml\n+++ b/cfg/a.yml\n@@ -1 +1,2 @@\n a: 1\n+c: 3\n"
	if _, err := Push(ctx, Branch{Checkout: clone, Name: "tvt/propose", Patch: patch2, Message: "tvt propose: changes from scan 4"}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, origin, "git", "show", "tvt/propose:cfg/a.yml"); got != "a: 1\nc: 3" {
		t.Errorf("rerun file %q", got)
	}
	if n := run(t, origin, "git", "rev-list", "--count", "main..tvt/propose"); n != "1" {
		t.Errorf("rerun stacked commits: %s", n)
	}
	if _, err := Push(ctx, Branch{Checkout: clone, Name: "tvt/propose", Patch: "--- a/nope\n+++ b/nope\n@@ -1 +1 @@\n-x\n+y\n", Message: "m"}); err == nil {
		t.Error("a patch that doesn't apply was pushed")
	}
	// With a revert branch: it sits on the change and undoes it.
	if _, err := Push(ctx, Branch{Checkout: clone, Name: "tvt/propose", Revert: "tvt/propose-revert", Patch: patch, Message: "tvt propose: changes from scan 5"}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, origin, "git", "show", "tvt/propose-revert:cfg/a.yml"); got != "a: 1" {
		t.Errorf("revert branch file %q, want the base's", got)
	}
	if got := run(t, origin, "git", "rev-parse", "tvt/propose-revert~1"); got != run(t, origin, "git", "rev-parse", "tvt/propose") {
		t.Error("the revert branch isn't one commit on top of the change")
	}
	if _, err := Push(ctx, Branch{Checkout: clone, Name: "tvt/propose", Revert: "tvt/propose", Patch: patch, Message: "m"}); err == nil {
		t.Error("a revert branch named like the change was accepted")
	}
	// A base or branch that reads as a git option never reaches git.
	pwned := filepath.Join(t.TempDir(), "pwned")
	for _, b := range []Branch{
		{Checkout: clone, Name: "tvt/propose", Base: "--upload-pack=touch " + pwned, Patch: patch, Message: "m"},
		{Checkout: clone, Name: "--force", Patch: patch, Message: "m"},
		{Checkout: clone, Name: "tvt/propose", Revert: "-x", Patch: patch, Message: "m"},
		{Checkout: clone, Name: "", Patch: patch, Message: "m"},
	} {
		if _, err := Push(ctx, b); err == nil || !strings.Contains(err.Error(), "bad branch name") {
			t.Errorf("Push(base %q, name %q, revert %q) = %v, want a bad branch name", b.Base, b.Name, b.Revert, err)
		}
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Error("a base branch ran a command through git")
	}
}

// fakeGitHub records requests and answers like the pulls API.
type fakeGitHub struct {
	mu   sync.Mutex
	reqs []string
	open bool
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	b, _ := json.Marshal(body)
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(b))
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
		if f.open {
			w.Write([]byte(`[{"number":7,"html_url":"https://github.com/o/r/pull/7"}]`))
			return
		}
		w.Write([]byte(`[]`))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
		f.open = true
		w.WriteHeader(201)
		w.Write([]byte(`{"number":7,"html_url":"https://github.com/o/r/pull/7"}`))
	case r.Method == "PATCH":
		w.Write([]byte(`{"html_url":"https://github.com/o/r/pull/7"}`))
	case strings.HasSuffix(r.URL.Path, "/requested_reviewers"):
		w.WriteHeader(422)
		w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","field":"reviewers","code":"custom","message":"Reviews may only be requested from collaborators."}]}`))
	default:
		w.WriteHeader(404)
	}
}

func TestOpenOrUpdate(t *testing.T) {
	f := &fakeGitHub{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	g := GitHub{BaseURL: srv.URL, Token: "tok"}
	pr := PR{Owner: "o", Repo: "r", Head: "tvt/propose", Base: "main", Title: "t", Body: "b", Reviewers: []string{"@alice", "@o/sre", "@other/team"}}
	ctx := context.Background()
	res, err := g.OpenOrUpdate(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Number != 7 || !strings.Contains(res.Note, "collaborators") {
		t.Errorf("%+v", res)
	}
	all := strings.Join(f.reqs, "\n")
	for _, want := range []string{"GET /repos/o/r/pulls?head=o%3Atvt%2Fpropose&state=open", `"draft":true`, `"base":"main"`, `"reviewers":["alice"]`, `"team_reviewers":["sre"]`} {
		if !strings.Contains(all, want) {
			t.Errorf("no %s in\n%s", want, all)
		}
	}
	res, err = g.OpenOrUpdate(ctx, pr)
	if err != nil || res.Created || res.Number != 7 || !strings.Contains(f.reqs[len(f.reqs)-1], "PATCH /repos/o/r/pulls/7") {
		t.Errorf("update: %+v %v\n%s", res, err, f.reqs[len(f.reqs)-1])
	}
	if _, err := (GitHub{BaseURL: srv.URL, Token: "bad"}).OpenOrUpdate(ctx, pr); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad token: %v", err)
	}
	if _, err := (GitHub{BaseURL: srv.URL}).OpenOrUpdate(ctx, pr); err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("no token: %v", err)
	}
}

func TestParseRepo(t *testing.T) {
	for in, want := range map[string]string{"github.com/o/r": "o/r", "https://github.com/o/r.git": "o/r"} {
		o, n, err := ParseRepo(in)
		if err != nil || o+"/"+n != want {
			t.Errorf("%s: %s/%s %v", in, o, n, err)
		}
	}
	if _, _, err := ParseRepo("gitlab.com/o/r"); err == nil {
		t.Error("gitlab accepted")
	}
}
