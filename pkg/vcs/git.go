package vcs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/opsmastery/tvt/internal/safe"
)

// Branch is a change to push as a branch: Patch (git apply format,
// relative to the repo root) applied on Base and committed as Message.
type Branch struct {
	Checkout string // a local clone of the repo, with an origin remote
	Base     string // "" is origin's default branch
	Name     string
	Patch    string
	Message  string
	// Revert, when set, is a second branch pushed on top of Name with a
	// commit that undoes the change (S10 item 6: every drop comes with
	// its revert).
	Revert string
}

// Push applies b.Patch on origin's base in a temporary worktree (the
// checkout's own working tree is never touched), commits it and pushes
// the branch to origin, replacing an earlier push of the same branch
// (tvt regenerates its blocks; the branch is tvt's). It returns the base
// used. Pushing uses the checkout's own Git credentials.
func Push(ctx context.Context, b Branch) (base string, err error) {
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("vcs: git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}
	// Every name reaches a git command line: one starting with "-" would
	// be read as an option (git fetch --upload-pack= runs a command).
	for i, n := range []string{b.Name, b.Revert, b.Base} {
		if n == "" && i > 0 { // Revert and Base are optional
			continue
		}
		if err := safe.Ref(n); err != nil {
			return "", fmt.Errorf("vcs: bad branch name: %w", err)
		}
	}
	if b.Revert == b.Name {
		return "", fmt.Errorf("vcs: the revert branch can't be the change's own branch")
	}
	base = b.Base
	if base == "" {
		ref, err := git(b.Checkout, "ls-remote", "--symref", "origin", "HEAD")
		if err != nil {
			return "", err
		}
		for _, l := range strings.Split(ref, "\n") {
			if rest, ok := strings.CutPrefix(l, "ref: refs/heads/"); ok {
				base, _, _ = strings.Cut(rest, "\t")
			}
		}
		if base == "" {
			return "", fmt.Errorf("vcs: origin has no default branch; set pipelines[].branch")
		}
		if err := safe.Ref(base); err != nil {
			return "", fmt.Errorf("vcs: origin's default branch: %w", err)
		}
	}
	if _, err := git(b.Checkout, "fetch", "--quiet", "origin", base); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "tvt-pr-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	wt := filepath.Join(tmp, "wt")
	if _, err := git(b.Checkout, "worktree", "add", "--quiet", "--detach", wt, "FETCH_HEAD"); err != nil {
		return "", err
	}
	defer git(b.Checkout, "worktree", "remove", "--force", wt) //nolint:errcheck
	patch := filepath.Join(tmp, "change.patch")
	if err := os.WriteFile(patch, []byte(b.Patch), 0o600); err != nil {
		return "", err
	}
	if _, err := git(wt, "apply", "--index", patch); err != nil {
		return "", err
	}
	if _, err := git(wt, "commit", "--quiet", "-m", b.Message); err != nil {
		return "", err
	}
	if _, err := git(wt, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/"+b.Name); err != nil {
		return "", err
	}
	if b.Revert != "" {
		if _, err := git(wt, "revert", "--no-edit", "HEAD"); err != nil {
			return "", err
		}
		if _, err := git(wt, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/"+b.Revert); err != nil {
			return "", err
		}
	}
	return base, nil
}
