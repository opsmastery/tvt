package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/opsmastery/tvt/internal/safe"
	"github.com/opsmastery/tvt/pkg/check"
	"github.com/opsmastery/tvt/pkg/report"
	"go.yaml.in/yaml/v3"
)

// exitRefused: tvt check ran and refused the change.
const exitRefused = 4

type checkOptions struct {
	Manifest string
	Repo     string
	Base     string
	Patch    string
	MaxAge   time.Duration
	JSON     bool
	// Unconfirmed leaves out the owner-confirmation rule: is the change
	// safe, whoever signs it off?
	Unconfirmed bool
	Now         time.Time
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	var o checkOptions
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: tvt check [flags]

Judges a change to a pipeline repo offline, against its protection
manifest (.tvt/protect.json, from tvt report --manifest-dir): it fails if
the change drops, samples or strips anything an alert, SLO, viewed
dashboard or record needs, or that tvt can't evaluate. The change is the
working tree against --base (in CI, the PR's base branch), or --patch
applied to the working tree. Exit 0: pass; 4: refused; 1: error.`)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.Manifest, "manifest", ".tvt/protect.json", "the protection manifest, relative to --repo")
	fs.StringVar(&o.Repo, "repo", ".", "the pipeline repo's checkout")
	fs.StringVar(&o.Base, "base", "", "git ref of the version before the change (origin/main); the working tree is the change")
	fs.StringVar(&o.Patch, "patch", "", "a patch to judge, applied to the working tree (instead of --base)")
	fs.DurationVar(&o.MaxAge, "max-age", 7*24*time.Hour, "how old the manifest may be")
	fs.BoolVar(&o.JSON, "json", false, "print the result as JSON")
	fs.BoolVar(&o.Unconfirmed, "skip-confirmation", false, "don't require an owner confirmation for drops (judge safety only)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 || (o.Base == "") == (o.Patch == "") {
		fmt.Fprintln(stderr, "tvt check: give exactly one of --base and --patch")
		fs.Usage()
		return exitUsage
	}
	o.Now = time.Now()
	r, err := runCheck(o)
	if err != nil {
		fmt.Fprintf(stderr, "tvt check: %v\n", err)
		return exitError
	}
	printCheck(r, o.JSON, stdout)
	if !r.Pass() {
		return exitRefused
	}
	return exitOK
}

func printCheck(r check.Result, asJSON bool, out io.Writer) {
	if asJSON {
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintln(out, string(b))
		return
	}
	for _, n := range r.Notes {
		fmt.Fprintln(out, n)
	}
	if r.Pass() {
		fmt.Fprintln(out, "PASS")
		return
	}
	fmt.Fprintf(out, "REFUSED: %d findings\n", len(r.Findings))
	for _, f := range r.Findings {
		fmt.Fprintf(out, "  %s\n", f)
	}
}

func runCheck(o checkOptions) (check.Result, error) {
	b, err := os.ReadFile(filepath.Join(o.Repo, o.Manifest))
	if err != nil {
		return check.Result{}, err
	}
	var m report.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return check.Result{}, fmt.Errorf("%s: %w", o.Manifest, err)
	}
	// The manifest is committed in the repo the change comes from: its
	// paths are joined to the checkout and a temporary copy, so one that
	// escapes the repo is refused before anything reads or writes it.
	if err := manifestPaths(m); err != nil {
		return check.Result{}, fmt.Errorf("%s: %w", o.Manifest, err)
	}
	if strings.HasPrefix(o.Base, "-") {
		return check.Result{}, fmt.Errorf("--base %q starts with -", o.Base)
	}
	tree := dirFiles(o.Repo)
	in := check.Input{Manifest: m, Now: o.Now, MaxAge: o.MaxAge, SkipConfirmation: o.Unconfirmed}
	if o.Patch != "" {
		mpath := filepath.ToSlash(o.Manifest)
		head, cleanup, err := patched(o.Repo, o.Patch, m, mpath)
		if err != nil {
			return check.Result{}, err
		}
		// A patch that also changes the manifest is judged as --base judges
		// a PR: against the manifest it brings, and against the one before
		// it, so regenerating protect.json with a move passes (S17) and
		// editing it can't lift a protection (S15). The patch is applied
		// again with the new manifest's paths too.
		hb, herr := head(mpath)
		switch {
		case errors.Is(herr, fs.ErrNotExist):
			cleanup()
			return check.Result{}, fmt.Errorf("%s: the patch deletes the manifest", o.Manifest)
		case herr != nil:
			cleanup()
			return check.Result{}, herr
		case !bytes.Equal(hb, b):
			var hm report.Manifest
			if err := json.Unmarshal(hb, &hm); err != nil {
				cleanup()
				return check.Result{}, fmt.Errorf("%s after the patch: %w", o.Manifest, err)
			}
			if err := manifestPaths(hm); err != nil {
				cleanup()
				return check.Result{}, fmt.Errorf("%s after the patch: %w", o.Manifest, err)
			}
			cleanup()
			if head, cleanup, err = patched(o.Repo, o.Patch, m, append(manifestFiles(hm), mpath)...); err != nil {
				return check.Result{}, err
			}
			base := m
			in.Manifest, in.BaseManifest = hm, &base
		}
		defer cleanup()
		in.Base, in.Head = tree, head
	} else {
		in.Base, in.Head = gitFiles(o.Repo, o.Base), tree
		// The base's own manifest: the change is judged against it too,
		// so editing protect.json in the same PR can't lift a protection.
		if b, err := in.Base(filepath.ToSlash(o.Manifest)); err == nil {
			var bm report.Manifest
			if err := json.Unmarshal(b, &bm); err != nil {
				return check.Result{}, fmt.Errorf("%s at %s: %w", o.Manifest, o.Base, err)
			}
			if err := manifestPaths(bm); err != nil {
				return check.Result{}, fmt.Errorf("%s at %s: %w", o.Manifest, o.Base, err)
			}
			in.BaseManifest = &bm
		} else if !errors.Is(err, fs.ErrNotExist) {
			return check.Result{}, err
		}
	}
	// Owner decisions and the PR's proposal (S9 item 2), as the change
	// leaves them.
	if b, err := in.Head(report.DecisionsPath); err == nil {
		var d report.Decisions
		if err := yaml.Unmarshal(b, &d); err != nil {
			return check.Result{}, fmt.Errorf("%s: %w", report.DecisionsPath, err)
		}
		in.Decisions = d.Decisions
	} else if !errors.Is(err, fs.ErrNotExist) {
		return check.Result{}, err
	}
	if b, err := in.Head(report.ProposalPath); err == nil {
		var p report.Proposal
		if err := json.Unmarshal(b, &p); err != nil {
			return check.Result{}, fmt.Errorf("%s: %w", report.ProposalPath, err)
		}
		in.Proposal = &p
	} else if !errors.Is(err, fs.ErrNotExist) {
		return check.Result{}, err
	}
	return check.Run(in), nil
}

// manifestPaths: every pipeline file the manifest names is inside the
// repo.
func manifestPaths(m report.Manifest) error {
	for _, p := range m.PipelineDefs {
		paths := slices.Clone(p.Files)
		if p.HelmValues != nil {
			paths = append(paths, p.HelmValues.File)
			if p.HelmValues.Defaults != "" {
				paths = append(paths, p.HelmValues.Defaults)
			}
		}
		if p.Build != "" {
			paths = append(paths, p.Build)
		}
		for _, f := range paths {
			if err := safe.RepoPath(f); err != nil {
				return fmt.Errorf("pipeline %q: %w", p.Name, err)
			}
		}
	}
	return nil
}

// dirFiles reads files under a directory.
func dirFiles(dir string) check.Files {
	return func(p string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
	}
}

// gitFiles reads files at a git ref; a path the ref doesn't have is
// fs.ErrNotExist.
func gitFiles(repo, ref string) check.Files {
	return func(p string) ([]byte, error) {
		cmd := exec.Command("git", "-C", repo, "show", ref+":"+p)
		out, err := cmd.Output()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) && (strings.Contains(string(ee.Stderr), "does not exist") || strings.Contains(string(ee.Stderr), "exists on disk, but not in")) {
				return nil, os.ErrNotExist
			}
			return nil, fmt.Errorf("git show %s:%s: %v", ref, p, err)
		}
		return out, nil
	}
}

// manifestFiles: the repo paths a manifest's pipelines read.
func manifestFiles(m report.Manifest) []string {
	var paths []string
	for _, p := range m.PipelineDefs {
		paths = append(paths, p.Files...)
		if p.HelmValues != nil {
			paths = append(paths, p.HelmValues.File)
			if p.HelmValues.Defaults != "" {
				paths = append(paths, p.HelmValues.Defaults)
			}
		}
		if p.Build != "" {
			paths = append(paths, p.Build)
		}
	}
	return paths
}

// patched applies patch to a temporary copy of the files check reads (the
// manifest's pipeline files, the owner decisions and proposal, and extra),
// so the checkout is never touched.
func patched(repo, patch string, m report.Manifest, extra ...string) (check.Files, func(), error) {
	dir, err := os.MkdirTemp("", "tvt-check-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	paths := append([]string{report.DecisionsPath, report.ProposalPath}, manifestFiles(m)...)
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	for _, p := range extra {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(p)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			cleanup()
			return nil, nil, err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	abs, err := filepath.Abs(patch)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	// A rename is applied only when both its paths are included: one that
	// moves a pipeline file out of the manifest's paths would otherwise be
	// skipped whole, and the file would look unchanged (S16). Including
	// the new path applies it, so the old path is gone and check refuses.
	moved, err := renamedTo(abs, paths)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	args := []string{"apply"}
	for _, p := range append(paths, moved...) {
		args = append(args, "--include="+p)
	}
	cmd := exec.Command("git", append(args, abs)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("git apply %s: %v: %s", patch, err, strings.TrimSpace(string(out)))
	}
	return dirFiles(dir), cleanup, nil
}

// renamedTo: the new paths of the patch's renames whose old path is one
// of paths, read from each file's "rename from" and "rename to" headers
// (git apply --numstat names only the new path of a pure rename). Paths
// git quotes (C-style, for unusual characters) are unquoted.
func renamedTo(patch string, paths []string) ([]string, error) {
	b, err := os.ReadFile(patch)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	unquote := func(s string) string {
		if strings.HasPrefix(s, `"`) {
			if u, err := strconv.Unquote(s); err == nil {
				return u
			}
		}
		return s
	}
	var moved []string
	from := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			from = ""
		case strings.HasPrefix(line, "rename from "):
			from = unquote(strings.TrimPrefix(line, "rename from "))
		case strings.HasPrefix(line, "rename to ") && from != "":
			to := unquote(strings.TrimPrefix(line, "rename to "))
			if want[from] && !want[to] {
				moved = append(moved, to)
			}
			from = ""
		}
	}
	return moved, nil
}
