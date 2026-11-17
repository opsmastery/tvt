package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/httpx"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/grafana"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/kube"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/loki"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/mimir"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/prometheus"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/runbook"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/slo"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
)

// Evidence source names, as stored in scan_sources.
const (
	sourcePromQueryLog  = scan.SourcePromQueryLog
	sourceMimirQueryLog = scan.SourceMimirQueryLog
	sourceLokiQueryLog  = scan.SourceLokiQueryLog
	sourceGrafanaLog    = scan.SourceGrafanaLog
)

// Test seams.
var (
	scanNow  = time.Now
	scanHTTP *http.Client // nil: each collector's default client
	// newProbeNonce names one scan's query-log probe; probePoll is how
	// often the logs are re-read while waiting for it.
	newProbeNonce = collect.NewProbeNonce
	probePoll     = 500 * time.Millisecond
)

// scanOptions are the inputs to `tvt scan`. Credentials are read from the
// environment (TVT_GRAFANA_TOKEN, TVT_PROM_TOKEN, TVT_LOKI_TOKEN), never flags,
// so they don't end up in shell history or CI logs.
type scanOptions struct {
	Grafana string
	Prom    string
	Mimir   string
	Loki    string
	DB      string
	Window  int // observation window in days
	// PromQueryLogs and GrafanaLogs are log files or globs on this machine;
	// MimirQueryLogs and LokiQueryLogs hold those backends' own logs (plain
	// or in the CRI container log format).
	PromQueryLogs  stringList
	MimirQueryLogs stringList
	LokiQueryLogs  stringList
	GrafanaLogs    stringList
	// ProbeWait is how long to wait for the probe marker to appear in a
	// query log before recording the log as unproven.
	ProbeWait time.Duration
	// LabelTop is how many of the largest metrics get per-label cardinality.
	LabelTop int
	// Config is tvt.yaml: external consumers and ownership. Optional.
	Config string
	// AllowInsecureToken permits sending a token over plain http to a host
	// that is not loopback.
	AllowInsecureToken bool
	// GrafanaEdition is "oss" (the default) or "cloud". On Grafana Cloud,
	// dashboard views come from Usage Insights instead of the router log.
	GrafanaEdition string
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func cmdScan(args []string, stdout, stderr io.Writer) int {
	var o scanOptions
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.Grafana, "grafana", "", "Grafana base URL (required)")
	fs.StringVar(&o.Prom, "prom", "", "Prometheus base URL")
	fs.StringVar(&o.Mimir, "mimir", "", "Mimir base URL (alternative to --prom); /prometheus is appended if missing")
	fs.StringVar(&o.Loki, "loki", "", "Loki base URL: rules and stream volume are collected")
	fs.StringVar(&o.DB, "db", "tvt.db", "path to the usage graph database")
	fs.IntVar(&o.Window, "window", 30, "observation window in days")
	fs.Var(&o.PromQueryLogs, "prom-query-log", "Prometheus query log file or glob (repeatable; .gz is read)")
	fs.Var(&o.MimirQueryLogs, "mimir-query-log", "Mimir log file or glob with query-frontend query stats lines (repeatable; .gz is read)")
	fs.Var(&o.LokiQueryLogs, "loki-query-log", "Loki log file or glob with metrics.go query lines (repeatable; .gz is read)")
	fs.DurationVar(&o.ProbeWait, "probe-wait", 30*time.Second, "how long to wait for tvt's probe query to appear in each query log")
	fs.Var(&o.GrafanaLogs, "grafana-log", "Grafana log file or glob with router logging on (repeatable; .gz is read)")
	fs.IntVar(&o.LabelTop, "label-top", 50, "collect per-label cardinality for this many of the largest metrics")
	fs.StringVar(&o.Config, "config", "", "tvt.yaml with external_consumers and ownership (optional)")
	fs.BoolVar(&o.AllowInsecureToken, "allow-insecure-token", false, "send tokens over plain http to non-loopback hosts (not recommended)")
	fs.StringVar(&o.GrafanaEdition, "grafana-edition", "oss", "oss, or cloud: dashboard views from Usage Insights, and TVT_PROM_USER/TVT_LOKI_USER sent with the tokens as basic auth")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tvt scan: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if err := o.validate(); err != nil {
		fmt.Fprintf(stderr, "tvt scan: %v\n", err)
		return exitUsage
	}
	if err := o.checkTokenTransport(os.Getenv); err != nil {
		fmt.Fprintf(stderr, "tvt scan: %v\n", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runScan(ctx, o, os.Getenv, stdout); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "tvt scan: interrupted; nothing was written")
		} else {
			fmt.Fprintf(stderr, "tvt scan: %v\n", err)
		}
		return exitError
	}
	return exitOK
}

// runScan collects everything, builds the graph and writes it as one scan.
// The database is opened only after collection succeeds, so a failed or
// interrupted scan leaves it untouched (or never creates it).
func runScan(ctx context.Context, o scanOptions, getenv func(string) string, out io.Writer) error {
	start := scanNow().UTC()
	window := time.Duration(o.Window) * 24 * time.Hour
	since := start.Add(-window)
	in := scan.Inputs{Start: start, Window: window}
	// tvt.yaml first: a typo there fails the scan before it touches any
	// backend, rather than silently dropping an external consumer. SLO
	// files are local, so they're read now too: one tvt can't read fails
	// the scan rather than leaving its SLI unprotected.
	var rbReader runbook.Reader
	if o.Config != "" {
		cfg, err := config.Load(o.Config)
		if err != nil {
			return err
		}
		in.External, in.Owners, in.Clients, in.SweepShare = cfg.ExternalConsumers, cfg.Ownership.Owners, cfg.Clients, cfg.SweepShare
		in.Records = cfg.Records
		in.Pipelines = cfg.Pipelines
		in.MinWindowDays = cfg.WindowDays
		in.Seasons = cfg.Seasons
		// Validated when the config was read.
		in.VolumeBytes, _ = cfg.Storage.VolumeBytes()
		if len(cfg.Kubernetes.Manifests) > 0 {
			objs, err := kube.Load(cfg.Kubernetes.Manifests)
			if err != nil {
				return err
			}
			sel := kube.Resolve(objs, cfg.Kubernetes.Namespace)
			in.Monitors = &sel
			_, summary := scan.MonitorSelection(sel, nil)
			fmt.Fprintf(out, "%-12s %s\n", "monitors", summary)
		}
		if cfg.PriceCard != "" {
			card, err := cost.Load(cfg.PriceCard)
			if err != nil {
				return err
			}
			in.PriceCard = &card
			note := ""
			if card.Preset {
				note = ", a vendor preset: approximate, as of " + card.AsOf
			}
			if card.Kind == cost.KindSelfHosted {
				note = ", self-hosted"
			}
			fmt.Fprintf(out, "%-12s %s (%s%s)\n", "pricecard", card.Name, card.Currency, note)
		}
		rbReader.Fetch = cfg.Runbooks.Fetch
		for _, m := range cfg.Runbooks.Mirrors {
			rbReader.Mirrors = append(rbReader.Mirrors, runbook.Mirror{URLPrefix: m.URLPrefix, Dir: m.Path})
		}
		if len(cfg.SLOs.Paths) > 0 {
			in.SLOsConfigured = true
			if in.SLOs, err = (slo.Files{Paths: cfg.SLOs.Paths}).SLOs(ctx); err != nil {
				return err
			}
			unreadable := 0
			for _, s := range in.SLOs {
				if s.Unreadable != "" {
					unreadable++
				}
			}
			fmt.Fprintf(out, "%-12s %s, %d with an SLI tvt can't read\n", "slos", plural(len(in.SLOs), "SLO", "SLOs"), unreadable)
		}
		if len(cfg.Postmortems.Paths) > 0 {
			if in.Mentions, err = postmortems(cfg.Postmortems.Paths); err != nil {
				return err
			}
			names := map[string]bool{}
			for _, m := range in.Mentions {
				for _, n := range m.Names {
					names[n] = true
				}
			}
			fmt.Fprintf(out, "%-12s %s naming %s\n", "postmortems", plural(len(in.Mentions), "file", "files"), plural(len(names), "metric-shaped name", "metric-shaped names"))
		}
	}
	if !in.SLOsConfigured {
		fmt.Fprintf(out, "%-12s not configured (slos.paths in tvt.yaml)\n", "slos")
	}

	// Query logs: each one tvt can probe gets the marker now, before
	// anything is collected, and must show it by the time it is read.
	qlogs := o.queryLogs(getenv)
	nonce := newProbeNonce()
	for _, ql := range qlogs {
		if ql.probe != nil {
			ql.probeErr = ql.probe.Probe(ctx, nonce)
		}
	}

	// Metrics: cardinality, labels on the largest metrics, rules. Mimir has
	// no TSDB status, so its cardinality comes from the cardinality API; its
	// rules from the same Prometheus-compatible endpoint.
	pc := prometheus.New(o.Prom, promToken(getenv))
	pc.HTTP, pc.Now = scanHTTP, scanNow
	var card collect.CardinalitySource = pc
	in.MetricsSource = "prom"
	if o.Mimir != "" {
		pc.BaseURL, pc.Source, in.MetricsSource = mimirAPI(o.Mimir), "mimir", "mimir"
		mc := mimir.New(pc.BaseURL, pc.Token)
		mc.HTTP, mc.Now = scanHTTP, scanNow
		card = mc
	}
	var err error
	if in.Metrics, err = card.MetricCardinality(ctx); err != nil {
		return err
	}
	var series int64
	for _, m := range in.Metrics {
		series += m.ActiveSeries
	}
	top := min(o.LabelTop, len(in.Metrics)) // sorted largest first
	for _, m := range in.Metrics[:top] {
		ls, err := card.LabelCardinality(ctx, m.Metric)
		if err != nil {
			return err
		}
		in.Labels = append(in.Labels, ls...)
	}
	if in.Rules, err = pc.Rules(ctx); err != nil {
		return err
	}
	// Overflow series (S6 item 5): an evidence gap, so a failed read is a
	// gap line, never a failed scan.
	if ov, err := pc.Overflow(ctx); err != nil {
		in.OverflowErr = err.Error()
	} else {
		in.Overflow, in.OverflowRead = ov, true
	}
	// Pipeline targets (S6 item 4): which values of the labels pipelines'
	// handles match on each metric carries. A failure leaves metrics
	// unmapped with the reason, never the scan failed.
	if labels := scan.HandledLabels(in.Pipelines); len(labels) > 0 {
		lm, err := pc.LabelMetrics(ctx, labels)
		switch {
		case err != nil && lm == nil:
			in.LabelMetricsErr = err.Error()
		case err != nil:
			in.LabelMetrics, in.LabelMetricsErr = lm, err.Error()
		default:
			in.LabelMetrics = lm
		}
	}
	// Scrape intervals turn series into samples per month. They're a cost
	// input, not protection: without them a metric's interval is assumed
	// (Prometheus's default 1m) and recorded as such.
	in.DefaultInterval = time.Minute
	if in.MetricsSource == "prom" {
		in.DefaultIntervalReason = "the targets API gave no interval; Prometheus's default 1m"
		if s, err := pc.Scrape(ctx); err != nil {
			fmt.Fprintf(out, "%-12s scrape intervals unavailable (%v): assuming 1m\n", "prom", err)
		} else {
			in.Scrape = &s
		}
	} else {
		in.DefaultIntervalReason = "Mimir has no targets API; assumed 1m"
	}
	// Prometheus reports its own TSDB on /metrics (a GET, never a query).
	// A self-hosted card prices disk from it; with any card it's stored so
	// the scan can be repriced at a self-hosted card later. Mimir's
	// footprint lives in object storage tvt doesn't read, so a self-hosted
	// card for it must give the retention.
	if in.MetricsSource == "prom" {
		if st, err := pc.Storage(ctx); err != nil {
			fmt.Fprintf(out, "%-12s TSDB storage unavailable (%v): a self-hosted card uses its own and default values\n", "prom", err)
		} else {
			in.Storage = &st
			_, summary := scan.StorageHygiene(st, in.VolumeBytes)
			fmt.Fprintf(out, "%-12s %s\n", "storage", summary)
		}
	}
	if c := in.PriceCard; c != nil && c.Kind == cost.KindSelfHosted {
		if in.Storage == nil && c.SelfHosted.RetentionDays == 0 {
			return fmt.Errorf("price card %q is self_hosted and %s doesn't report its retention: set self_hosted.retention_days", c.Name, in.MetricsSource)
		}
	}
	fmt.Fprintf(out, "%-12s %d metrics (%d series), %d labels on the top %d, %d rules\n",
		in.MetricsSource, len(in.Metrics), series, len(in.Labels), top, len(in.Rules))

	// Loki: rules (consumers), and every stream's bytes per day from the
	// index volume API: a cost input, not protection, so a failure is
	// reported and the scan goes on without stream costs.
	if o.Loki != "" {
		lc := loki.New(o.Loki, lokiToken(getenv))
		lc.HTTP, lc.Now = scanHTTP, scanNow
		rules, err := lc.Rules(ctx)
		if err != nil {
			return err
		}
		in.Rules = append(in.Rules, rules...)
		volume := ""
		vols, err := lc.StreamVolumes(ctx, 24*time.Hour)
		switch {
		case err != nil && !errors.Is(err, collect.ErrTruncated):
			volume = fmt.Sprintf("stream volume unavailable: %v", err)
		default:
			in.StreamVolumes, in.VolumeTruncated = vols, err != nil
			if in.StreamVolumes == nil {
				in.StreamVolumes = []collect.StreamVolume{}
			}
			var total int64
			for _, v := range vols {
				total += v.Bytes
			}
			volume = fmt.Sprintf("%s, %s/day", plural(len(vols), "stream", "streams"), humanBytes(total))
			if in.VolumeTruncated {
				volume += fmt.Sprintf("; truncated at %d series per request, so stream bytes are low", loki.VolumeLimit)
			}
		}
		fmt.Fprintf(out, "%-12s %s (%s)\n", "loki", plural(len(rules), "rule", "rules"), volume)
		if len(in.StreamVolumes) > 0 {
			var msg string
			in.Patterns, msg = streamPatterns(ctx, lc, in.StreamVolumes)
			fmt.Fprintf(out, "%-12s %s\n", "patterns", msg)
		}
	}

	// Grafana: who tvt is (to exclude its own loads), the org, dashboards.
	gc := grafana.New(o.Grafana, getenv("TVT_GRAFANA_TOKEN"))
	gc.HTTP = scanHTTP
	login, err := gc.Login(ctx)
	if err != nil {
		if gc.Token == "" {
			err = fmt.Errorf("%w (TVT_GRAFANA_TOKEN is not set)", err)
		}
		return err
	}
	if in.OrgID, err = gc.OrgID(ctx); err != nil {
		return err
	}
	if in.Dashboards, err = gc.Dashboards(ctx); err != nil {
		return err
	}
	var queries, vars int
	for _, d := range in.Dashboards {
		for _, p := range d.Panels {
			queries += len(p.Queries)
		}
		for _, v := range d.Variables {
			if v.Type == "query" {
				vars++ // custom, constant and textbox variables are values, not consumers
			}
		}
	}
	// Grafana-managed rules: Grafana evaluates them, so no datasource ruler
	// lists them, and a metric only one reads would otherwise look unused.
	grules, err := gc.Rules(ctx)
	if err != nil {
		return err
	}
	in.Rules = append(in.Rules, grules...)
	fmt.Fprintf(out, "%-12s %s, %s, %s, %s (org %d, as %s)\n", "grafana",
		plural(len(in.Dashboards), "dashboard", "dashboards"), plural(queries, "panel query", "panel queries"),
		plural(vars, "variable", "variables"), plural(len(grules), "Grafana-managed rule", "Grafana-managed rules"), in.OrgID, login)

	// Runbooks: links in alert annotations, read from mirrors or fetched
	// (GET, no credentials) only when tvt.yaml allows it.
	in.Runbooks = rbReader.Read(ctx, runbook.Links(in.Rules))
	if len(in.Runbooks) > 0 {
		failed := 0
		for _, rb := range in.Runbooks {
			if rb.Err != "" {
				failed++
			}
		}
		fmt.Fprintf(out, "%-12s %s from alert annotations, %d unreadable\n", "runbooks",
			plural(len(in.Runbooks), "link", "links"), failed)
	}

	// Label-only selectors in rules and panels ({asserts_metric_request=
	// "total"}): ask the backend which metrics carry those labels, so they
	// don't protect every metric. A failure keeps the conservative reading.
	var exprs []string
	for _, r := range in.Rules {
		if len(r.Queries) > 0 { // Grafana-managed: one query per datasource
			for _, q := range r.Queries {
				if lang, _ := collect.QueryLanguage(q.DatasourceType); lang == "promql" {
					exprs = append(exprs, q.Expr)
				}
			}
		} else if r.Language == "promql" {
			exprs = append(exprs, r.Expr)
		}
	}
	for _, d := range in.Dashboards {
		for _, p := range d.Panels {
			for _, q := range p.Queries {
				if lang, _ := collect.QueryLanguage(q.DatasourceType); lang == "promql" {
					exprs = append(exprs, q.Expr)
				}
			}
		}
	}
	if sels := scan.LabelOnlySelectors(exprs); len(sels) > 0 {
		sm, err := pc.SelectorMetrics(ctx, sels)
		if err != nil {
			fmt.Fprintf(out, "%-12s %s not resolved (%v): they read every metric\n", "selectors", plural(len(sels), "label-only selector", "label-only selectors"), err)
		} else {
			in.SelectorMetrics = sm
			fmt.Fprintf(out, "%-12s %d of %s resolved to the metrics carrying their labels\n", "selectors", len(sm), plural(len(sels), "label-only selector", "label-only selectors"))
		}
	}

	// Usage evidence. Each source is recorded, configured or not, so the
	// report can say what the scan couldn't see.
	grafanaLog := graph.SourceCoverage{Source: sourceGrafanaLog}
	if o.cloud() {
		grafanaLog.Source = scan.SourceGrafanaUsageInsights
		ui := &grafana.UsageInsights{Client: gc, ExcludeUsers: []string{login}, OrgID: in.OrgID, Now: scanNow}
		if in.Views, err = ui.DashboardViews(ctx, since); err != nil {
			return err
		}
		if grafanaLog.Coverage, err = ui.Coverage(ctx, since); err != nil {
			return err
		}
		grafanaLog.Configured = true
		fmt.Fprintf(out, "%-12s %s, coverage %s\n", "usage ins.", plural(len(in.Views), "dashboard view", "dashboard views"), percent(grafanaLog.Coverage))
	} else if len(o.GrafanaLogs) > 0 {
		rl := &grafana.RouterLog{Paths: o.GrafanaLogs, ExcludeUsers: []string{login}, OrgID: in.OrgID, Now: scanNow}
		if in.Views, err = rl.DashboardViews(ctx, since); err != nil {
			return err
		}
		if grafanaLog.Coverage, err = rl.Coverage(ctx, since); err != nil {
			return err
		}
		grafanaLog.Configured = true
		var browser int
		for _, v := range in.Views {
			if v.Browser {
				browser++
			}
		}
		fmt.Fprintf(out, "%-12s %d dashboard loads (%d in a browser), coverage %s\n",
			"grafana log", len(in.Views), browser, percent(grafanaLog.Coverage))
	} else {
		fmt.Fprintf(out, "%-12s not configured (--grafana-log): no dashboard counts as viewed\n", "grafana log")
	}

	in.Sources = []graph.SourceCoverage{grafanaLog}
	if o.cloud() {
		// Grafana Cloud logs queries sent straight to Mimir and Loki
		// without their text, so their readers can't be attributed.
		in.Sources = append(in.Sources, graph.SourceCoverage{
			Source: scan.SourceCloudDirectQueries, Configured: true, Coverage: 0, WindowStart: since,
			Gap: "queries sent straight to Grafana Cloud's Mimir and Loki APIs (scripts, agents, other API clients) are logged without their text, so tvt can't see what they read",
		})
		fmt.Fprintf(out, "%-12s not readable: Grafana Cloud logs direct API queries without their text (listed as a gap)\n", "api queries")
	}
	for _, ql := range qlogs {
		src, queries, err := ql.read(ctx, since, nonce, o.ProbeWait, out)
		if err != nil {
			return err
		}
		src.WindowStart = since
		in.Sources = append(in.Sources, src)
		in.Queries = append(in.Queries, queries...)
		// A log that also records the ruler's own evaluations prices rules
		// (S6 item 1); a failure here only leaves rule costs unknown.
		if re, ok := ql.reader.(collect.RuleEvaluationSource); ok && ql.reader != nil {
			evals, err := re.RuleEvaluations(ctx, since)
			if err != nil {
				fmt.Fprintf(out, "%-12s rule evaluations unreadable (%v): rule costs unknown\n", ql.label, err)
			} else {
				in.RuleEvaluations = append(in.RuleEvaluations, evals...)
			}
		}
	}

	if in.History, err = history(ctx, o.DB); err != nil {
		return err
	}
	g, err := scan.Build(in)
	if err != nil {
		return err
	}
	if st := g.Storage; st != nil {
		fmt.Fprintf(out, "%-12s %.2f bytes/sample (%s), %s retention (%s)\n", "disk model",
			st.BytesPerSample, st.BytesPerSampleFrom, st.Retention.Round(time.Hour), st.RetentionFrom)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	classes := map[string]int{}
	for _, s := range g.Signals {
		classes[s.Class]++
	}
	fmt.Fprintf(out, "%-12s %d hard keep, %d keep_high (viewed), %d dormant, %d candidates; %d external consumers\n", "classes",
		classes[string(score.ClassHardKeep)], classes[string(score.ClassKeepHigh)], classes[string(score.ClassDormant)],
		classes[string(score.ClassCandidate)], len(in.External))

	fmt.Fprintf(out, "%-12s %s\n", "scores", scoreSummary(g.Scores, in.MinWindowDays))
	if len(g.RuleCosts) > 0 {
		fmt.Fprintf(out, "%-12s %s\n", "rules", ruleSummary(g.RuleCosts, g.RuleFindings))
	}
	if len(g.QueryCosts) > 0 {
		fmt.Fprintf(out, "%-12s %s\n", "query load", queryLoadSummary(g.Consumers, g.QueryCosts))
	}
	if len(in.Pipelines) > 0 {
		fmt.Fprintf(out, "%-12s %s\n", "pipelines", pipelineSummary(in.Pipelines, g.PipelineTargets))
	}
	fmt.Fprintf(out, "%-12s %s\n", "overflow", overflowSummary(in, g.OverflowFindings))
	if in.SLOsConfigured {
		fmt.Fprintf(out, "%-12s %s\n", "slo coverage", sloSummary(g.SLOFindings, len(in.Owners) > 0))
	}
	if in.Patterns != nil {
		fmt.Fprintf(out, "%-12s %s\n", "lines", lineSummary(g.LineFindings))
	}

	id, err := save(ctx, o.DB, start, o.Window, g)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "scan %d written to %s: %d signals, %d consumers, %d references, %d dashboard loads, log coverage %s\n",
		id, o.DB, len(g.Signals), len(g.Consumers), len(g.References), loads(g), percent(g.LogCoverage))
	return nil
}

// postmortemExts are the files a postmortem directory is searched for.
var postmortemExts = map[string]bool{".md": true, ".markdown": true, ".txt": true, ".html": true, ".htm": true}

// postmortems reads every postmortem file under paths (files or
// directories, walked recursively) and returns the names each mentions.
// They are local files the user listed: one tvt can't read fails the scan
// rather than leaving a signal it names unprotected.
func postmortems(paths []string) ([]scan.Mention, error) {
	var out []scan.Mention
	read := func(path string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("postmortem: %w", err)
		}
		ext := strings.ToLower(filepath.Ext(path))
		if names := parse.Mentions(string(b), ext == ".html" || ext == ".htm"); len(names) > 0 {
			out = append(out, scan.Mention{Source: "postmortem " + path, Names: names})
		}
		return nil
	}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("postmortems: %w", err)
		}
		if !fi.IsDir() {
			if err := read(p); err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !postmortemExts[strings.ToLower(filepath.Ext(path))] {
				return nil
			}
			return read(path)
		})
		if err != nil {
			return nil, fmt.Errorf("postmortems: %w", err)
		}
	}
	return out, nil
}

// history reads what earlier scans stored, for scoring: each signal's
// first sighting and the last reads query logs
// ever showed. A database that doesn't exist yet is a first scan; history
// never creates one.
func history(ctx context.Context, path string) (scan.History, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return scan.History{}, nil
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		return scan.History{}, err
	}
	defer st.Close()
	var h scan.History
	if h.FirstSeen, err = st.FirstSeen(ctx); err != nil {
		return scan.History{}, err
	}
	reads, err := st.LastReads(ctx)
	if err != nil {
		return scan.History{}, err
	}
	for _, r := range reads {
		h.LastReads = append(h.LastReads, scan.PastRead{SignalID: r.SignalID, At: r.At})
	}
	if h.LastViewed, err = st.LastViewed(ctx); err != nil {
		return scan.History{}, err
	}
	return h, nil
}

// patternWindow is how far back a scan reads Loki's line patterns: about
// what the pattern ingester keeps.
const patternWindow = 3 * time.Hour

// streamPatterns reads the line patterns of the scan.PatternStreams largest
// streams, one patterns call per stream (its full label set). Patterns are
// an input to line findings, not protection: a Loki without the pattern
// ingester, or a failed call, leaves the scan without line findings (nil)
// and says why.
func streamPatterns(ctx context.Context, lc *loki.Client, vols []collect.StreamVolume) (map[string][]collect.LinePattern, string) {
	top := slices.Clone(vols)
	sort.SliceStable(top, func(i, j int) bool { return top[i].Bytes > top[j].Bytes })
	top = top[:min(len(top), scan.PatternStreams)]
	// Ask for exactly each stream: its labels, and every other stream's
	// label names pinned to "", so a stream carrying more labels can't add
	// its lines (S5 debug pass).
	var names []string
	for _, v := range vols {
		for k := range v.Labels {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	out := map[string][]collect.LinePattern{}
	var lines int
	for _, v := range top {
		sel := loki.StreamSelector(v.Labels)
		ps, err := lc.Patterns(ctx, loki.ExactStreamSelector(v.Labels, names), patternWindow)
		switch {
		case errors.Is(err, loki.ErrNotFound):
			return nil, "unavailable: Loki's pattern API answered 404 (enable pattern_ingester for line findings)"
		case err != nil:
			return nil, fmt.Sprintf("unavailable: %v", err)
		}
		out[sel] = ps
		lines += len(ps)
	}
	return out, fmt.Sprintf("%s in the %d largest streams over the last %s", plural(lines, "pattern", "patterns"), len(top), patternWindow)
}

// lineSummary is the scan's one-line account of its line findings.
func lineSummary(fs []graph.LineFinding) string {
	if len(fs) == 0 {
		return "no line findings"
	}
	n := map[string]int{}
	for _, f := range fs {
		n[f.Kind]++
	}
	var parts []string
	if c := n[graph.LineRepeated]; c > 0 {
		parts = append(parts, plural(c, "repeated line", "repeated lines"))
	}
	if c := n[graph.LineHandoff]; c > 0 {
		parts = append(parts, plural(c, "line-level hand-off", "line-level hand-offs")+" to zeroreads")
	}
	return strings.Join(parts, ", ") + fmt.Sprintf("; top: %q in %s (%.0f%% of its bytes)", fs[0].Pattern, fs[0].Stream, fs[0].Share*100)
}

// scoreSummary is the scan's one-line account of its scores: how many
// candidates were ranked, how many confidence held back and why, and the
// top one.
func scoreSummary(scores []graph.Score, minWindow int) string {
	held := map[string]int{}
	for _, r := range scores {
		if r.Held != "" {
			held[r.Held]++
		}
	}
	line := fmt.Sprintf("%d candidates ranked", len(scores))
	if n := held[score.HeldWindow]; n > 0 {
		if minWindow <= 0 {
			minWindow = score.DefaultMinWindowDays
		}
		line += fmt.Sprintf(", none recommendable: the window is shorter than window_days (%d)", minWindow)
	}
	if n := held[score.HeldCoverage]; n > 0 {
		line += fmt.Sprintf(", %d held (no query log could show them read)", n)
	}
	if n := held[score.HeldNew]; n > 0 {
		line += fmt.Sprintf(", %d held (new this scan)", n)
	}
	if len(scores) > 0 && scores[0].Priority > 0 {
		top := scores[0]
		line += fmt.Sprintf("; top %s (priority %.2f, %s)", top.SignalID, top.Priority, top.Strongest)
	}
	return line
}

// scanBeforeWrite runs after the store is open; tests use it to fail a scan
// mid-write.
var scanBeforeWrite = func() error { return nil }

// save writes g as one scan and returns its ID. Open initializes a new
// database's schema in its own transaction, so if save created the file and
// the scan doesn't commit, it removes the file again: a failed scan never
// leaves a database behind. An existing database keeps its previous scans.
func save(ctx context.Context, path string, start time.Time, windowDays int, g scan.Output) (id int64, err error) {
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, fs.ErrNotExist)
	st, err := store.Open(ctx, path)
	if err != nil {
		if created {
			removeDB(path)
		}
		return 0, err
	}
	committed := false
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if created && !committed {
			removeDB(path)
		}
	}()
	if err := scanBeforeWrite(); err != nil {
		return 0, err
	}
	sc, err := st.BeginScan(ctx, start, windowDays)
	if err != nil {
		return 0, err
	}
	defer sc.Abort() // no-op after Finish
	if err := write(ctx, sc, g); err != nil {
		return 0, err
	}
	if err := sc.Finish(ctx, g.LogCoverage); err != nil {
		return 0, err
	}
	committed = true
	return sc.ID, nil
}

// removeDB deletes a SQLite database and its sidecar files.
func removeDB(path string) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(path + suffix)
	}
}

// loads is the number of dashboard loads; each graph row aggregates several.
func loads(g scan.Output) int64 {
	var n int64
	for _, l := range g.Loads {
		n += l.Loads
	}
	return n
}

func write(ctx context.Context, sc *store.Scan, g scan.Output) error {
	if p := g.PriceCard; p != nil {
		kind := p.Kind
		if kind == "" {
			kind = cost.KindVendor
		}
		if err := sc.SetPriceCard(ctx, p.Name, p.Currency, p.Preset, kind); err != nil {
			return err
		}
	}
	if g.TSDB != nil {
		if err := sc.SetTSDB(ctx, *g.TSDB); err != nil {
			return err
		}
	}
	for _, v := range g.StreamVolumes {
		if err := sc.PutStreamVolume(ctx, v); err != nil {
			return err
		}
	}
	for _, r := range g.Scores {
		if err := sc.PutScore(ctx, r); err != nil {
			return err
		}
	}
	for _, f := range g.LineFindings {
		if err := sc.PutLineFinding(ctx, f); err != nil {
			return err
		}
	}
	for _, c := range g.QueryCosts {
		if err := sc.PutQueryCost(ctx, c); err != nil {
			return err
		}
	}
	for _, f := range g.SLOFindings {
		if err := sc.PutSLOFinding(ctx, f); err != nil {
			return err
		}
	}
	for _, t := range g.PipelineTargets {
		if err := sc.PutPipelineTarget(ctx, t); err != nil {
			return err
		}
	}
	for _, f := range g.OverflowFindings {
		if err := sc.PutOverflowFinding(ctx, f); err != nil {
			return err
		}
	}
	for _, c := range g.RuleCosts {
		if err := sc.PutRuleCost(ctx, c); err != nil {
			return err
		}
	}
	for _, f := range g.RuleFindings {
		if err := sc.PutRuleFinding(ctx, f); err != nil {
			return err
		}
	}
	if g.VolumeTruncated {
		if err := sc.SetVolumeTruncated(ctx); err != nil {
			return err
		}
	}
	if st := g.Storage; st != nil {
		if err := sc.SetSelfHosted(ctx, store.SelfHosted{BytesPerSample: st.BytesPerSample, BytesPerSampleFrom: st.BytesPerSampleFrom,
			Retention: st.Retention, RetentionFrom: st.RetentionFrom, HeadBytesPerSeries: st.HeadBytesPerSeries}); err != nil {
			return err
		}
	}
	for _, s := range g.Signals {
		if err := sc.PutSignal(ctx, s); err != nil {
			return err
		}
	}
	for _, c := range g.Consumers {
		if err := sc.PutConsumer(ctx, c); err != nil {
			return err
		}
	}
	for _, r := range g.References {
		if err := sc.PutReference(ctx, r); err != nil {
			return err
		}
	}
	for _, c := range g.Costs {
		if err := sc.PutCost(ctx, c); err != nil {
			return err
		}
	}
	for _, s := range g.Sources {
		if err := sc.PutSource(ctx, s); err != nil {
			return err
		}
	}
	for _, l := range g.Loads {
		if err := sc.PutLoad(ctx, l); err != nil {
			return err
		}
	}
	return nil
}

// mimirAPI returns Mimir's Prometheus-compatible API root.
func mimirAPI(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	// Grafana Cloud's endpoint (/api/prom) is already the API root.
	if p := strings.TrimSuffix(u.Path, "/"); strings.HasSuffix(p, "/prometheus") || strings.HasSuffix(p, "/api/prom") {
		return base
	}
	return u.JoinPath("prometheus").String()
}

// percent formats a 0..1 share, never rounding a partial share to 0% or 100%.
func percent(f float64) string {
	switch {
	case f > 0 && f < 0.005:
		return "<1%"
	case f < 1 && f >= 0.995:
		return ">99%"
	}
	return fmt.Sprintf("%.0f%%", f*100)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (o scanOptions) validate() error {
	if o.Grafana == "" {
		return errors.New("--grafana is required")
	}
	if o.Prom == "" && o.Mimir == "" {
		return errors.New("one of --prom or --mimir is required")
	}
	if o.Prom != "" && o.Mimir != "" {
		return errors.New("--prom and --mimir are mutually exclusive")
	}
	for name, v := range map[string]string{"grafana": o.Grafana, "prom": o.Prom, "mimir": o.Mimir, "loki": o.Loki} {
		if v == "" {
			continue
		}
		if err := checkURL(v); err != nil {
			return fmt.Errorf("--%s: %w", name, err)
		}
	}
	if o.Window < 1 {
		return errors.New("--window must be at least 1 day")
	}
	if o.LabelTop < 0 {
		return errors.New("--label-top must not be negative")
	}
	if o.DB == "" {
		return errors.New("--db must not be empty")
	}
	if len(o.MimirQueryLogs) > 0 && o.Mimir == "" {
		return errors.New("--mimir-query-log needs --mimir")
	}
	if len(o.LokiQueryLogs) > 0 && o.Loki == "" {
		return errors.New("--loki-query-log needs --loki")
	}
	if o.ProbeWait < 0 {
		return errors.New("--probe-wait must not be negative")
	}
	switch o.GrafanaEdition {
	case "", "oss":
	case "cloud":
		if len(o.GrafanaLogs) > 0 {
			return errors.New("--grafana-log is for Grafana OSS; on Grafana Cloud, views come from Usage Insights")
		}
	default:
		return fmt.Errorf("--grafana-edition must be oss or cloud, not %q", o.GrafanaEdition)
	}
	return nil
}

// cloud reports whether the scan reads Grafana Cloud.
func (o scanOptions) cloud() bool { return o.GrafanaEdition == "cloud" }

// promToken and lokiToken are the backends' credentials: the token alone
// as a bearer token, or with TVT_PROM_USER / TVT_LOKI_USER (Grafana
// Cloud's instance IDs) as basic auth.
func promToken(getenv func(string) string) string {
	return httpx.BasicAuth(getenv("TVT_PROM_USER"), getenv("TVT_PROM_TOKEN"))
}

func lokiToken(getenv func(string) string) string {
	return httpx.BasicAuth(getenv("TVT_LOKI_USER"), getenv("TVT_LOKI_TOKEN"))
}

// checkTokenTransport refuses to send a bearer token over plain http to a
// host that is not loopback, where anyone on the path could read it.
func (o scanOptions) checkTokenTransport(getenv func(string) string) error {
	if o.AllowInsecureToken {
		return nil
	}
	backends := []struct{ flag, url, tokenEnv string }{
		{"grafana", o.Grafana, "TVT_GRAFANA_TOKEN"},
		{"prom", o.Prom, "TVT_PROM_TOKEN"},
		{"mimir", o.Mimir, "TVT_PROM_TOKEN"},
		{"loki", o.Loki, "TVT_LOKI_TOKEN"},
	}
	for _, b := range backends {
		if b.url == "" || getenv(b.tokenEnv) == "" {
			continue
		}
		u, err := url.Parse(b.url)
		if err != nil || u.Scheme != "http" || isLoopback(u.Hostname()) {
			continue
		}
		return fmt.Errorf("--%s: %s is set but %q is plain http: the token would be sent unencrypted; use https or pass --allow-insecure-token", b.flag, b.tokenEnv, b.url)
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q: missing host", raw)
	}
	return nil
}

// queryLog is one query log a scan reads, and how to prove it.
type queryLog struct {
	source, flag, label string
	reader              collect.QueryLogSource // nil: not configured
	probe               interface {            // nil: tvt can't probe this source
		Probe(ctx context.Context, nonce string) error
	}
	unprovable string // why there is no probe, when that is a gap
	probeErr   error
}

// queryLogs lists the query logs to record, configured or not: the metrics
// backend's (Prometheus or Mimir), a Prometheus log given with --mimir (the
// lab's Prometheus remote-writes to Mimir and still answers Grafana), and
// Loki's when the scan reads Loki (probed since S5 item 5).
func (o scanOptions) queryLogs(getenv func(string) string) []*queryLog {
	var out []*queryLog
	prom := &queryLog{source: sourcePromQueryLog, flag: "prom-query-log", label: "prom log"}
	if len(o.PromQueryLogs) > 0 {
		prom.reader = &prometheus.QueryLog{Paths: o.PromQueryLogs, Now: scanNow}
		if o.Prom != "" {
			c := prometheus.New(o.Prom, promToken(getenv))
			c.HTTP = scanHTTP
			prom.probe = c
		} else {
			prom.unprovable = "not proven: the scan reads Mimir, so tvt has no Prometheus URL to send its probe query to"
		}
	}
	if o.Mimir != "" {
		m := &queryLog{source: sourceMimirQueryLog, flag: "mimir-query-log", label: "mimir log"}
		if len(o.MimirQueryLogs) > 0 {
			m.reader = &mimir.QueryLog{Paths: o.MimirQueryLogs, Now: scanNow}
			c := prometheus.New(mimirAPI(o.Mimir), promToken(getenv))
			c.HTTP = scanHTTP
			m.probe = c
		}
		out = append(out, m)
		if prom.reader != nil {
			out = append(out, prom)
		}
	} else {
		out = append(out, prom)
	}
	if o.Loki != "" {
		l := &queryLog{source: sourceLokiQueryLog, flag: "loki-query-log", label: "loki log"}
		if len(o.LokiQueryLogs) > 0 {
			l.reader = &loki.QueryLog{Paths: o.LokiQueryLogs, Now: scanNow}
			c := loki.New(o.Loki, lokiToken(getenv))
			c.HTTP = scanHTTP
			l.probe = c
		}
		out = append(out, l)
	}
	return out
}

// read collects one query log's queries and coverage. A log tvt probed
// must show the marker within wait; one that doesn't, or that tvt couldn't
// probe, covers nothing and records the gap. Its queries still count:
// unknown means used.
func (ql *queryLog) read(ctx context.Context, since time.Time, nonce string, wait time.Duration, out io.Writer) (graph.SourceCoverage, []collect.LoggedQuery, error) {
	src := graph.SourceCoverage{Source: ql.source}
	if ql.reader == nil {
		fmt.Fprintf(out, "%-12s not configured (--%s)\n", ql.label, ql.flag)
		return src, nil, nil
	}
	src.Configured = true
	switch {
	case ql.probeErr != nil:
		src.Gap = "probe query failed: " + ql.probeErr.Error()
	case ql.probe != nil:
		seen, err := waitForProbe(ctx, ql.reader.(collect.ProbeSource), nonce, wait)
		if err != nil {
			return src, nil, err
		}
		if !seen {
			src.Gap = fmt.Sprintf("tvt's probe query never appeared in the log within %s: it isn't the live log, or the backend isn't writing it", wait)
		}
	default:
		src.Gap = ql.unprovable
	}
	// Read the log after the probe wait, not before: the backend may
	// flush buffered queries along with the marker, and a proven log
	// must include them.
	queries, err := ql.reader.Queries(ctx, since)
	if err != nil {
		return src, nil, err
	}
	if src.Coverage, err = ql.reader.Coverage(ctx, since); err != nil {
		return src, nil, err
	}
	proof := "probe found"
	switch {
	case src.Gap != "":
		src.Coverage = 0
		proof = "gap: " + src.Gap
	case ql.probe == nil:
		proof = "not probed"
	}
	fmt.Fprintf(out, "%-12s %s, coverage %s (%s)\n", ql.label, plural(len(queries), "query", "queries"), percent(src.Coverage), proof)
	return src, queries, nil
}

// waitForProbe re-reads the log until the marker for nonce shows up or
// wait has passed.
func waitForProbe(ctx context.Context, p collect.ProbeSource, nonce string, wait time.Duration) (bool, error) {
	deadline := time.Now().Add(wait)
	for {
		seen, err := p.ProbeSeen(ctx, nonce)
		if err != nil || seen {
			return seen, err
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(min(probePoll, time.Until(deadline))):
		}
	}
}

// humanBytes formats n in decimal units (1 MB = 1e6 bytes), as volume and
// price cards count them.
func humanBytes(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "kB", "MB", "GB", "TB"} {
		if f < 1000 || u == "TB" {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1000
	}
	return ""
}

// pipelineSummary is the scan's pipelines line (S6 item 4): signals per
// pipeline, unmapped signals and streams, rule outputs.
func overflowSummary(in scan.Inputs, findings []graph.OverflowFinding) string {
	switch {
	case in.OverflowErr != "":
		return "not read: " + in.OverflowErr
	case len(findings) == 0:
		return "none"
	}
	series, readers := 0, 0
	for _, f := range findings {
		series += f.Series
		readers += len(f.Readers)
	}
	return fmt.Sprintf("%d metrics with %d overflow series (cost is a floor), %d readers filter on capped labels", len(findings), series, readers)
}

func pipelineSummary(ps []config.Pipeline, targets []graph.PipelineTarget) string {
	per := map[string]int{}
	var unmapped, streams, rule int
	for _, t := range targets {
		switch {
		case strings.HasPrefix(t.Subject, "stream:"):
			if t.Pipeline == "" {
				streams++
			}
		case t.Note == scan.PipelineRuleOutput:
			rule++
		case t.Pipeline == "":
			unmapped++
		default:
			per[t.Pipeline]++
		}
	}
	var parts []string
	for _, p := range ps {
		parts = append(parts, fmt.Sprintf("%s %d", p.Name, per[p.Name]))
	}
	return fmt.Sprintf("%s signals; %d unmapped, %d recording-rule outputs; %d streams no pipeline claims",
		strings.Join(parts, ", "), unmapped, rule, streams)
}

// sloSummary is the scan's SLO coverage line (S6 item 3).
func sloSummary(findings []graph.SLOFinding, owners bool) string {
	n := map[string]int{}
	for _, f := range findings {
		n[f.Kind]++
	}
	services := fmt.Sprintf("%d services without an SLO", n[graph.SLOServiceWithout])
	if !owners {
		services = "services unknown (no ownership.owners)"
	}
	return fmt.Sprintf("%s, %d paging alerts not on burn rate, %d SLOs without an alert",
		services, n[graph.SLOPagingNotBurnRate], n[graph.SLOWithoutAlert])
}

// queryLoadSummary is the scan's query load line (S6 item 2): queries
// logged and the share of their samples and bytes each client drove.
func queryLoadSummary(consumers []graph.Consumer, costs []graph.QueryCost) string {
	client := map[string]string{}
	for _, c := range consumers {
		client[c.ID] = c.Client
	}
	var queries, samples, bytes float64
	bySamples, byBytes := map[string]float64{}, map[string]float64{}
	for _, c := range costs {
		queries, samples, bytes = queries+c.Queries, samples+c.Samples, bytes+c.Bytes
		bySamples[client[c.ConsumerID]] += c.Samples
		byBytes[client[c.ConsumerID]] += c.Bytes
	}
	shares := func(by map[string]float64, total float64) string {
		if total <= 0 {
			return "none logged"
		}
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(by)) {
			if by[k] > 0 {
				parts = append(parts, fmt.Sprintf("%s %.0f%%", k, 100*by[k]/total))
			}
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%.0f queries; samples: %s; bytes: %s", queries, shares(bySamples, samples), shares(byBytes, bytes))
}

// ruleSummary is the scan's rules line: how many rules were priced from
// logged evaluations, and the findings by kind.
func ruleSummary(costs []graph.RuleCost, findings []graph.RuleFinding) string {
	measured := 0
	for _, c := range costs {
		if c.Unknown == "" {
			measured++
		}
	}
	s := fmt.Sprintf("%d of %d priced from logged evaluations, %s", measured, len(costs), plural(len(findings), "finding", "findings"))
	if len(findings) == 0 {
		return s
	}
	kinds := map[string]int{}
	for _, f := range findings {
		kinds[f.Kind]++
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(kinds)) {
		parts = append(parts, fmt.Sprintf("%s %d", k, kinds[k]))
	}
	return s + " (" + strings.Join(parts, ", ") + ")"
}
