package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
	"github.com/prometheus/common/model"
	"go.yaml.in/yaml/v3"
)

type reportOptions struct {
	DB     string
	Config string
	JSON   string
	HTML   string
	Top    int
	// PriceCard reprices the scan for a what-if report: a card file or a
	// preset name. Empty keeps the scan's prices.
	PriceCard string
	// LabelDrops turns on label-removal recommendations (S6 item 7). Off,
	// every label candidate is held as label_drops_off until a precision
	// review on real data passes.
	LabelDrops bool
	// ManifestDir, when set, also writes each pipeline repo's protection
	// manifest to ManifestDir/<repo>/.tvt/protect.json (S7 item 1, ADR
	// 0003). It needs Config for the pipelines' repos.
	ManifestDir string
	// Digest is where each owner's weekly digest goes (S9 item 6).
	Digest string
}

func cmdReport(args []string, stdout, stderr io.Writer) int {
	var o reportOptions
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.DB, "db", "tvt.db", "path to the usage graph database (from tvt scan)")
	fs.StringVar(&o.Config, "config", "", "tvt.yaml: gaps.acknowledge entries (optional)")
	fs.StringVar(&o.JSON, "json", "report.json", "where to write the JSON report (- for stdout)")
	fs.StringVar(&o.HTML, "html", "report.html", "where to write the HTML report, rendered from the JSON (empty: skip)")
	fs.StringVar(&o.PriceCard, "price-card", "", "what-if: reprice the scan at this price card (a file or a preset: "+strings.Join(cost.Presets(), ", ")+") without rescanning")
	fs.BoolVar(&o.LabelDrops, "label-drops", false, "recommend label removals (labeldrop when no series merge, aggregate when they merge and sum); off until a precision review on real data passes")
	fs.IntVar(&o.Top, "top", 50, "recommendations to keep, highest savings per minute of checking first (0: all)")
	fs.StringVar(&o.Digest, "digest", "", "also write each owner's weekly digest (Markdown, for a GitHub issue) to DIR/<owner>.md")
	fs.StringVar(&o.ManifestDir, "manifest-dir", "", "also write each pipeline repo's protection manifest to DIR/<repo>/.tvt/protect.json (needs --config)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tvt report: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if o.Top < 0 {
		fmt.Fprintf(stderr, "tvt report: --top %d: want 0 or more\n", o.Top)
		return exitUsage
	}
	if o.ManifestDir != "" && o.Config == "" {
		fmt.Fprintln(stderr, "tvt report: --manifest-dir needs --config (the pipelines' repos come from tvt.yaml)")
		return exitUsage
	}
	if err := runReport(context.Background(), o, stdout); err != nil {
		fmt.Fprintf(stderr, "tvt report: %v\n", err)
		return exitError
	}
	return exitOK
}

// runReport builds the JSON report from the latest scan in o.DB and, unless
// o.HTML is empty, renders the HTML report from that JSON. With --json -,
// only the JSON is written (to out). It never creates a database: a
// missing one is an error.
func runReport(ctx context.Context, o reportOptions, out io.Writer) error {
	if _, err := os.Stat(o.DB); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: no such database (run tvt scan first)", o.DB)
	}
	in, tsdb, err := loadReport(ctx, o.DB)
	if err != nil {
		return err
	}
	in.LabelDrops = o.LabelDrops
	in.RecordLogEvents = append([]string{}, report.BuiltinRecordMatches.LogEvents...)
	queue := 0
	if o.Config != "" {
		cfg, err := config.Load(o.Config)
		if err != nil {
			return err
		}
		queue = cfg.Queue.PerOwner
		in.RecordLogEvents = append(in.RecordLogEvents, cfg.Records.LogEvents...)
		in.Objectives, in.GroupWait = detectionOf(cfg)
		if d, err := model.ParseDuration(cfg.Tiers.DormantRetention); err == nil && cfg.Tiers.DormantRetention != "" {
			in.DormantRetention = time.Duration(d)
		}
		in.DormantTenant = cfg.Tiers.DormantTenant
		if in.Decisions, err = repoDecisions(cfg); err != nil {
			return err
		}
	}
	if in.Acknowledged, err = acknowledgements(o.Config); err != nil {
		return err
	}
	if o.PriceCard != "" {
		card, err := cost.Load(o.PriceCard)
		if err != nil {
			return err
		}
		if in, err = report.Reprice(in, card, tsdb); err != nil {
			return fmt.Errorf("--price-card %s: %w", o.PriceCard, err)
		}
	}
	if o.ManifestDir != "" {
		if err := writeManifests(o, in, out); err != nil {
			return err
		}
	}
	r := report.Build(in, o.Top)
	if o.Digest != "" {
		// Every open row counts toward the digests, not just --top's.
		if err := os.MkdirAll(o.Digest, 0o755); err != nil {
			return err
		}
		for _, d := range report.Digests(report.Build(in, 0), queue) {
			if err := writeFileAtomic(filepath.Join(o.Digest, d.File), []byte(d.Markdown)); err != nil {
				return err
			}
			fmt.Fprintf(out, "digest for %s: %s\n", d.Owner, filepath.Join(o.Digest, d.File))
		}
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if o.JSON == "-" {
		_, err = out.Write(b)
		return err
	}
	if err := writeFileAtomic(o.JSON, b); err != nil {
		return err
	}
	if o.HTML != "" {
		var page bytes.Buffer
		if err := report.RenderHTMLFromJSON(&page, b); err != nil {
			return err
		}
		if err := writeFileAtomic(o.HTML, page.Bytes()); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "report of scan %d: %d recommendations (of %d candidates)", r.Scan.ID, r.Totals.Recommendations, r.Totals.Candidates)
	if r.Scan.RepricedFrom != "" {
		fmt.Fprintf(out, ", what-if on %s (scanned on %s)", r.Scan.PriceCard, r.Scan.RepricedFrom)
	}
	if r.Scan.Currency != "" {
		fmt.Fprintf(out, ", %.2f %s/month in metrics", r.Totals.MetricSavings, r.Scan.Currency)
		if r.Totals.StreamSavings > 0 {
			fmt.Fprintf(out, " and up to %.2f in streams", r.Totals.StreamSavings)
		}
	}
	if l := r.Ledger; l != nil {
		measured := 0
		for _, row := range l.Rows {
			if row.Status == report.LedgerMeasured {
				measured++
			}
		}
		fmt.Fprintf(out, "; ledger: %d merged changes measured, saving %.2f/month (claimed %.2f)", measured, l.Saved, l.Expected)
		if l.Settling > 0 {
			fmt.Fprintf(out, ", %d still settling (not counted)", l.Settling)
		}
		if l.Pending > 0 {
			fmt.Fprintf(out, ", %d waiting for a scan after their merge", l.Pending)
		}
	}
	reasons := make([]string, 0, len(r.Held))
	for reason := range r.Held {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		fmt.Fprintf(out, "; %d held (%s)", r.Held[reason], reason)
	}
	written := o.JSON
	if o.HTML != "" {
		written += " and " + o.HTML
	}
	unacked := 0
	for _, g := range r.Gaps {
		if !g.Acknowledged {
			unacked++
		}
	}
	fmt.Fprintf(out, "; %d evidence gaps (%d acknowledged)", len(r.Gaps), len(r.Gaps)-unacked)
	if n := len(r.UnmatchedAcknowledgements); n > 0 {
		fmt.Fprintf(out, ", %d acknowledgements match no gap", n)
	}
	fmt.Fprintf(out, "; written to %s\n", written)
	return nil
}

// writeManifests writes one protection manifest per pipeline repo in
// o.Config, built from the scan (prices don't matter to it).
func writeManifests(o reportOptions, in report.Input, out io.Writer) error {
	cfg, err := config.Load(o.Config)
	if err != nil {
		return err
	}
	ms := buildManifests(cfg, in)
	names := make([]string, 0, len(ms))
	for r := range ms {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		m := ms[r]
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		dir := filepath.Join(o.ManifestDir, report.ManifestDir(r), ".tvt")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, "protect.json")
		if err := writeFileAtomic(path, append(b, '\n')); err != nil {
			return err
		}
		fmt.Fprintf(out, "manifest %s: %d protected signals (%s) written to %s\n", r, len(m.Signals), strings.Join(m.Pipelines, ", "), path)
	}
	return nil
}

// acknowledgements reads tvt.yaml's gaps.acknowledge entries; an empty
// path has none.
func acknowledgements(path string) ([]report.Acknowledgement, error) {
	if path == "" {
		return nil, nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	var out []report.Acknowledgement
	for _, a := range cfg.Gaps.Acknowledge {
		out = append(out, report.Acknowledgement{Source: a.Source, Match: a.Match, Reason: a.Reason})
	}
	return out, nil
}

// loadReport reads the latest scan in path, plus the TSDB measurements it
// stored (nil when it scanned no Prometheus) for what-if repricing.
func loadReport(ctx context.Context, path string) (report.Input, *collect.Storage, error) {
	in, info, err := readLatest(ctx, path)
	return in, info.TSDB, err
}

func readLatest(ctx context.Context, path string) (report.Input, store.ScanInfo, error) {
	st, err := store.Open(ctx, path)
	if err != nil {
		return report.Input{}, store.ScanInfo{}, err
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		return report.Input{}, info, err
	}
	in := report.Input{Scan: report.Scan{
		ID: info.ID, StartedAt: info.StartedAt, WindowDays: info.WindowDays, LogCoverage: info.LogCoverage,
		PriceCard: info.PriceCard, Currency: info.Currency, Approximate: info.PricePreset,
	}}
	if info.SelfHosted != nil {
		in.Retention = info.SelfHosted.Retention
	}
	if in.Signals, err = st.Signals(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.Consumers, err = st.Consumers(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.References, err = st.References(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.Costs, err = st.Costs(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.Sources, err = st.Sources(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.Scores, err = st.Scores(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.StreamVolumes, err = st.StreamVolumes(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.QueryCosts, err = st.QueryCosts(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.SLOFindings, err = st.SLOFindings(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.PipelineTargets, err = st.PipelineTargets(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.OverflowFindings, err = st.OverflowFindings(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.RuleCosts, err = st.RuleCosts(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.RuleFindings, err = st.RuleFindings(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.StreamAttrs, err = st.StreamAttrs(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.LineFindings, err = st.LineFindings(ctx, info.ID); err != nil {
		return in, info, err
	}
	if err := readLedger(ctx, st, info, &in); err != nil {
		return in, info, err
	}
	reads, err := st.LastReads(ctx)
	if err != nil {
		return in, info, err
	}
	for _, r := range reads {
		in.LastReads = append(in.LastReads, report.LastReadOf{SignalID: r.SignalID,
			LastRead: report.LastRead{Consumer: r.ConsumerID, Actor: r.Actor, At: r.At}})
	}
	return in, info, nil
}

// readLedger loads the merged tvt changes and the costs of the scans that
// measure them (S11): for each merge, its proposal's scan, the last scan
// before it and the first after it, plus the current scan.
func readLedger(ctx context.Context, st *store.Store, info store.ScanInfo, in *report.Input) error {
	merges, err := st.Merges(ctx)
	if err != nil || len(merges) == 0 {
		return err
	}
	scans, err := st.Scans(ctx)
	if err != nil {
		return err
	}
	byID := map[int64]store.ScanInfo{}
	for _, s := range scans {
		byID[s.ID] = s
	}
	need := map[int64]bool{info.ID: true}
	for _, m := range merges {
		if s, ok := byID[m.ProposalScan]; ok && s.StartedAt.Before(m.MergedAt) {
			need[m.ProposalScan] = true
		}
		var before, after int64
		for _, s := range scans { // oldest first
			if s.StartedAt.Before(m.MergedAt) {
				before = s.ID
			} else if after == 0 && s.StartedAt.After(m.MergedAt) {
				after = s.ID
			}
		}
		need[before], need[after] = true, true
	}
	delete(need, 0)
	in.Merges = merges
	if in.Collector, err = st.CollectorSent(ctx); err != nil {
		return err
	}
	in.ScanTimes = map[int64]time.Time{}
	for _, s := range scans {
		in.ScanTimes[s.ID] = s.StartedAt
	}
	in.History = map[int64]report.ScanCosts{}
	for id := range need {
		cs, err := st.Costs(ctx, id)
		if err != nil {
			return err
		}
		sc := report.ScanCosts{StartedAt: byID[id].StartedAt, Costs: map[string]graph.Cost{}}
		if sh := byID[id].SelfHosted; sh != nil {
			sc.Retention = sh.Retention
		}
		for _, c := range cs {
			sc.Costs[c.SignalID] = c
		}
		in.History[id] = sc
	}
	return nil
}

// writeFileAtomic writes b to path through a temporary file in the same
// directory, so a failed write never leaves half a report.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tvt-report-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// buildManifests builds the protection manifests from a scan and
// tvt.yaml: the signals (report.Manifests), then what tvt check needs
// beyond them (version 2).
func buildManifests(cfg *config.Config, in report.Input) map[string]report.Manifest {
	repos := map[string]string{}
	for _, p := range cfg.Pipelines {
		repos[p.Name] = p.Repo
	}
	ms := report.Manifests(in, repos)
	// Version 2: what tvt check needs beyond the signals.
	matches := report.RecordMatches{
		Spans:     append(append([]string{}, report.BuiltinRecordMatches.Spans...), cfg.Records.Spans...),
		LogEvents: append(append([]string{}, report.BuiltinRecordMatches.LogEvents...), cfg.Records.LogEvents...),
	}
	alerts := report.AlertTimings(in)
	obj, gw := detectionOf(cfg)
	det := report.ManifestDetection{GroupWaitSec: gw.Seconds()}
	for sev, d := range obj {
		if det.ObjectivesSec == nil {
			det.ObjectivesSec = map[string]float64{}
		}
		det.ObjectivesSec[sev] = d.Seconds()
	}
	var gaps []report.ManifestGap
	var unselected []string
	in.Objectives, in.GroupWait = obj, gw
	full := report.Build(in, 0)
	recsByPipeline := map[string][]report.Recommendation{}
	for _, rec := range full.Recommendations {
		if rec.Pipeline != "" {
			recsByPipeline[rec.Pipeline] = append(recsByPipeline[rec.Pipeline], rec)
		}
	}
	for _, g := range full.Gaps {
		if g.Source == scan.SourceMonitors {
			if m := unselectedRe.FindStringSubmatch(g.Text); m != nil {
				unselected = append(unselected, m[1]+" "+m[2])
			}
		}
		if !g.Acknowledged {
			gaps = append(gaps, report.ManifestGap{Source: g.Source, Gap: g.Text})
		}
	}
	sort.Strings(unselected)
	for r, m := range ms {
		var recs []report.Recommendation
		for _, p := range cfg.Pipelines {
			if p.Repo != r {
				continue
			}
			d := report.PipelineDef{Name: p.Name, Kind: p.Kind, Files: p.Files, Build: p.Build, Version: p.Version, Handles: p.Handles,
				Replicas: p.Replicas, LoadBalanced: p.LoadBalanced, Cold: p.Cold}
			if h := p.HelmValues; h != nil {
				d.HelmValues = &report.ManifestHelm{File: h.File, Path: h.Path, Defaults: h.Defaults}
			}
			m.PipelineDefs = append(m.PipelineDefs, d)
			recs = append(recs, recsByPipeline[p.Name]...)
		}
		m.Evidence = report.EvidenceOf(recs)
		var names []string
		for _, d := range m.PipelineDefs {
			names = append(names, d.Name)
		}
		m.ScrapeLimits = report.ScrapeLimits(full.Intervals, names)
		sort.Slice(m.PipelineDefs, func(i, j int) bool { return m.PipelineDefs[i].Name < m.PipelineDefs[j].Name })
		m.RecordMatches = matches
		if gaps != nil {
			m.Gaps = gaps
		}
		if alerts != nil {
			m.Alerts = alerts
		}
		m.Detection = det
		m.UnselectedMonitors = unselected
		ms[r] = m
	}
	return ms
}

// detectionOf reads tvt.yaml detection: objectives by severity and
// group_wait (Alertmanager's 30s default when unset). Values that don't
// parse are skipped (config.Load has validated them).
func detectionOf(cfg *config.Config) (map[string]time.Duration, time.Duration) {
	gw := report.DefaultGroupWait
	if d, err := model.ParseDuration(cfg.Detection.GroupWait); err == nil && cfg.Detection.GroupWait != "" {
		gw = time.Duration(d)
	}
	var obj map[string]time.Duration
	for sev, o := range cfg.Detection.Objectives {
		if d, err := model.ParseDuration(o); err == nil {
			if obj == nil {
				obj = map[string]time.Duration{}
			}
			obj[sev] = time.Duration(d)
		}
	}
	return obj, gw
}

// unselectedRe reads scan.MonitorSelection's gap line for a monitor no
// server selects.
var unselectedRe = regexp.MustCompile(`^(ServiceMonitor|PodMonitor) (\S+/\S+) is selected by no `)

// repoDecisions reads each pipeline repo's .tvt/decisions.yaml from its
// tvt.yaml checkout (S9): one read per repo, repos without a checkout or
// without decisions skipped.
func repoDecisions(cfg *config.Config) ([]report.RepoDecisions, error) {
	var out []report.RepoDecisions
	seen := map[string]bool{}
	for _, p := range cfg.Pipelines {
		if p.Checkout == "" || seen[p.Repo] {
			continue
		}
		seen[p.Repo] = true
		b, err := os.ReadFile(filepath.Join(p.Checkout, filepath.FromSlash(report.DecisionsPath)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var d report.Decisions
		if err := yaml.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("%s %s: %w", p.Repo, report.DecisionsPath, err)
		}
		out = append(out, report.RepoDecisions{Repo: p.Repo, Decisions: d.Decisions})
	}
	return out, nil
}
