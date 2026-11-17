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

	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/propose"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/validate"
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
}

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
	fs.StringVar(&o.Output, "output", "patch", "what to write: patch (one git-apply patch per pipeline repo)")
	fs.StringVar(&o.Dir, "dir", "proposals", "where to write the patches")
	fs.StringVar(&o.Validate, "validate", "auto", "run each pipeline's own validator on the changed config: auto (when promtool, alloy, otelcol-contrib, fluent-bit or a container runtime is available), require (skip what can't be validated), off")
	fs.Var(o.Images, "validator-image", "kind=image: validate that pipeline kind with this image, your distribution and version (repeatable)")
	fs.StringVar(&o.Runtime, "container-runtime", "", "podman or docker for validators not on PATH (default: whichever answers; none: never)")
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
	if o.Output != "patch" {
		fmt.Fprintf(stderr, "tvt propose: --output %q: only patch is built (pull requests come with S9 approvals)\n", o.Output)
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
	for _, rec := range r.Recommendations {
		switch {
		case !rec.Proposable:
			skipped = append(skipped, proposal{Rec: rec, Note: "not proposable: " + cmp.Or(rec.PipelineNote, "no pipeline")})
		case !(rec.Action == report.ActionDrop && rec.Signal.Kind == string(graph.KindMetric)) && !(rec.Action == report.ActionReroute && rec.Signal.Kind == string(graph.KindStream)) && !(rec.Action == report.ActionLabelRemoval && rec.Signal.Kind == string(graph.KindLabel)):
			skipped = append(skipped, proposal{Rec: rec, Note: "no writer for " + rec.Action + " of a " + rec.Signal.Kind + " yet"})
		default:
			byPipeline[rec.Pipeline] = append(byPipeline[rec.Pipeline], rec)
		}
	}
	v := &validator{o: o}
	defer v.cleanup()
	patches := map[string]*strings.Builder{} // by repo
	var written []proposal
	for _, p := range cfg.Pipelines {
		recs := byPipeline[p.Name]
		if len(recs) == 0 {
			continue
		}
		done, skip, diff := proposePipeline(p, recs, jobs, o.Checkouts, r.Scan.ID, v.check(ctx, p), func(n string) { v.build = append(v.build, p.Repo+" "+n) })
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
	return writeProposals(o, r.Scan.ID, patches, written, skipped, v.notes, v.build, out)
}

// proposePipeline writes recs (whole-metric drops) into pipeline p's
// files from its checkout and returns the diff. A recommendation is
// written only when every job its metric carries has a scrape config in
// p's files: a drop in some jobs would leave the rest of the series, and
// the savings, in place.
func proposePipeline(p config.Pipeline, recs []report.Recommendation, jobs map[string][]string, checkouts checkoutFlag, scan int64, check checkFunc, note func(string)) (written, skipped []proposal, diff string) {
	skipAll := func(note string) ([]proposal, []proposal, string) {
		for _, rec := range recs {
			skipped = append(skipped, proposal{Rec: rec, Note: note})
		}
		return nil, skipped, ""
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
		case len(p.Files) == 0:
			return skipAll("pipeline " + p.Name + " lists no files (Helm values aren't written yet)")
		}
		return proposeFluentBit(p, recs, root, scan, check)
	}
	// Stream reroutes have a writer only for Fluent Bit so far.
	var drops []report.Recommendation
	for _, rec := range recs {
		if rec.Action == report.ActionReroute {
			skipped = append(skipped, proposal{Rec: rec, Note: "no writer for stream reroutes in " + p.Kind + " pipelines yet"})
			continue
		}
		if rec.Action == report.ActionLabelRemoval && p.Kind != "otelcol" {
			skipped = append(skipped, proposal{Rec: rec, Note: "no writer for label removals in " + p.Kind + " pipelines yet"})
			continue
		}
		drops = append(drops, rec)
	}
	if recs = drops; len(recs) == 0 {
		return nil, skipped, ""
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
		return proposeOtelcol(p, recs, root, scan, check, note)
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
	var b strings.Builder
	failed := map[string]string{} // rec ID -> why its file couldn't be written
	for _, f := range p.Files {
		byJob := plan[f]
		if len(byJob) == 0 {
			continue
		}
		var out []byte
		var err error
		if p.Kind == "prometheus" {
			drops := map[string]propose.Drop{}
			for j, rs := range byJob {
				drops[j] = dropOf(p.Name+"/"+j, rs, scan)
			}
			out, err = propose.PrometheusDrops(srcs[f], drops)
		} else {
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
		}
		if err == nil {
			err = check(root, f, out)
		}
		if err != nil {
			for _, rs := range byJob {
				for _, rec := range rs {
					failed[rec.ID] = f + ": " + err.Error()
				}
			}
			continue
		}
		b.WriteString(propose.Diff(f, srcs[f], out))
	}
	for _, rec := range recs {
		if note, ok := failed[rec.ID]; ok {
			skipped = append(skipped, proposal{Rec: rec, Note: note})
		} else if slices.ContainsFunc(skipped, func(s proposal) bool { return s.Rec.ID == rec.ID }) {
			continue
		} else {
			written = append(written, proposal{Rec: rec})
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
	var monthly float64
	currency := ""
	for _, rec := range recs {
		ev, _ := json.Marshal(rec.Evidence)
		fmt.Fprintf(h, "%s\n%s\n", rec.ID, ev)
		d.Metrics = append(d.Metrics, rec.Signal.Name)
		monthly += rec.Savings.Monthly
		currency = cmp.Or(currency, rec.Savings.Currency)
	}
	d.Evidence = hex.EncodeToString(h.Sum(nil))[:12]
	d.Reason = plural(len(recs), "unread metric", "unread metrics") + ", see tvt why"
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
func proposeOtelcol(p config.Pipeline, recs []report.Recommendation, root string, scan int64, check checkFunc, note func(string)) (written, skipped []proposal, diff string) {
	form, err := propose.OTTLFormFor(p.Version)
	if err != nil {
		for _, rec := range recs {
			skipped = append(skipped, proposal{Rec: rec, Note: err.Error()})
		}
		return nil, skipped, ""
	}
	var ok []report.Recommendation
	var labels []report.Recommendation
	for _, rec := range recs {
		if rec.Action == report.ActionLabelRemoval {
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
	if len(ok) == 0 && len(labels) == 0 {
		return nil, skipped, ""
	}
	// Label removals: the ordered pipeline for delta counters (S7 item 8).
	var agg *propose.Aggregate
	if len(labels) > 0 {
		a := propose.Aggregate{ID: p.Name + "/labels", Scan: scan, Collector: p.Version, Labels: map[string][]string{}}
		d := dropOf(a.ID, labels, scan)
		a.Evidence = d.Evidence
		var names []string
		for _, rec := range labels {
			a.Labels[rec.Signal.Parent] = append(a.Labels[rec.Signal.Parent], rec.Signal.Name)
			if !slices.Contains(names, rec.Signal.Name) {
				names = append(names, rec.Signal.Name)
			}
		}
		slices.Sort(names)
		a.Reason = fmt.Sprintf("%s unread on %d %s", strings.Join(names, ", "), len(a.Labels), map[bool]string{true: "metric", false: "metrics"}[len(a.Labels) == 1])
		agg = &a
	}
	var b strings.Builder
	var failure string
	files := 0
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
		if len(pipes) == 0 {
			continue
		}
		var dp *propose.Drop
		if len(ok) > 0 {
			d := dropOf(p.Name, ok, scan)
			d.PromNames = true
			d.Collector = p.Version
			dp = &d
		}
		out, err := propose.OtelcolChanges(src, pipes, dp, agg)
		if err == nil {
			err = check(root, f, out)
		}
		if err != nil {
			failure = f + ": " + err.Error()
			break
		}
		b.WriteString(propose.Diff(f, src, out))
		files++
	}
	if failure == "" && files == 0 {
		failure = "no metrics pipeline in " + strings.Join(p.Files, ", ")
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
	if failure != "" {
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
		res := validate.Run(ctx, p.Kind, dir, file, validate.Options{Images: v.o.Images, Runtime: v.o.Runtime})
		switch {
		case res.Ran && res.OK:
			v.notes = append(v.notes, fmt.Sprintf("%s %s: valid (%s)", p.Repo, file, res.By))
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
		if rec.Action != report.ActionReroute {
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
		routes = append(routes, propose.Route{ID: rec.ID, Scan: scan, Evidence: d.Evidence, Reason: d.Reason, Stream: labels, Cold: p.Cold})
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
			if _, err := propose.FluentBit(srcs[f], r); err != nil {
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
		out, err := propose.FluentBitRoutes(srcs[f], rs)
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
	return out, nil
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
