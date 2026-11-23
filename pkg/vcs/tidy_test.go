package vcs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// TestTidy: a new proposal settles tvt's earlier PRs. The S11 lab run
// found the old fixed branch names let a new proposal rewrite the open
// revert PR of a merged change, which then reverted the wrong thing.
func TestTidy(t *testing.T) {
	type pr struct {
		n            int
		head, base   string
		fork, merged bool
		open         bool
	}
	prs := []*pr{
		{n: 1, head: "tvt/propose-scan-1", base: "main"},                                  // closed unmerged
		{n: 2, head: "tvt/propose-scan-1-revert", base: "tvt/propose-scan-1", open: true}, // C: nothing to revert
		{n: 3, head: "tvt/propose-scan-2", base: "main", merged: true},                    // merged
		{n: 4, head: "tvt/propose-scan-2-revert", base: "tvt/propose-scan-2", open: true}, // B: retarget
		{n: 5, head: "tvt/propose-scan-3", base: "main", open: true},                      // A: superseded
		{n: 6, head: "tvt/propose-scan-3-revert", base: "tvt/propose-scan-3", open: true}, // A: its revert
		{n: 7, head: "tvt/propose-scan-4-revert", base: "main", open: true},               // D: already on main
		{n: 8, head: "tvt/propose-scan-5", base: "main", open: true},                      // E: this scan
		{n: 9, head: "tvt/propose-scan-5-revert", base: "tvt/propose-scan-5", open: true}, // E
		{n: 10, head: "tvt/propose-scan-9", base: "main", open: true, fork: true},         // F: a fork's
		{n: 11, head: "feature/x", base: "main", open: true},                              // not tvt's
		{n: 12, head: "tvt/propose", base: "main", merged: true},                          // legacy, merged
		{n: 13, head: "tvt/propose-revert", base: "tvt/propose", open: true},              // legacy revert: retarget
	}
	var mu sync.Mutex
	var acts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			var out []map[string]any
			head := strings.TrimPrefix(r.URL.Query().Get("head"), "o:")
			state := r.URL.Query().Get("state")
			for _, p := range prs {
				if head != "" && p.head != head {
					continue
				}
				if (state == "open") != p.open {
					continue
				}
				repo := "o/r"
				if p.fork {
					repo = "someone/r"
				}
				m := map[string]any{"number": p.n, "head": map[string]any{"ref": p.head, "repo": map[string]any{"full_name": repo}}, "base": map[string]any{"ref": p.base}}
				if p.merged {
					m["merged_at"] = "2026-11-21T22:38:38Z"
				} else {
					m["merged_at"] = nil
				}
				out = append(out, m)
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == "POST" && strings.Contains(r.URL.Path, "/comments"):
			w.Write([]byte(`{}`))
		case r.Method == "PATCH":
			var n int
			fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%d", &n)
			if body["state"] == "closed" {
				acts = append(acts, fmt.Sprintf("close #%d", n))
			}
			if b, ok := body["base"].(string); ok {
				acts = append(acts, fmt.Sprintf("retarget #%d %s", n, b))
			}
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g := GitHub{BaseURL: srv.URL, Token: "tok"}
	notes, err := g.Tidy(context.Background(), "o", "r", ChangeBranch(5), "main")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(acts)
	want := []string{"close #2", "close #5", "close #6", "retarget #13 main", "retarget #4 main"}
	if strings.Join(acts, ", ") != strings.Join(want, ", ") {
		t.Errorf("actions %v, want %v\nnotes:\n%s", acts, want, strings.Join(notes, "\n"))
	}
	if len(notes) != len(want) {
		t.Errorf("%d notes for %d actions: %v", len(notes), len(want), notes)
	}
	if ChangeBranch(12) != "tvt/propose-scan-12" || RevertBranch(12) != "tvt/propose-scan-12-revert" {
		t.Errorf("branches %s, %s", ChangeBranch(12), RevertBranch(12))
	}
}
