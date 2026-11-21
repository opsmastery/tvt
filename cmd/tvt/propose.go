package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/propose"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/validate"
	"github.com/Opsmastery-Summit/tvt/pkg/vcs"
	"github.com/prometheus/common/model"
)

type proposeOptions struct {
	DB        string
	Config    string
	Report    string
	Output    string
	Dir       string
	Checkouts checkoutFlag
	// Validate: auto (run each pipeline's validator when one is
	// available, note when not), require (and skip what can't be
	// validated) or off.
	Validate string
	Images   checkoutFlag // kind=image overrides of the validators' images
	Runtime  string
	// GitHub opens the draft PRs (--output pr); its token is GITHUB_TOKEN.
	GitHub vcs.GitHub
	// ReplayProm is the Prometheus API that protected queries are replayed
	// against before a label removal (S8 item 4); empty: no replay.
	ReplayProm string
	replay     replayer // tests
}

// prBranch is the branch tvt pushes in each pipeline repo. It's stable,
// so a rerun updates the same draft PR: tvt regenerates its own blocks
// (S7 item 10), and the branch is tvt's.
const prBranch = "tvt/propose"

// revertBranch holds the commit that undoes prBranch's (S10 item 6): its
// PR is based on prBranch, so GitHub retargets it to the base branch
// once the change merges and its branch is deleted.
const revertBranch = "tvt/propose-revert"

// checkoutFlag is --checkout repo=path, repeatable: where each pipeline
// repo is checked out. tvt reads the files there and writes patches; it
// never pushes, so it needs no Git token.
type checkoutFlag map[string]string

func (c checkoutFlag) String() string { return "" }

func (c checkoutFlag) Set(v string) error {
	repo, path, ok := strings.Cut(v, "=")
	if !ok || repo == "" || path == "" {
		return fmt.Errorf("want repo=path, got %q", v)
	}
	c[repo] = path
	return nil
}

func cmdPropose(args []string, stdout, stderr io.Writer) int {
	o := proposeOptions{Checkouts: checkoutFlag{}, Images: checkoutFlag{}}
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.DB, "db", "tvt.db", "path to the usage graph database (from tvt scan)")
	fs.StringVar(&o.Config, "config", "", "tvt.yaml: the pipelines the drops go into (required)")
	fs.StringVar(&o.Report, "report", "", "the reviewed report JSON (from tvt report); must be of the database's latest scan (default: build it from the database)")
	fs.StringVar(&o.Output, "output", "patch", "what to write: patch (one git-apply patch per pipeline repo), or pr (the patches too, then a draft pull request per repo from its --checkout; needs GITHUB_TOKEN)")
	fs.StringVar(&o.GitHub.BaseURL, "github-api", "", "GitHub API root for --output pr (default https://api.github.com; GitHub Enterprise: https://HOST/api/v3)")
	fs.StringVar(&o.Dir, "dir", "proposals", "where to write the patches")
	fs.StringVar(&o.Validate, "validate", "auto", "run each pipeline's own validator on the changed config: auto (when promtool, alloy, otelcol-contrib, fluent-bit or a container runtime is available), require (skip what can't be validated), off")
	fs.Var(o.Images, "validator-image", "kind=image: validate that pipeline kind with this image, your distribution and version (repeatable)")
	fs.StringVar(&o.Runtime, "container-runtime", "", "podman or docker for validators not on PATH (default: whichever answers; none: never)")
	fs.StringVar(&o.ReplayProm, "replay-prom", "", "Prometheus (or Mimir's Prometheus API) to replay protected queries against before a label removal: as written and without the label; a query that goes empty or changes its grouping blocks the removal (token in TVT_PROM_TOKEN)")
	fs.Var(o.Checkouts, "checkout", "repo=path: a pipeline repo's local checkout, as tvt.yaml names the repo (repeatable)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tvt propose: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if o.Config == "" {
		fmt.Fprintln(stderr, "tvt propose: --config is required (the pipelines come from tvt.yaml)")
		return exitUsage
	}
	switch o.Output {
	case "patch":
	case "pr":
		o.GitHub.Token = os.Getenv("GITHUB_TOKEN")
		if o.GitHub.Token == "" {
			fmt.Fprintln(stderr, "tvt propose: --output pr needs GITHUB_TOKEN in the environment (a token that can push branches and open pull requests in the pipeline repos)")
			return exitUsage
		}
	default:
		fmt.Fprintf(stderr, "tvt propose: --output %q: want patch or pr\n", o.Output)
		return exitUsage
	}
	if o.Validate != "auto" && o.Validate != "require" && o.Validate != "off" {
		fmt.Fprintf(stderr, "tvt propose: --validate %q: want auto, require or off\n", o.Validate)
		return exitUsage
	}
	if err := runPropose(context.Background(), o, stdout); err != nil {
		fmt.Fprintf(stderr, "tvt propose: %v\n", err)
		return exitError
	}
	return exitOK
}

// proposal is what tvt propose did with one recommendation.
type proposal struct {
	Rec  report.Recommendation
	Note string // why it was skipped; empty when written
	Repo string // the pipeline repo, when written
}

// runPropose turns the latest scan's proposable drop recommendations into
// one patch per pipeline repo (S7 item 3). Only whole-metric drops into
// Prometheus and Alloy pipelines are written so far; every other
// recommendation is listed as skipped, with the reason.
func runPropose(ctx context.Context, o proposeOptions, out io.Writer) error {
	if _, err := os.Stat(o.DB); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: no such database (run tvt scan first)", o.DB)
	}
	cfg, err := config.Load(o.Config)
	if err != nil {
		return err
	}
	in, _, err := readLatest(ctx, o.DB)
	if err != nil {
		return err
	}
	if in.Acknowledged, err = acknowledgements(o.Config); err != nil {
		return err
	}
	r := report.Build(in, 0)
	if o.Report != "" {
		b, err := os.ReadFile(o.Report)
		if err != nil {
			return err
		}
		var reviewed report.Report
		if err := json.Unmarshal(b, &reviewed); err != nil {
			return fmt.Errorf("%s: %w", o.Report, err)
		}
		if reviewed.Scan.ID != in.Scan.ID {
			return fmt.Errorf("%s is the report of scan %d, but the database's latest scan is %d: rerun tvt report and review it", o.Report, reviewed.Scan.ID, in.Scan.ID)
		}
		r = reviewed
	}
	jobs := map[string][]string{}
	for _, t := range in.PipelineTargets {
		jobs[t.Subject] = t.Jobs
	}
	byPipeline := map[string][]report.Recommendation{}
	var skipped []proposal
	var replayLines map[string][]string
	if o.ReplayProm != "" || o.replay != nil {
		rp := o.replay
		if rp == nil {
			rp = newReplayer(o.ReplayProm)
		}
		var blocked []proposal
		r.Recommendations, blocked, replayLines = replayLabelRemovals(ctx, in, r.Recommendations, rp)
		skipped = append(skipped, blocked...)
	}
	// ADR 0010: a signal something reads ad hoc gets a less lossy action
	// than a drop or a whole-stream reroute, and check refuses those
	// (least_lossy). tvt doesn't write what check would refuse; the less
	// lossy row (a label removal, an interval, a tier) goes ahead on its
	// own (S10 done-when, Nov 21).
	repoOf := map[string]string{}
	for _, p := range cfg.Pipelines {
		repoOf[p.Name] = p.Repo
	}
	read := map[string]report.ReadSignal{}
	for _, m := range report.Manifests(in, repoOf) {
		for _, rs := range m.Read {
			read[rs.ID] = rs
		}
	}
	r.Recommendations = slices.DeleteFunc(r.Recommendations, func(rec report.Recommendation) bool {
		rs, ok := read[rec.Signal.ID]
		if !ok || (rec.Action != report.ActionDrop && rec.Action != report.ActionReroute) {
			return false
		}
		skipped = append(skipped, proposal{Rec: rec, Note: fmt.Sprintf("read ad hoc by %d readers: ADR 0010 wants a less lossy action before a %s, and check refuses it (least_lossy)", rs.Readers, rec.Action)})
		return true
	})
	var queued []proposal
	r.Recommendations, queued = capQueue(r.Recommendations, cfg.Queue.PerOwner)
	skipped = append(skipped, queued...)
	for _, rec := range r.Recommendations {
		switch {
		case !rec.Proposable:
			skipped = append(skipped, proposal{Rec: rec, Note: "not proposable: " + cmp.Or(rec.PipelineNote, "no pipeline")})
		case !(rec.Action == report.ActionDrop && rec.Signal.Kind == string(graph.KindMetric)) && !(rec.Action == report.ActionReroute && rec.Signal.Kind == string(graph.KindStream)) && !(rec.Action == report.ActionLabelRemoval && rec.Signal.Kind == string(graph.KindLabel)) && !(rec.Action == report.ActionBucket && rec.Signal.Kind == string(graph.KindLabel)) && !(rec.Action == report.ActionLevelTier && rec.Signal.Kind == string(graph.KindStream)) && !(rec.Action == report.ActionCollapse && rec.Signal.Kind == string(graph.KindStream)) && !(rec.Action == report.ActionRetentionTier && rec.Signal.Kind == string(graph.KindMetric)):
			skipped = append(skipped, proposal{Rec: rec, Note: "no writer for " + rec.Action + " of a " + rec.Signal.Kind + " yet"})
		default:
			byPipeline[rec.Pipeline] = append(byPipeline[rec.Pipeline], rec)
		}
	}
	// Longer scrape intervals (S10 item 5), by pipeline.
	ivsByPipeline := map[string][]report.IntervalChange{}
	for _, iv := range r.Intervals {
		switch {
		case iv.Held != "" || iv.ProposedSec == 0:
		case iv.Pipeline == "":
			skipped = append(skipped, proposal{Rec: intervalRec(iv, r.Scan.Currency), Note: "the job's metrics come through no single pipeline"})
		default:
			ivsByPipeline[iv.Pipeline] = append(ivsByPipeline[iv.Pipeline], iv)
		}
	}
	// Record log events stay hot in a level tier (ADR 0011).
	records := append(append([]string{}, report.BuiltinRecordMatches.LogEvents...), cfg.Records.LogEvents...)
	v := &validator{o: o}
	defer v.cleanup()
	patches := map[string]*strings.Builder{} // by repo
	var written []proposal
	for _, p := range cfg.Pipelines {
		recs := byPipeline[p.Name]
		ivs := ivsByPipeline[p.Name]
		delete(ivsByPipeline, p.Name)
		if len(recs) == 0 && len(ivs) == 0 {
			continue
		}
		done, skip, diff := proposePipeline(p, recs, ivs, records, cfg.Tiers.DormantTenant, r.Scan.Currency, jobs, o.Checkouts, r.Scan.ID, v.check(ctx, p), func(n string) { v.build = append(v.build, p.Repo+" "+n) })
		for i := range done {
			done[i].Repo = p.Repo
		}
		written = append(written, done...)
		skipped = append(skipped, skip...)
		if diff != "" {
			if patches[p.Repo] == nil {
				patches[p.Repo] = &strings.Builder{}
			}
			patches[p.Repo].WriteString(diff)
		}
		delete(byPipeline, p.Name)
	}
	for name, recs := range byPipeline {
		for _, rec := range recs {
			skipped = append(skipped, proposal{Rec: rec, Note: "pipeline " + name + " isn't in " + o.Config})
		}
	}
	for name, ivs := range ivsByPipeline {
		for _, iv := range ivs {
			skipped = append(skipped, proposal{Rec: intervalRec(iv, r.Scan.Currency), Note: "pipeline " + name + " isn't in " + o.Config})
		}
	}
	// What each repo's patch proposes, for check's confirmations (S9).
	for repo, pb := range patches {
		d, err := proposalDiff(o.Checkouts[repo], r.Scan.ID, repo, written)
		if err != nil {
			return err
		}
		pb.WriteString(d)
	}
	// Replay results go with the validation notes, per repo. Without a
	// backend to replay against, a written label removal says so.
	replayed := o.ReplayProm != "" || o.replay != nil
	for _, p := range cfg.Pipelines {
		for _, l := range replayLines[p.Name] {
			v.notes = append(v.notes, p.Repo+" replay: "+l)
		}
		if replayed {
			continue
		}
		for _, w := range written {
			if w.Rec.Pipeline == p.Name && w.Rec.Action == report.ActionLabelRemoval {
				v.notes = append(v.notes, p.Repo+" replay: not run (no --replay-prom): protected queries weren't replayed without the label")
				break
			}
		}
	}
	if err := writeProposals(o, r.Scan.ID, patches, written, skipped, v.notes, v.build, out); err != nil {
		return err
	}
	if o.Output == "pr" {
		return openPRs(ctx, o, cfg.Pipelines, cfg.Ownership.GitHub, r.Scan.ID, patches, written, v.notes, v.build, out)
	}
	return nil
}

// openPRs pushes each repo's patch as tvt's branch and opens (or updates)
// a draft pull request for it (S7 item 3). PRs stay drafts: an owner's
// confirmation (S9) is what takes one out of draft. A repo that fails is
// reported and the others still go ahead; the error at the end says how
// many failed.
func openPRs(ctx context.Context, o proposeOptions, pipes []config.Pipeline, githubOf map[string]string, scan int64, patches map[string]*strings.Builder, written []proposal, validation, build []string, out io.Writer) error {
	repos := make([]string, 0, len(patches))
	for repo := range patches {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	failed := 0
	for _, repo := range repos {
		var base string
		var reviewers []string
		for _, p := range pipes {
			if p.Repo != repo {
				continue
			}
			base = cmp.Or(base, p.Branch)
			for _, r := range p.Reviewers {
				if !slices.Contains(reviewers, r) {
					reviewers = append(reviewers, r)
				}
			}
		}
		// Each recommendation's owner reviews it (S9 item 2).
		for _, w := range written {
			if w.Repo != repo {
				continue
			}
			if h := ownerHandle(w.Rec.Owner, githubOf); h != "" && !slices.Contains(reviewers, h) {
				reviewers = append(reviewers, h)
			}
		}
		err := func() error {
			owner, name, err := vcs.ParseRepo(repo)
			if err != nil {
				return err
			}
			checkout, ok := o.Checkouts[repo]
			if !ok {
				return fmt.Errorf("no --checkout for %s", repo)
			}
			used, err := vcs.Push(ctx, vcs.Branch{Checkout: checkout, Base: base, Name: prBranch, Revert: revertBranch, Patch: patches[repo].String(),
				Message: fmt.Sprintf("tvt propose: changes from scan %d", scan)})
			if err != nil {
				return err
			}
			res, err := o.GitHub.OpenOrUpdate(ctx, vcs.PR{Owner: owner, Repo: name, Head: prBranch, Base: used, Reviewers: reviewers,
				Title: fmt.Sprintf("tvt: telemetry changes from scan %d", scan),
				Body:  prBody(repo, scan, written, validation, build)})
			if err != nil {
				return err
			}
			verb := "updated"
			if res.Created {
				verb = "opened"
			}
			fmt.Fprintf(out, "draft PR %s for %s: %s\n", verb, repo, res.URL)
			if res.Note != "" {
				fmt.Fprintf(out, "  %s\n", res.Note)
			}
			// Every change comes with its revert (S10 item 6, ADR 0010).
			rev, err := o.GitHub.OpenOrUpdate(ctx, vcs.PR{Owner: owner, Repo: name, Head: revertBranch, Base: prBranch, Reviewers: reviewers,
				Title: fmt.Sprintf("tvt: revert the telemetry changes from scan %d", scan),
				Body:  revertBody(scan, res.URL, used)})
			if err != nil {
				return fmt.Errorf("the change's PR is up, but its revert PR isn't: %w", err)
			}
			verb = "updated"
			if rev.Created {
				verb = "opened"
			}
			fmt.Fprintf(out, "revert PR %s for %s: %s\n", verb, repo, rev.URL)
			return nil
		}()
		if err != nil {
			failed++
			fmt.Fprintf(out, "no PR for %s: %v\n", repo, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d pull requests failed (the patches are in %s)", failed, len(repos), o.Dir)
	}
	return nil
}

// capQueue keeps at most perOwner proposable open recommendations per
// owner (S9 item 6; 0: no cap). recs are highest savings per minute of
// checking first, so each owner gets their best ones; the rest wait.
func capQueue(recs []report.Recommendation, perOwner int) (kept []report.Recommendation, waiting []proposal) {
	if perOwner <= 0 {
		return recs, nil
	}
	n := map[string]int{}
	for _, rec := range recs {
		if rec.Proposable && rec.Status == report.StatusOpen {
			owner := cmp.Or(rec.Owner, report.Unowned)
			if n[owner] >= perOwner {
				waiting = append(waiting, proposal{Rec: rec, Note: fmt.Sprintf("queued: %s already has %d recommendations in front of them (queue.per_owner); it comes up as those are decided", owner, perOwner)})
				continue
			}
			n[owner]++
		}
		kept = append(kept, rec)
	}
	return kept, waiting
}

// ownerHandle is the GitHub user or team that reviews owner's changes:
// its tvt.yaml ownership.github entry, or the owner itself when it's
// already a handle.
func ownerHandle(owner string, githubOf map[string]string) string {
	if h := githubOf[owner]; h != "" {
		return h
	}
	if strings.HasPrefix(owner, "@") {
		return owner
	}
	return ""
}

// proposalDiff adds or updates report.ProposalPath in a repo: what this
// proposal carries per pipeline, with each recommendation's evidence
// hash, so tvt check can ask for a matching confirmation of each.
func proposalDiff(checkout string, scan int64, repo string, written []proposal) (string, error) {
	p := report.Proposal{Scan: scan, Pipelines: map[string][]report.ProposedChange{}}
	for _, w := range written {
		if w.Repo != repo || w.Rec.Pipeline == "" || strings.HasPrefix(w.Rec.ID, propose.IntervalPrefix) {
			// A scrape interval change drops nothing, so it needs no owner
			// confirmation: check guards it with the scrape limits.
			continue
		}
		p.Pipelines[w.Rec.Pipeline] = append(p.Pipelines[w.Rec.Pipeline], report.ProposedChange{Recommendation: w.Rec.ID, Signal: w.Rec.Signal.ID,
			Evidence: w.Rec.EvidenceHash, Owner: w.Rec.Owner, OwnerInferred: w.Rec.OwnerInferred, Expected: w.Rec.Expected})
	}
	for k := range p.Pipelines {
		sort.Slice(p.Pipelines[k], func(i, j int) bool { return p.Pipelines[k][i].Recommendation < p.Pipelines[k][j].Recommendation })
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	var old []byte
	if checkout != "" {
		old, err = os.ReadFile(filepath.Join(checkout, filepath.FromSlash(report.ProposalPath)))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if old == nil {
		return newFileDiff(report.ProposalPath, b), nil
	}
	return propose.Diff(report.ProposalPath, old, b), nil
}

// newFileDiff is a git patch that creates path with content.
func newFileDiff(path string, content []byte) string {
	lines := strings.SplitAfter(string(content), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, path, path, len(lines))
	for _, l := range lines {
		b.WriteString("+" + l)
	}
	return b.String()
}

// revertBody describes the revert PR: what it undoes and when to merge it.
func revertBody(scan int64, change, base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Undoes tvt's telemetry changes from scan %d (%s). ", scan, change)
	fmt.Fprintf(&b, "It's based on tvt's branch, so until that PR merges it shows exactly the commit that reverses it. Once it merges and its branch is deleted, GitHub retargets this PR to `%s`.\n\n", base)
	b.WriteString("Merge this if something that needed the dropped data turns up: it puts back what tvt removed, and adds nothing. `tvt check` needs no owner confirmation for it, since it drops nothing.\n\n")
	b.WriteString("tvt regenerates both branches on each `tvt propose`, so don't edit them by hand.\n")
	return b.String()
}

// prBody is a draft PR's description: what changes and why, validation,
// build notes, and that it stays a draft until its owner confirms.
func prBody(repo string, scan int64, written []proposal, validation, build []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Telemetry changes from tvt scan %d. Each rule sits in a marked `tvt:begin` block with its evidence; tvt regenerates its own blocks on the next run, so don't edit inside them.\n\n", scan)
	// GitHub refuses a body over 65,536 characters (a 422), and a first
	// scan can carry a thousand drops: list rows while they fit, then say
	// how many more the patch carries.
	const budget = 50000
	n, shown := 0, 0
	var rows strings.Builder
	for _, w := range written {
		if w.Repo != repo {
			continue
		}
		n++
		who := cmp.Or(w.Rec.Owner, "none found")
		if w.Rec.OwnerInferred {
			who += " (inferred)"
		}
		after := strings.Join(w.Rec.Expected.Statements, "; ")
		row := fmt.Sprintf("| `%s` | %s | `%s` | %s | %s |\n", w.Rec.ID, strings.ReplaceAll(who, "|", "\\|"), w.Rec.EvidenceHash, strings.ReplaceAll(w.Rec.Claim, "|", "\\|"), strings.ReplaceAll(after, "|", "\\|"))
		if rows.Len()+len(row) > budget {
			continue
		}
		rows.WriteString(row)
		shown++
	}
	fmt.Fprintf(&b, "%d recommendations.\n\n| Recommendation | Owner | Evidence | Why | After the merge |\n|---|---|---|---|---|\n%s", n, rows.String())
	if shown < n {
		fmt.Fprintf(&b, "\n%d more aren't listed here (GitHub caps a PR description); each rule's block in the diff names its recommendation and evidence.\n", n-shown)
	}
	b.WriteString("\n**Validation**\n\n")
	for _, n := range validation {
		if rest, ok := strings.CutPrefix(n, repo+" "); ok {
			fmt.Fprintf(&b, "- %s\n", rest)
		}
	}
	var builds []string
	for _, n := range build {
		if rest, ok := strings.CutPrefix(n, repo+" "); ok {
			builds = append(builds, rest)
		}
	}
	if len(builds) > 0 {
		b.WriteString("\n**Collector build**\n\n")
		for _, n := range builds {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	fmt.Fprintf(&b, "\n**To confirm** (S9)\n\nThis PR stays a draft, and `tvt check` refuses it, until each recommendation's owner confirms the evidence above. Check each signal with `tvt why <signal>`, then, on this branch:\n\n```sh\ntvt confirm --report <report.json> --dir . --owner <you> <recommendation>...\ngit add %s && git commit -m 'tvt: confirm' && git push\n```\n\nBefore confirming, answer one more question: is the signal read only at certain times of year (a year-end report, a holiday peak)? If so, record `--seasonal MONTHS` instead (a keep until after the next season), or `--dismiss REASON`. A confirmation records the evidence hash. If a new reader or a reopened gap changes it before the merge, `tvt check` asks the owner to look again.\n", report.DecisionsPath)
	return b.String()
}

// proposePipeline writes recs (whole-metric drops) into pipeline p's
// files from its checkout and returns the diff. A recommendation is
// written only when every job its metric carries has a scrape config in
// p's files: a drop in some jobs would leave the rest of the series, and
// the savings, in place.
func proposePipeline(p config.Pipeline, recs []report.Recommendation, ivs []report.IntervalChange, records []string, tenant, currency string, jobs map[string][]string, checkouts checkoutFlag, scan int64, check checkFunc, note func(string)) (written, skipped []proposal, diff string) {
	var tiers, collapses, tenantRecs []report.Recommendation
	skipAll := func(note string) ([]proposal, []proposal, string) {
		for _, rec := range append(append(append(slices.Clone(recs), tiers...), collapses...), tenantRecs...) {
			skipped = append(skipped, proposal{Rec: rec, Note: note})
		}
		for _, iv := range ivs {
			skipped = append(skipped, proposal{Rec: intervalRec(iv, currency), Note: note})
		}
		return nil, skipped, ""
	}
	// Level tiers (S10 item 3): an OTel Collector's go to its own writer;
	// Fluent Bit and Alloy write them as reroutes of the lines below warn.
	recs = slices.DeleteFunc(slices.Clone(recs), func(rec report.Recommendation) bool {
		if rec.Action == report.ActionRetentionTier {
			// Retention tiers (S10 item 2): a route to the tier tenant in
			// a Prometheus pipeline's remote_write.
			if p.Kind != "prometheus" {
				skipped = append(skipped, proposal{Rec: rec, Note: "the Mimir tenant tier is written in a Prometheus pipeline's remote_write; none for " + p.Kind + " pipelines yet"})
			} else {
				tenantRecs = append(tenantRecs, rec)
			}
			return true
		}
		if rec.Action == report.ActionCollapse {
			// Collapses (S10 item 4): log_dedup, an OTel Collector processor.
			if p.Kind != "otelcol" {
				skipped = append(skipped, proposal{Rec: rec, Note: "a collapse is the OTel Collector's log_dedup; no collapse writer for " + p.Kind + " pipelines"})
			} else {
				collapses = append(collapses, rec)
			}
			return true
		}
		if rec.Action != report.ActionLevelTier || p.Kind == "fluentbit" || p.Kind == "alloy" {
			return false
		}
		switch {
		case p.Kind != "otelcol":
			skipped = append(skipped, proposal{Rec: rec, Note: "no level-tier writer for " + p.Kind + " pipelines yet"})
		case p.Cold == "":
			skipped = append(skipped, proposal{Rec: rec, Note: "pipeline " + p.Name + " names no cold exporter (tvt.yaml pipelines[].cold, such as otlp_http/cold)"})
		default:
			tiers = append(tiers, rec)
		}
		return true
	})
	if len(recs) == 0 && len(ivs) == 0 && len(tiers) == 0 && len(collapses) == 0 && len(tenantRecs) == 0 {
		return nil, skipped, ""
	}
	if len(ivs) > 0 && p.Kind != "prometheus" && p.Kind != "alloy" {
		for _, iv := range ivs {
			skipped = append(skipped, proposal{Rec: intervalRec(iv, currency), Note: "no scrape interval writer for " + p.Kind + " pipelines"})
		}
		ivs = nil
	}
	if p.Kind == "prometheus-operator" {
		root, ok := checkouts[p.Repo]
		switch {
		case !ok:
			return skipAll("no --checkout for " + p.Repo)
		case len(p.Files) == 0:
			return skipAll("pipeline " + p.Name + " lists no monitor manifests (files; with helm_values, the chart's rendered output)")
		}
		var drops []report.Recommendation
		for _, rec := range recs {
			if rec.Action == report.ActionReroute {
				skipped = append(skipped, proposal{Rec: rec, Note: "a prometheus-operator pipeline carries metrics; no writer for reroute there"})
				continue
			}
			if rec.Action == report.ActionLabelRemoval {
				skipped = append(skipped, proposal{Rec: rec, Note: "no writer for label removals in prometheus-operator pipelines yet"})
				continue
			}
			drops = append(drops, rec)
		}
		w, s, diff := proposeMonitors(p, drops, jobs, root, scan, check)
		return w, append(skipped, s...), diff
	}
	if p.Kind == "fluentbit" {
		root, ok := checkouts[p.Repo]
		switch {
		case !ok:
			return skipAll("no --checkout for " + p.Repo)
		case p.HelmValues != nil:
			return proposeFluentBitHelm(p, recs, root, scan, check)
		case len(p.Files) == 0:
			return skipAll("pipeline " + p.Name + " lists no files or helm_values")
		}
		return proposeFluentBit(p, recs, root, scan, check)
	}
	// Stream reroutes: Fluent Bit above; Alloy below, with the drops.
	var drops, reroutes []report.Recommendation
	for _, rec := range recs {
		if rec.Action == report.ActionReroute || rec.Action == report.ActionLevelTier {
			switch {
			case p.Kind != "alloy":
				skipped = append(skipped, proposal{Rec: rec, Note: "no writer for stream reroutes in " + p.Kind + " pipelines yet"})
			case p.Cold == "":
				skipped = append(skipped, proposal{Rec: rec, Note: "pipeline " + p.Name + " names no cold component (tvt.yaml pipelines[].cold, such as loki.write.cold)"})
			default:
				reroutes = append(reroutes, rec)
			}
			continue
		}
		if rec.Action == report.ActionLabelRemoval && (rec.Signal.Name == "job" || rec.Signal.Name == "instance" || strings.HasPrefix(rec.Signal.Name, "__")) {
			// One refused label must not take the file's other rules with it.
			skipped = append(skipped, proposal{Rec: rec, Note: "tvt never removes " + rec.Signal.Name + ": it identifies the scrape target, and rules, alerts and joins rely on it"})
			continue
		}
		if rec.Action == report.ActionLabelRemoval && p.Kind == "otelcol" && strings.HasPrefix(rec.Signal.Name, "otel_scope_") {
			// The Prometheus exporters add otel_scope_name and friends from
			// the instrumentation scope; a datapoint attribute delete
			// changes nothing (test plan M7, Nov 18).
			skipped = append(skipped, proposal{Rec: rec, Note: rec.Signal.Name + " isn't a datapoint attribute: the Collector's Prometheus exporter adds it from the instrumentation scope, so a transform can't remove it"})
			continue
		}
		if rec.Action == report.ActionBucket && p.Kind != "otelcol" {
			skipped = append(skipped, proposal{Rec: rec, Note: fmt.Sprintf("bucketing %s merges about %d series, which a relabel rule can't sum; tvt writes it in an OTel Collector's ordered pipeline (stream aggregation comes later)", rec.Signal.Name, rec.Merges)})
			continue
		}
		if rec.Action == report.ActionLabelRemoval && p.Kind != "otelcol" {
			switch {
			case p.Kind != "prometheus" && p.Kind != "alloy":
				skipped = append(skipped, proposal{Rec: rec, Note: "no writer for label removals in " + p.Kind + " pipelines yet"})
				continue
			case rec.Removal != "labeldrop":
				skipped = append(skipped, proposal{Rec: rec, Note: fmt.Sprintf("removing %s merges %d series, which a relabel rule can't sum; that needs a recording rule or a collector's ordered pipeline", rec.Signal.Name, rec.Merges)})
				continue
			}
		}
		drops = append(drops, rec)
	}
	if recs = drops; len(recs) == 0 && len(reroutes) == 0 && len(ivs) == 0 && len(tiers) == 0 && len(collapses) == 0 && len(tenantRecs) == 0 {
		return nil, skipped, ""
	}
	var routes []propose.Route
	for _, rec := range reroutes {
		labels, err := selectorLabels(rec.Signal.Name)
		if err != nil {
			skipped = append(skipped, proposal{Rec: rec, Note: err.Error()})
			continue
		}
		d := dropOf(rec.ID, []report.Recommendation{rec}, scan)
		routes = append(routes, propose.Route{ID: rec.ID, Scan: scan, Evidence: d.Evidence, Reason: d.Reason, Stream: labels, Cold: p.Cold, BelowWarn: rec.Action == report.ActionLevelTier})
	}
	if len(recs) == 0 && len(ivs) == 0 && len(tiers) == 0 && len(collapses) == 0 && len(tenantRecs) == 0 {
		return proposeAlloyRoutesOnly(p, reroutes, routes, checkouts, scan, check, skipped)
	}
	var listJobs func([]byte) ([]string, error)
	switch p.Kind {
	case "prometheus":
		listJobs = propose.PrometheusJobs
	case "alloy":
		listJobs = propose.AlloyJobs
	case "otelcol":
		if p.HelmValues != nil || len(p.Files) == 0 {
			return skipAll("pipeline " + p.Name + " lists no files (Helm values aren't written yet)")
		}
		root, ok := checkouts[p.Repo]
		if !ok {
			return skipAll("no --checkout for " + p.Repo)
		}
		// Keep what the loop above skipped (job, instance, scope labels).
		w, s, diff := proposeOtelcol(p, recs, tiers, collapses, records, root, scan, check, note)
		return w, append(skipped, s...), diff
	default:
		return skipAll("no writer for " + p.Kind + " pipelines yet")
	}
	if p.HelmValues != nil || len(p.Files) == 0 {
		return skipAll("pipeline " + p.Name + " lists no files (Helm values aren't written yet)")
	}
	root, ok := checkouts[p.Repo]
	if !ok {
		return skipAll("no --checkout for " + p.Repo)
	}
	srcs := map[string][]byte{}
	fileOf := map[string]string{} // job -> the first file with its scrape config
	for _, f := range p.Files {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			return skipAll(err.Error())
		}
		js, err := listJobs(b)
		if err != nil {
			return skipAll(f + ": " + err.Error())
		}
		srcs[f] = b
		for _, j := range js {
			if _, ok := fileOf[j]; !ok {
				fileOf[j] = f
			}
		}
	}
	// file -> job -> recommendations whose metric that job carries.
	plan := map[string]map[string][]report.Recommendation{}
	for _, rec := range recs {
		js := jobs[rec.Signal.ID]
		if len(js) == 0 {
			skipped = append(skipped, proposal{Rec: rec, Note: fmt.Sprintf("scan %d didn't read the metric's job label", scan)})
			continue
		}
		var missing []string
		for _, j := range js {
			if _, ok := fileOf[j]; !ok {
				missing = append(missing, j)
			}
		}
		if len(missing) > 0 {
			skipped = append(skipped, proposal{Rec: rec, Note: fmt.Sprintf("job %s has no scrape config in %s", strings.Join(missing, ", "), strings.Join(p.Files, ", "))})
			continue
		}
		for _, j := range js {
			f := fileOf[j]
			if plan[f] == nil {
				plan[f] = map[string][]report.Recommendation{}
			}
			plan[f][j] = append(plan[f][j], rec)
		}
	}
	// file -> job -> interval, for jobs with a scrape config in p's files.
	ivPlan := map[string]map[string]propose.Interval{}
	ivRecs := map[string]report.Recommendation{} // by job
	for _, iv := range ivs {
		rec := intervalRec(iv, currency)
		f, ok := fileOf[iv.Job]
		if !ok {
			skipped = append(skipped, proposal{Rec: rec, Note: fmt.Sprintf("job %s has no scrape config in %s", iv.Job, strings.Join(p.Files, ", "))})
			continue
		}
		if ivPlan[f] == nil {
			ivPlan[f] = map[string]propose.Interval{}
		}
		ivPlan[f][iv.Job] = propose.Interval{ID: rec.ID, Scan: scan, Reason: rec.Claim, Every: time.Duration(iv.ProposedSec * float64(time.Second))}
		ivRecs[iv.Job] = rec
	}
	// The retention tier: one route to the tier tenant per pipeline.
	var tenantTier *propose.TenantTier
	if len(tenantRecs) > 0 {
		t := propose.TenantTier{ID: propose.TenantPrefix + p.Name, Scan: scan, Tenant: tenant}
		for _, rec := range tenantRecs {
			t.Metrics = append(t.Metrics, rec.Signal.Name)
			t.KeepDays = max(t.KeepDays, rec.KeepDays)
		}
		d := dropOf(t.ID, tenantRecs, scan)
		t.Evidence, t.Reason = d.Evidence, plural(len(tenantRecs), "dormant metric keeps", "dormant metrics keep")+fmt.Sprintf(" %d days in tenant %s, see tvt why", t.KeepDays, tenant)
		tenantTier = &t
	}
	tenantDone, tenantFailure := false, ""
	var b strings.Builder
	routed := false
	failed := map[string]string{} // rec ID -> why its file couldn't be written
	var ivWritten []proposal
	for _, f := range p.Files {
		byJob := plan[f]
		hasRW := tenantTier != nil && !tenantDone && p.Kind == "prometheus" && strings.Contains(string(srcs[f]), "remote_write:")
		if len(byJob) == 0 && len(ivPlan[f]) == 0 && !hasRW {
			continue
		}
		out := srcs[f]
		var err error
		switch {
		case len(byJob) == 0:
			// Intervals or the tier only; an Alloy file still takes the
			// reroutes.
			if p.Kind == "alloy" && len(routes) > 0 && !routed {
				if next, rerr := propose.AlloyRoutesKeep(out, routes); rerr == nil {
					out, routed = next, true
				}
			}
		case p.Kind == "prometheus":
			drops := map[string]propose.Drop{}
			for j, rs := range byJob {
				drops[j] = dropOf(p.Name+"/"+j, rs, scan)
			}
			out, err = propose.PrometheusDrops(srcs[f], drops)
		default:
			// One block per relabel component: every job's metrics, since
			// jobs can share a component and a drop is of the whole metric.
			var all []report.Recommendation
			var js []string
			for j, rs := range byJob {
				js = append(js, j)
				all = append(all, rs...)
			}
			slices.Sort(js)
			out, err = propose.Alloy(srcs[f], js, dropOf(p.Name, all, scan))
			// The stream reroutes go in the same pass, into the file with
			// the loki.process shipping to Loki.
			if err == nil && len(routes) > 0 && !routed {
				if next, rerr := propose.AlloyRoutesKeep(out, routes); rerr == nil {
					out, routed = next, true
				}
			}
		}
		// After the drops: the drop writers strip every tvt block, the
		// interval writers only their own.
		if err == nil && len(ivPlan[f]) > 0 {
			if p.Kind == "prometheus" {
				out, err = propose.PrometheusIntervals(out, ivPlan[f])
			} else {
				out, err = propose.AlloyIntervals(out, ivPlan[f])
			}
		}
		if err == nil && hasRW {
			var tout []byte
			if tout, err = propose.PrometheusTenantTier(out, tenantTier); err == nil {
				out, tenantDone = tout, true
			} else if len(byJob) == 0 && len(ivPlan[f]) == 0 {
				tenantFailure = f + ": " + err.Error()
				continue
			} else {
				tenantFailure, err = f+": "+err.Error(), nil
			}
		}
		if err == nil {
			err = check(root, f, out)
		}
		if err != nil {
			if hasRW && tenantDone {
				tenantDone, tenantFailure = false, f+": "+err.Error()
			}
			for _, rs := range byJob {
				for _, rec := range rs {
					failed[rec.ID] = f + ": " + err.Error()
				}
			}
			for j := range ivPlan[f] {
				skipped = append(skipped, proposal{Rec: ivRecs[j], Note: f + ": " + err.Error()})
			}
			continue
		}
		for j := range ivPlan[f] {
			ivWritten = append(ivWritten, proposal{Rec: ivRecs[j]})
		}
		b.WriteString(propose.Diff(f, srcs[f], out))
	}
	sort.Slice(ivWritten, func(i, j int) bool { return ivWritten[i].Rec.ID < ivWritten[j].Rec.ID })
	written = append(written, ivWritten...)
	for _, rec := range recs {
		if note, ok := failed[rec.ID]; ok {
			skipped = append(skipped, proposal{Rec: rec, Note: note})
		} else if slices.ContainsFunc(skipped, func(s proposal) bool { return s.Rec.ID == rec.ID }) {
			continue
		} else {
			written = append(written, proposal{Rec: rec})
		}
	}
	for _, rec := range tenantRecs {
		switch {
		case tenantDone:
			written = append(written, proposal{Rec: rec})
		case tenantFailure != "":
			skipped = append(skipped, proposal{Rec: rec, Note: tenantFailure})
		default:
			skipped = append(skipped, proposal{Rec: rec, Note: "no file of the pipeline has a remote_write to send the tier from"})
		}
	}
	for _, rec := range reroutes {
		switch {
		case slices.ContainsFunc(skipped, func(s proposal) bool { return s.Rec.ID == rec.ID }):
		case routed:
			written = append(written, proposal{Rec: rec})
		default:
			skipped = append(skipped, proposal{Rec: rec, Note: "no file with the pipeline's drops has the loki.process that ships to Loki"})
		}
	}
	return written, skipped, b.String()
}

// dropOf is the marked block for recs: their metrics, the scan, and a
// short hash of the evidence they rest on (S9 binds approvals to it).
func dropOf(id string, recs []report.Recommendation, scan int64) propose.Drop {
	recs = slices.Clone(recs)
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	recs = slices.CompactFunc(recs, func(a, b report.Recommendation) bool { return a.ID == b.ID })
	h := sha256.New()
	fmt.Fprintf(h, "scan %d\n", scan)
	d := propose.Drop{ID: id, Scan: scan}
	streams, tiers, collapses := 0, 0, 0
	var monthly float64
	currency := ""
	for _, rec := range recs {
		ev, _ := json.Marshal(rec.Evidence)
		fmt.Fprintf(h, "%s\n%s\n", rec.ID, ev)
		if rec.Action == report.ActionLabelRemoval {
			i := slices.IndexFunc(d.Labels, func(l propose.LabelRule) bool { return l.Label == rec.Signal.Name })
			if i < 0 {
				d.Labels = append(d.Labels, propose.LabelRule{Label: rec.Signal.Name})
				i = len(d.Labels) - 1
			}
			d.Labels[i].Metrics = append(d.Labels[i].Metrics, rec.Signal.Parent)
			monthly += rec.Savings.Monthly
			currency = cmp.Or(currency, rec.Savings.Currency)
			continue
		}
		if rec.Action == report.ActionCollapse {
			collapses++ // repeated lines: nothing for the metric list
		} else if rec.Action == report.ActionLevelTier {
			tiers++ // a stream's lower levels: nothing for the metric list
		} else if rec.Action == report.ActionReroute {
			streams++ // a stream's route: nothing for the Drop's metric list
		} else {
			d.Metrics = append(d.Metrics, rec.Signal.Name)
		}
		monthly += rec.Savings.Monthly
		currency = cmp.Or(currency, rec.Savings.Currency)
	}
	d.Evidence = hex.EncodeToString(h.Sum(nil))[:12]
	var parts []string
	if len(d.Metrics) > 0 {
		parts = append(parts, plural(len(d.Metrics), "unread metric", "unread metrics"))
	}
	if len(d.Labels) > 0 {
		parts = append(parts, plural(len(d.Labels), "unread label", "unread labels"))
	}
	if streams > 0 {
		parts = append(parts, plural(streams, "unread stream", "unread streams"))
	}
	if collapses > 0 {
		parts = append(parts, plural(collapses, "unread stream of repeated lines", "unread streams of repeated lines"))
	}
	if tiers > 0 {
		parts = append(parts, plural(tiers, "stream whose lines below warn nothing reads", "streams whose lines below warn nothing reads"))
	}
	d.Reason = strings.Join(parts, ", ") + ", see tvt why"
	if currency != "" {
		d.Reason += fmt.Sprintf("; saves %.2f %s/month", monthly, currency)
	}
	return d
}

// writeProposals writes each repo's patch to o.Dir/<repo>.patch, headed by
// the recommendations it carries (git apply skips text before the first
// diff), and prints what was written and what was skipped, and why.
func writeProposals(o proposeOptions, scan int64, patches map[string]*strings.Builder, written, skipped []proposal, validation, build []string, out io.Writer) error {
	repos := make([]string, 0, len(patches))
	for repo := range patches {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	if len(repos) > 0 {
		if err := os.MkdirAll(o.Dir, 0o755); err != nil {
			return err
		}
	}
	for _, repo := range repos {
		var b strings.Builder
		fmt.Fprintf(&b, "tvt propose: drops from scan %d for %s\n\n", scan, repo)
		for _, w := range written {
			if w.Repo != repo {
				continue
			}
			fmt.Fprintf(&b, "  %s\n    %s\n", w.Rec.ID, w.Rec.Claim)
		}
		b.WriteString("\nValidation:\n")
		for _, n := range validation {
			if strings.HasPrefix(n, repo+" ") {
				fmt.Fprintf(&b, "  %s\n", strings.TrimPrefix(n, repo+" "))
			}
		}
		var builds []string
		for _, n := range build {
			if rest, ok := strings.CutPrefix(n, repo+" "); ok {
				builds = append(builds, rest)
			}
		}
		if len(builds) > 0 {
			b.WriteString("\nCollector build:\n")
			for _, n := range builds {
				fmt.Fprintf(&b, "  %s\n", n)
			}
		}
		b.WriteString("\nCheck each with tvt why <signal> before merging. Apply with: git apply <this file>\n\n")
		b.WriteString(patches[repo].String())
		path := filepath.Join(o.Dir, report.ManifestDir(repo)+".patch")
		if err := writeFileAtomic(path, []byte(b.String())); err != nil {
			return err
		}
		fmt.Fprintf(out, "patch for %s written to %s\n", repo, path)
	}
	for _, n := range validation {
		fmt.Fprintf(out, "validation: %s\n", n)
	}
	for _, n := range build {
		fmt.Fprintf(out, "build: %s\n", n)
	}
	fmt.Fprintf(out, "scan %d: %d drops written, %d recommendations skipped\n", scan, len(written), len(skipped))
	sort.SliceStable(skipped, func(i, j int) bool { return skipped[i].Note < skipped[j].Note })
	for _, s := range skipped {
		fmt.Fprintf(out, "  skipped %s: %s\n", s.Rec.ID, s.Note)
	}
	return nil
}

// proposeOtelcol writes recs into an OpenTelemetry Collector pipeline: one
// filter/tvt processor per file, first in each of the file's metrics
// pipelines, matching every OTel name that Prometheus stores under a
// recommended name (propose.OtelConditions, S7 item 9). A name that may
// be one series of an OTel histogram is skipped: the collector sees the
// whole histogram.
func proposeOtelcol(p config.Pipeline, recs, tierRecs, collapseRecs []report.Recommendation, records []string, root string, scan int64, check checkFunc, note func(string)) (written, skipped []proposal, diff string) {
	form, err := propose.OTTLFormFor(p.Version)
	if err != nil {
		for _, rec := range append(append(slices.Clone(recs), tierRecs...), collapseRecs...) {
			skipped = append(skipped, proposal{Rec: rec, Note: err.Error()})
		}
		return nil, skipped, ""
	}
	var ok []report.Recommendation
	var labels []report.Recommendation
	for _, rec := range recs {
		if rec.Action == report.ActionLabelRemoval || rec.Action == report.ActionBucket {
			if p.Replicas > 1 && !p.LoadBalanced {
				skipped = append(skipped, proposal{Rec: rec, Note: fmt.Sprintf("%d Collector replicas with no load-balancing exporter in front: summing over a label needs one Collector to see every session (tvt.yaml pipelines[].load_balanced)", p.Replicas)})
				continue
			}
			if _, matchable := propose.OtelConditions(rec.Signal.Parent); !matchable {
				skipped = append(skipped, proposal{Rec: rec, Note: "its metric may be one series of an OTel histogram"})
				continue
			}
			labels = append(labels, rec)
			continue
		}
		if _, matchable := propose.OtelConditions(rec.Signal.Name); !matchable {
			skipped = append(skipped, proposal{Rec: rec, Note: "may be one series of an OTel histogram, which a collector can't drop alone"})
			continue
		}
		ok = append(ok, rec)
	}
	// The level tier: one per pipeline, by service.name.
	var tier *propose.Tier
	var tiered []report.Recommendation
	for _, rec := range tierRecs {
		l, err := selectorLabels(rec.Signal.Name)
		if err != nil || len(l) != 1 || l["service_name"] == "" {
			skipped = append(skipped, proposal{Rec: rec, Note: "a Collector level tier picks lines by service.name: tvt writes one only for a stream whose one label is service_name"})
			continue
		}
		if tier == nil {
			tier = &propose.Tier{ID: propose.TierPrefix + p.Name, Scan: scan, Records: records, Cold: p.Cold, Collector: p.Version}
		}
		tier.Services = append(tier.Services, l["service_name"])
		tiered = append(tiered, rec)
	}
	if tier != nil {
		d := dropOf(tier.ID, tiered, scan)
		tier.Evidence, tier.Reason = d.Evidence, d.Reason
	}
	// The collapse: one log_dedup per pipeline, by service.name.
	var dedup *propose.Dedup
	var collapsed []report.Recommendation
	for _, rec := range collapseRecs {
		l, err := selectorLabels(rec.Signal.Name)
		if err != nil || len(l) != 1 || l["service_name"] == "" {
			skipped = append(skipped, proposal{Rec: rec, Note: "a Collector collapse picks lines by service.name: tvt writes one only for a stream whose one label is service_name"})
			continue
		}
		if dedup == nil {
			dedup = &propose.Dedup{ID: propose.DedupPrefix + p.Name, Scan: scan, Records: records, Collector: p.Version}
		}
		dedup.Services = append(dedup.Services, l["service_name"])
		collapsed = append(collapsed, rec)
	}
	if dedup != nil {
		d := dropOf(dedup.ID, collapsed, scan)
		dedup.Evidence, dedup.Reason = d.Evidence, d.Reason
	}
	if len(ok) == 0 && len(labels) == 0 && tier == nil && dedup == nil {
		return nil, skipped, ""
	}
	// Label removals: the ordered pipeline for delta counters (S7 item 8).
	var agg *propose.Aggregate
	if len(labels) > 0 {
		a := propose.Aggregate{ID: p.Name + "/labels", Scan: scan, Collector: p.Version, Labels: map[string][]string{}}
		d := dropOf(a.ID, labels, scan)
		a.Evidence = d.Evidence
		var names, bucketed []string
		for _, rec := range labels {
			if rec.Action == report.ActionBucket {
				if a.Buckets == nil {
					a.Buckets = map[string][]string{}
				}
				a.Buckets[rec.Signal.Parent] = append(a.Buckets[rec.Signal.Parent], rec.Signal.Name)
				if !slices.Contains(bucketed, rec.Signal.Name) {
					bucketed = append(bucketed, rec.Signal.Name)
				}
				continue
			}
			a.Labels[rec.Signal.Parent] = append(a.Labels[rec.Signal.Parent], rec.Signal.Name)
			if !slices.Contains(names, rec.Signal.Name) {
				names = append(names, rec.Signal.Name)
			}
		}
		slices.Sort(names)
		slices.Sort(bucketed)
		var why []string
		if len(names) > 0 {
			why = append(why, fmt.Sprintf("%s unread on %d %s", strings.Join(names, ", "), len(a.Labels), map[bool]string{true: "metric", false: "metrics"}[len(a.Labels) == 1]))
		}
		if len(bucketed) > 0 {
			why = append(why, strings.Join(bucketed, ", ")+" read only by class: bucketed")
		}
		a.Reason = strings.Join(why, "; ")
		agg = &a
	}
	var b strings.Builder
	var failure, tierFailure, dedupFailure string
	metricsDone, tierDone, dedupDone := false, false, false
	metricRecs := len(ok)+len(labels) > 0
	for _, f := range p.Files {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			failure = err.Error()
			break
		}
		pipes, err := propose.OtelcolPipelines(src)
		if err != nil {
			failure = f + ": " + err.Error()
			break
		}
		out := src
		if len(pipes) > 0 {
			var dp *propose.Drop
			if len(ok) > 0 {
				d := dropOf(p.Name, ok, scan)
				d.PromNames = true
				d.Collector = p.Version
				dp = &d
			}
			if out, err = propose.OtelcolChanges(src, pipes, dp, agg); err != nil {
				failure = f + ": " + err.Error()
				break
			}
			metricsDone = metricsDone || metricRecs
		}
		if dedup != nil && !dedupDone {
			lp, err := propose.OtelcolLogsShipper(out, p.Cold)
			if err == nil && lp != "" {
				var dout []byte
				if dout, err = propose.OtelcolDedup(out, []string{lp}, dedup); err == nil {
					out, dedupDone = dout, true
				}
			}
			if err != nil {
				dedupFailure = f + ": " + err.Error()
			}
		}
		if tier != nil && !tierDone {
			lp, err := propose.OtelcolLogsShipper(out, p.Cold)
			if err == nil && lp != "" {
				var tout []byte
				if tout, err = propose.OtelcolTier(out, lp, tier); err == nil {
					out, tierDone = tout, true
				}
			}
			if err != nil {
				tierFailure = f + ": " + err.Error()
			}
		}
		if string(out) == string(src) {
			continue
		}
		if err := check(root, f, out); err != nil {
			failure = f + ": " + err.Error()
			break
		}
		b.WriteString(propose.Diff(f, src, out))
	}
	if failure == "" && metricRecs && !metricsDone {
		failure = "no metrics pipeline in " + strings.Join(p.Files, ", ")
	}
	if dedup != nil && !dedupDone && dedupFailure == "" {
		dedupFailure = "no logs pipeline ships to an exporter in " + strings.Join(p.Files, ", ")
	}
	if tier != nil && !tierDone && tierFailure == "" {
		tierFailure = "no logs pipeline ships to an exporter in " + strings.Join(p.Files, ", ")
	}
	if failure == "" && form.Note != "" {
		note(form.Note)
	}
	if failure == "" {
		d := dropOf(p.Name, append(slices.Clone(ok), labels...), scan)
		var modules []string
		if len(ok) > 0 {
			modules = append(modules, propose.FilterProcessorModule)
		}
		if agg != nil {
			modules = append(modules, propose.TransformProcessorModule, propose.IntervalProcessorModule)
		}
		if tierDone && len(ok) == 0 {
			modules = append(modules, propose.FilterProcessorModule)
		}
		if dedupDone {
			modules = append(modules, propose.LogDedupProcessorModule)
		}
		mdiff, err := ensureFilterBuild(p, root, d, modules, note)
		if err != nil {
			failure = "the Collector build can't take filter/tvt: " + err.Error()
		}
		b.WriteString(mdiff)
	}
	for _, rec := range append(slices.Clone(ok), labels...) {
		if failure != "" {
			skipped = append(skipped, proposal{Rec: rec, Note: failure})
		} else {
			written = append(written, proposal{Rec: rec})
		}
	}
	for _, rec := range collapsed {
		switch {
		case failure != "":
			skipped = append(skipped, proposal{Rec: rec, Note: failure})
		case dedupFailure != "":
			skipped = append(skipped, proposal{Rec: rec, Note: dedupFailure})
		default:
			written = append(written, proposal{Rec: rec})
		}
	}
	for _, rec := range tiered {
		switch {
		case failure != "":
			skipped = append(skipped, proposal{Rec: rec, Note: failure})
		case tierFailure != "":
			skipped = append(skipped, proposal{Rec: rec, Note: tierFailure})
		default:
			written = append(written, proposal{Rec: rec})
		}
	}
	if failure != "" || len(written) == 0 {
		return nil, skipped, ""
	}
	return written, skipped, b.String()
}

// checkFunc validates one changed file of a pipeline (S7 item 5): root is
// the pipeline's checkout, out the file's new content. An error skips the
// file's recommendations with the reason.
type checkFunc func(root, file string, out []byte) error

// validator runs each pipeline's own validator on its changed files, in a
// copy of the checkout with tvt's changes written in, so references to
// other files (rule files, imports) resolve. notes records, per repo,
// what validated each file or why nothing did.
type validator struct {
	o      proposeOptions
	notes  []string
	build  []string          // per repo: what the Collector build has, or was given (S7 item 4)
	copies map[string]string // pipeline -> its scratch copy
	clean  []func()
}

func (v *validator) cleanup() {
	for _, c := range v.clean {
		c()
	}
}

func (v *validator) check(ctx context.Context, p config.Pipeline) checkFunc {
	return func(root, file string, out []byte) error {
		if v.o.Validate == "off" {
			v.notes = append(v.notes, fmt.Sprintf("%s %s: not validated (--validate off)", p.Repo, file))
			return nil
		}
		// Operator custom resources aren't a pipeline config (the operator
		// builds one): kubeconform checks them against their schemas.
		kind := p.Kind
		if kind == "prometheus-operator" || (kind == "fluentbit" && propose.IsFluentOperator(out)) {
			kind = "kubernetes"
		}
		if p.HelmValues != nil && file == p.HelmValues.File {
			// A values file isn't the pipeline's config: the chart renders
			// it. tvt checked the edit against the joined config itself.
			if v.o.Validate == "require" {
				return fmt.Errorf("not validated (--validate require): Helm values; render the chart with helm template to check")
			}
			v.notes = append(v.notes, fmt.Sprintf("%s %s: NOT VALIDATED: Helm values; render the chart with helm template to check", p.Repo, file))
			return nil
		}
		if v.copies == nil {
			v.copies = map[string]string{}
		}
		dir, ok := v.copies[p.Name]
		if !ok {
			scratch, done, err := validate.Scratch()
			if err != nil {
				return fmt.Errorf("validation: %w", err)
			}
			v.clean = append(v.clean, done)
			dir = filepath.Join(scratch, "repo")
			if err := copyTree(root, dir); err != nil {
				return fmt.Errorf("validation: copying the checkout: %w", err)
			}
			v.copies[p.Name] = dir
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(file)), out, 0o644); err != nil {
			return fmt.Errorf("validation: %w", err)
		}
		res := validate.Run(ctx, kind, dir, file, validate.Options{Images: v.o.Images, Runtime: v.o.Runtime, FeatureGates: p.FeatureGates})
		switch {
		case res.Ran && res.OK:
			note := fmt.Sprintf("%s %s: valid (%s)", p.Repo, file, res.By)
			if kind == "kubernetes" {
				if n := validate.Unchecked(res.Output); n > 0 {
					note += fmt.Sprintf("; %s of other kinds not checked", plural(n, "resource", "resources"))
				}
			}
			v.notes = append(v.notes, note)
			return nil
		case res.Ran:
			return fmt.Errorf("%s rejected the changed config: %s", res.By, validate.FirstLines(res.Output, 3))
		case v.o.Validate == "require":
			return fmt.Errorf("not validated (--validate require): %s", res.Skipped)
		}
		v.notes = append(v.notes, fmt.Sprintf("%s %s: NOT VALIDATED: %s", p.Repo, file, res.Skipped))
		return nil
	}
}

// copyTree copies a checkout's regular files (not .git) to dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
}

// ensureFilterBuild makes the pipeline's Collector build include the
// filter processor (S7 item 4) and returns the manifest's diff. The
// manifest is pipelines[].build, or a builder-config.yaml next to one of
// the pipeline's files. With none, the distribution is assumed to be
// otelcol-contrib, which has the filter processor, and the patch says so.
func ensureFilterBuild(p config.Pipeline, root string, d propose.Drop, modules []string, note func(string)) (string, error) {
	path := p.Build
	if path == "" {
		for _, f := range p.Files {
			cand := filepath.ToSlash(filepath.Join(filepath.Dir(filepath.FromSlash(f)), "builder-config.yaml"))
			if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(cand))); err == nil && propose.IsBuilderManifest(b) {
				path = cand
				break
			}
		}
	}
	if path == "" {
		note("no ocb builder manifest (pipelines[].build, or builder-config.yaml next to the config): assumed otelcol-contrib, which includes the processors tvt writes")
		return "", nil
	}
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return "", err
	}
	out := src
	for i, m := range modules {
		bm := d
		bm.ID = fmt.Sprintf("%s/%d", d.ID, i)
		ensure := propose.EnsureProcessorKeep
		if i == 0 {
			ensure = propose.EnsureProcessor // removes tvt's old blocks, once
		}
		next, version, err := ensure(out, m, bm)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		short := m[strings.LastIndex(m, "/")+1:]
		if version == "" {
			note(path + ": already includes " + short)
		} else {
			note(fmt.Sprintf("%s: adds %s %s (rebuild the Collector with ocb before deploying)", path, short, version))
		}
		out = next
	}
	return propose.Diff(path, src, out), nil
}

// proposeFluentBit writes stream reroutes into a Fluent Bit pipeline: a
// rewrite_tag per stream, moving its records to the cold output
// pipelines[].cold names (propose.FluentBitRoutes). A stream must be
// named by label equalities only, and its Loki output must carry exactly
// those labels. All of a file's routes are written, or none.
func proposeFluentBit(p config.Pipeline, recs []report.Recommendation, root string, scan int64, check checkFunc) (written, skipped []proposal, diff string) {
	var ok []report.Recommendation
	var routes []propose.Route
	for _, rec := range recs {
		if rec.Action != report.ActionReroute && rec.Action != report.ActionLevelTier {
			skipped = append(skipped, proposal{Rec: rec, Note: "a fluentbit pipeline carries logs; no writer for " + rec.Action + " there"})
			continue
		}
		if p.Cold == "" {
			skipped = append(skipped, proposal{Rec: rec, Note: "pipeline " + p.Name + " names no cold output (tvt.yaml pipelines[].cold)"})
			continue
		}
		labels, err := selectorLabels(rec.Signal.Name)
		if err != nil {
			skipped = append(skipped, proposal{Rec: rec, Note: err.Error()})
			continue
		}
		d := dropOf(rec.ID, []report.Recommendation{rec}, scan)
		routes = append(routes, propose.Route{ID: rec.ID, Scan: scan, Evidence: d.Evidence, Reason: d.Reason, Stream: labels, Cold: p.Cold, BelowWarn: rec.Action == report.ActionLevelTier})
		ok = append(ok, rec)
	}
	if len(ok) == 0 {
		return nil, skipped, ""
	}
	// Each route goes into the first file that has its stream's output.
	byFile := map[string][]int{}
	srcs := map[string][]byte{}
	failure := map[int]string{}
	for i, r := range routes {
		var last error
		for _, f := range p.Files {
			if _, read := srcs[f]; !read {
				b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
				if err != nil {
					last = err
					continue
				}
				srcs[f] = b
			}
			if _, err := fluentRoutes(f, srcs[f])(srcs[f], []propose.Route{r}); err != nil {
				last = fmt.Errorf("%s: %w", f, err)
				continue
			}
			byFile[f] = append(byFile[f], i)
			last = nil
			break
		}
		if last != nil {
			failure[i] = last.Error()
		}
	}
	var b strings.Builder
	for _, f := range p.Files {
		idx := byFile[f]
		if len(idx) == 0 {
			continue
		}
		var rs []propose.Route
		for _, i := range idx {
			rs = append(rs, routes[i])
		}
		out, err := fluentRoutes(f, srcs[f])(srcs[f], rs)
		if err == nil {
			err = check(root, f, out)
		}
		if err != nil {
			for _, i := range idx {
				failure[i] = f + ": " + err.Error()
			}
			continue
		}
		b.WriteString(propose.Diff(f, srcs[f], out))
	}
	for i, rec := range ok {
		if note, bad := failure[i]; bad {
			skipped = append(skipped, proposal{Rec: rec, Note: note})
		} else {
			written = append(written, proposal{Rec: rec})
		}
	}
	return written, skipped, b.String()
}

// selectorLabels is a stream selector's labels, when it names them all by
// equality ({app="shop", level="debug"}).
func selectorLabels(selector string) (map[string]string, error) {
	refs, err := parse.LogQL(selector)
	if err != nil || len(refs) != 1 || len(refs[0].Matchers) == 0 {
		return nil, fmt.Errorf("stream %s isn't one selector tvt can read", selector)
	}
	out := map[string]string{}
	for _, m := range refs[0].Matchers {
		if m.Op != parse.MatchEqual || m.Variable {
			return nil, fmt.Errorf("stream %s matches %s by more than equality; tvt reroutes streams named by exact labels", selector, m.Label)
		}
		out[m.Label] = m.Value
	}
	dropDerivedServiceName(out)
	return out, nil
}

// lokiServiceSources is the order in which Loki 3 (limits_config
// discover_service_name) picks a stream's service_name from its other
// labels when the stream has none.
var lokiServiceSources = []string{"service", "app", "application", "app_name", "name", "app_kubernetes_io_name",
	"container", "container_name", "k8s_container_name", "component", "workload", "job", "k8s_job_name"}

// dropDerivedServiceName removes a service_name label Loki added at
// ingest: its value is the first of lokiServiceSources the stream has. No
// pipeline sets it, so a rule that matched on it would never match the
// stream where tvt writes (the live lab's debug streams carry
// service_name="shop" from app="shop").
func dropDerivedServiceName(l map[string]string) {
	sn, ok := l["service_name"]
	if !ok {
		return
	}
	for _, k := range lokiServiceSources {
		if v, ok := l[k]; ok {
			if v == sn {
				delete(l, "service_name")
			}
			return
		}
	}
}

// proposeMonitors writes whole-metric drops into a prometheus-operator
// pipeline: a metricRelabelings rule on every endpoint of each monitor
// whose targets carry the metric's jobs (propose.MonitorJobs, plus
// pipelines[].monitor_jobs for jobs the manifests can't tell). A metric
// is written only when every job it carries maps to a monitor in the
// pipeline's files.
func proposeMonitors(p config.Pipeline, recs []report.Recommendation, jobs map[string][]string, root string, scan int64, check checkFunc) (written, skipped []proposal, diff string) {
	if len(recs) == 0 {
		return nil, nil, ""
	}
	srcs := map[string][]byte{}
	var all [][]byte
	for _, f := range p.Files {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			for _, rec := range recs {
				skipped = append(skipped, proposal{Rec: rec, Note: err.Error()})
			}
			return nil, skipped, ""
		}
		srcs[f] = b
		all = append(all, b)
	}
	byJob, unresolved, err := propose.MonitorJobs(all...)
	if err != nil {
		for _, rec := range recs {
			skipped = append(skipped, proposal{Rec: rec, Note: err.Error()})
		}
		return nil, skipped, ""
	}
	for job, ref := range p.MonitorJobs {
		parts := strings.SplitN(ref, "/", 3)
		if len(parts) == 3 {
			byJob[job] = []propose.MonitorRef{{Kind: parts[0], Namespace: parts[1], Name: parts[2]}}
		}
	}
	plan := map[propose.MonitorRef][]report.Recommendation{}
	for _, rec := range recs {
		js := jobs[rec.Signal.ID]
		if len(js) == 0 {
			skipped = append(skipped, proposal{Rec: rec, Note: fmt.Sprintf("scan %d didn't read the metric's job label", scan)})
			continue
		}
		var missing []string
		for _, j := range js {
			if len(byJob[j]) == 0 {
				missing = append(missing, j)
			}
		}
		if len(missing) > 0 {
			note := fmt.Sprintf("job %s maps to no monitor in %s", strings.Join(missing, ", "), strings.Join(p.Files, ", "))
			if len(unresolved) > 0 {
				note += " (set pipelines[].monitor_jobs for monitors tvt can't resolve)"
			}
			skipped = append(skipped, proposal{Rec: rec, Note: note})
			continue
		}
		for _, j := range js {
			for _, m := range byJob[j] {
				plan[m] = append(plan[m], rec)
			}
		}
	}
	byMonitor := map[propose.MonitorRef]propose.Drop{}
	for m, rs := range plan {
		byMonitor[m] = dropOf(p.Name+"/"+m.Namespace+"/"+m.Name, rs, scan)
	}
	if p.HelmValues != nil {
		w, s, d := proposeHelmValues(p, recs, plan, byMonitor, root, check)
		return w, append(skipped, s...), d
	}
	var b strings.Builder
	failed := map[string]string{}
	done := 0
	for _, f := range p.Files {
		out, n, err := propose.Monitors(srcs[f], byMonitor)
		if err == nil && n > 0 {
			err = check(root, f, out)
		}
		if err != nil {
			for _, rs := range plan {
				for _, rec := range rs {
					failed[rec.ID] = f + ": " + err.Error()
				}
			}
			continue
		}
		done += n
		b.WriteString(propose.Diff(f, srcs[f], out))
	}
	if done < len(byMonitor) && len(failed) == 0 {
		for _, rs := range plan {
			for _, rec := range rs {
				failed[rec.ID] = "a monitor for its jobs isn't in " + strings.Join(p.Files, ", ")
			}
		}
		b.Reset()
	}
	for _, rec := range recs {
		if note, ok := failed[rec.ID]; ok {
			skipped = append(skipped, proposal{Rec: rec, Note: note})
		} else if !slices.ContainsFunc(skipped, func(s proposal) bool { return s.Rec.ID == rec.ID }) {
			written = append(written, proposal{Rec: rec})
		}
	}
	if len(failed) > 0 {
		return nil, skipped, ""
	}
	return written, skipped, b.String()
}

// proposeHelmValues writes a prometheus-operator pipeline's drops into
// kube-prometheus-stack's values (helm_values) instead of its rendered
// monitors: each monitor's rule goes into the values keys that render its
// endpoints' metricRelabelings (propose.KPSValuesKeys), a new key starting
// with the chart's default rules from helm_values.defaults. The rendered
// manifests in files only resolve jobs to monitors.
func proposeHelmValues(p config.Pipeline, recs []report.Recommendation, plan map[propose.MonitorRef][]report.Recommendation, byMonitor map[propose.MonitorRef]propose.Drop, root string, check checkFunc) (written, skipped []proposal, diff string) {
	hv := p.HelmValues
	fail := func(note string) ([]proposal, []proposal, string) {
		for _, rs := range plan {
			for _, rec := range rs {
				if !slices.ContainsFunc(skipped, func(s proposal) bool { return s.Rec.ID == rec.ID }) {
					skipped = append(skipped, proposal{Rec: rec, Note: note})
				}
			}
		}
		return nil, skipped, ""
	}
	if hv.Defaults == "" {
		return fail("helm_values.defaults names no chart values.yaml; without it tvt can't keep the chart's default rules, which Helm would replace")
	}
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(hv.File)))
	if err != nil {
		return fail(err.Error())
	}
	defaults, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(hv.Defaults)))
	if err != nil {
		return fail(err.Error())
	}
	byPath := map[string]propose.Drop{}
	for m, d := range byMonitor {
		keys := propose.KPSValuesKeys(m.Name)
		if keys == nil {
			return fail(m.String() + " isn't a monitor kube-prometheus-stack renders from values")
		}
		for _, k := range keys {
			byPath[k] = d
		}
	}
	out, err := propose.HelmValues(src, defaults, hv.Path, byPath)
	if err == nil {
		err = check(root, hv.File, out)
	}
	if err != nil {
		return fail(hv.File + ": " + err.Error())
	}
	for _, rs := range plan {
		for _, rec := range rs {
			if !slices.ContainsFunc(written, func(w proposal) bool { return w.Rec.ID == rec.ID }) {
				written = append(written, proposal{Rec: rec})
			}
		}
	}
	return written, skipped, propose.Diff(hv.File, src, out)
}

// proposeAlloyRoutesOnly writes an Alloy pipeline's stream reroutes when
// it has no metric drops: into the first file that has the loki.process
// shipping to Loki.
func proposeAlloyRoutesOnly(p config.Pipeline, recs []report.Recommendation, routes []propose.Route, checkouts checkoutFlag, scan int64, check checkFunc, skipped []proposal) (written, _ []proposal, diff string) {
	fail := func(note string) ([]proposal, []proposal, string) {
		for _, rec := range recs {
			if !slices.ContainsFunc(skipped, func(s proposal) bool { return s.Rec.ID == rec.ID }) {
				skipped = append(skipped, proposal{Rec: rec, Note: note})
			}
		}
		return nil, skipped, ""
	}
	if len(routes) == 0 {
		return nil, skipped, ""
	}
	root, ok := checkouts[p.Repo]
	if !ok {
		return fail("no --checkout for " + p.Repo)
	}
	var last error
	for _, f := range p.Files {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			last = err
			continue
		}
		out, err := propose.AlloyRoutes(src, routes)
		if err == nil {
			err = check(root, f, out)
		}
		if err != nil {
			last = fmt.Errorf("%s: %w", f, err)
			continue
		}
		for _, rec := range recs {
			if !slices.ContainsFunc(skipped, func(s proposal) bool { return s.Rec.ID == rec.ID }) {
				written = append(written, proposal{Rec: rec})
			}
		}
		return written, skipped, propose.Diff(f, src, out)
	}
	return fail(fmt.Sprint(last))
}

// fluentRoutes is the Fluent Bit writer for a file's format: Fluent
// Operator resources, classic for .conf, YAML otherwise.
func fluentRoutes(file string, src []byte) func([]byte, []propose.Route) ([]byte, error) {
	switch {
	case propose.IsFluentOperator(src):
		return propose.FluentOperatorRoutes
	case strings.HasSuffix(strings.ToLower(file), ".conf"):
		return propose.FluentBitClassicRoutes
	}
	return propose.FluentBitRoutes
}

// proposeFluentBitHelm writes stream reroutes into the fluent/fluent-bit
// chart's values (helm_values): a rewrite_tag [FILTER] in config.filters,
// created from the chart's default filters (helm_values.defaults) when
// the overlay doesn't set it, since Helm replaces the string.
func proposeFluentBitHelm(p config.Pipeline, recs []report.Recommendation, root string, scan int64, check checkFunc) (written, skipped []proposal, diff string) {
	hv := p.HelmValues
	var ok []report.Recommendation
	var routes []propose.Route
	for _, rec := range recs {
		if rec.Action != report.ActionReroute && rec.Action != report.ActionLevelTier {
			skipped = append(skipped, proposal{Rec: rec, Note: "a fluentbit pipeline carries logs; no writer for " + rec.Action + " there"})
			continue
		}
		if p.Cold == "" {
			skipped = append(skipped, proposal{Rec: rec, Note: "pipeline " + p.Name + " names no cold output (tvt.yaml pipelines[].cold)"})
			continue
		}
		labels, err := selectorLabels(rec.Signal.Name)
		if err != nil {
			skipped = append(skipped, proposal{Rec: rec, Note: err.Error()})
			continue
		}
		d := dropOf(rec.ID, []report.Recommendation{rec}, scan)
		routes = append(routes, propose.Route{ID: rec.ID, Scan: scan, Evidence: d.Evidence, Reason: d.Reason, Stream: labels, Cold: p.Cold, BelowWarn: rec.Action == report.ActionLevelTier})
		ok = append(ok, rec)
	}
	fail := func(note string) ([]proposal, []proposal, string) {
		for _, rec := range ok {
			skipped = append(skipped, proposal{Rec: rec, Note: note})
		}
		return nil, skipped, ""
	}
	if len(routes) == 0 {
		return nil, skipped, ""
	}
	if hv.Defaults == "" {
		return fail("helm_values.defaults names no chart values.yaml; without it tvt can't keep the chart's default filters, which Helm would replace")
	}
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(hv.File)))
	if err != nil {
		return fail(err.Error())
	}
	defaults, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(hv.Defaults)))
	if err != nil {
		return fail(err.Error())
	}
	out, err := propose.FluentBitHelmRoutes(src, defaults, hv.Path, routes)
	if err == nil {
		err = check(root, hv.File, out)
	}
	if err != nil {
		return fail(hv.File + ": " + err.Error())
	}
	for _, rec := range ok {
		written = append(written, proposal{Rec: rec})
	}
	return written, skipped, propose.Diff(hv.File, src, out)
}

// intervalRec shows a scrape interval change (S10 item 5) in propose's
// output and the PR body as a row like a recommendation's. It isn't one:
// nothing is dropped, so check asks for no owner confirmation, and its
// guard is the manifest's scrape limits.
func intervalRec(iv report.IntervalChange, currency string) report.Recommendation {
	every := model.Duration(time.Duration(iv.ProposedSec * float64(time.Second))).String()
	now := model.Duration(time.Duration(iv.CurrentSec * float64(time.Second))).String()
	why := fmt.Sprintf("job %s every %s instead of %s (%d metrics, %d readers)", iv.Job, every, now, iv.Metrics, iv.Readers)
	if iv.Bound != "" {
		why += "; not longer: " + iv.Bound
	}
	return report.Recommendation{ID: propose.IntervalPrefix + iv.Job, Action: report.ActionDownsample,
		Signal:   report.SignalRef{ID: "job:" + iv.Job, Kind: "job", Name: iv.Job},
		Pipeline: iv.Pipeline, Proposable: true, Claim: why,
		Savings:  report.Savings{Monthly: iv.Monthly, Currency: currency},
		Expected: report.Expected{Statements: []string{fmt.Sprintf("job %s is scraped every %s; every reader's range still holds four samples", iv.Job, every)}, SeriesAfter: -1}}
}
