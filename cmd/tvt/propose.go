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
	"github.com/Opsmastery-Summit/tvt/pkg/propose"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

type proposeOptions struct {
	DB        string
	Config    string
	Report    string
	Output    string
	Dir       string
	Checkouts checkoutFlag
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
	o := proposeOptions{Checkouts: checkoutFlag{}}
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.DB, "db", "tvt.db", "path to the usage graph database (from tvt scan)")
	fs.StringVar(&o.Config, "config", "", "tvt.yaml: the pipelines the drops go into (required)")
	fs.StringVar(&o.Report, "report", "", "the reviewed report JSON (from tvt report); must be of the database's latest scan (default: build it from the database)")
	fs.StringVar(&o.Output, "output", "patch", "what to write: patch (one git-apply patch per pipeline repo)")
	fs.StringVar(&o.Dir, "dir", "proposals", "where to write the patches")
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
		case rec.Action != report.ActionDrop || rec.Signal.Kind != string(graph.KindMetric):
			skipped = append(skipped, proposal{Rec: rec, Note: "no writer for " + rec.Action + " of a " + rec.Signal.Kind + " yet"})
		default:
			byPipeline[rec.Pipeline] = append(byPipeline[rec.Pipeline], rec)
		}
	}
	patches := map[string]*strings.Builder{} // by repo
	var written []proposal
	for _, p := range cfg.Pipelines {
		recs := byPipeline[p.Name]
		if len(recs) == 0 {
			continue
		}
		done, skip, diff := proposePipeline(p, recs, jobs, o.Checkouts, r.Scan.ID)
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
	return writeProposals(o, r.Scan.ID, patches, written, skipped, out)
}

// proposePipeline writes recs (whole-metric drops) into pipeline p's
// files from its checkout and returns the diff. A recommendation is
// written only when every job its metric carries has a scrape config in
// p's files: a drop in some jobs would leave the rest of the series, and
// the savings, in place.
func proposePipeline(p config.Pipeline, recs []report.Recommendation, jobs map[string][]string, checkouts checkoutFlag, scan int64) (written, skipped []proposal, diff string) {
	skipAll := func(note string) ([]proposal, []proposal, string) {
		for _, rec := range recs {
			skipped = append(skipped, proposal{Rec: rec, Note: note})
		}
		return nil, skipped, ""
	}
	var listJobs func([]byte) ([]string, error)
	switch p.Kind {
	case "prometheus":
		listJobs = propose.PrometheusJobs
	case "alloy":
		listJobs = propose.AlloyJobs
	case "otelcol":
		// propose.Otelcol needs the names the receivers emit, and the
		// report has the backend's (Prometheus translation adds units and
		// _total). Mapping one to the other is S7 item 9.
		return skipAll("the otelcol writer needs each metric's OTel name, which the scan doesn't map yet (S7 item 9)")
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
func writeProposals(o proposeOptions, scan int64, patches map[string]*strings.Builder, written, skipped []proposal, out io.Writer) error {
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
		b.WriteString("\nCheck each with tvt why <signal> before merging. Apply with: git apply <this file>\n\n")
		b.WriteString(patches[repo].String())
		path := filepath.Join(o.Dir, report.ManifestDir(repo)+".patch")
		if err := writeFileAtomic(path, []byte(b.String())); err != nil {
			return err
		}
		fmt.Fprintf(out, "patch for %s written to %s\n", repo, path)
	}
	fmt.Fprintf(out, "scan %d: %d drops written, %d recommendations skipped\n", scan, len(written), len(skipped))
	sort.SliceStable(skipped, func(i, j int) bool { return skipped[i].Note < skipped[j].Note })
	for _, s := range skipped {
		fmt.Fprintf(out, "  skipped %s: %s\n", s.Rec.ID, s.Note)
	}
	return nil
}
