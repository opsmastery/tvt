package main

import (
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
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/safe"
	"github.com/Opsmastery-Summit/tvt/pkg/check"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
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
		head, cleanup, err := patched(o.Repo, o.Patch, m)
		if err != nil {
			return check.Result{}, err
		}
		defer cleanup()
		in.Base, in.Head = tree, head
	} else {
		in.Base, in.Head = gitFiles(o.Repo, o.Base), tree
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

// patched copies the manifest's pipeline files to a temp dir and applies
// the patch there, so the checkout is never touched.
func patched(repo, patch string, m report.Manifest) (check.Files, func(), error) {
	dir, err := os.MkdirTemp("", "tvt-check-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	paths := []string{report.DecisionsPath, report.ProposalPath}
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
	args := []string{"apply"}
	for _, p := range paths {
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
