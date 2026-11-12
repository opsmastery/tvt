package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/grafana"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/loki"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/mimir"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/prometheus"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
)

// Evidence source names, as stored in scan_sources.
const (
	sourcePromQueryLog  = "prometheus query log"
	sourceMimirQueryLog = "mimir query log"
	sourceLokiQueryLog  = "loki query log"
	sourceGrafanaLog    = "grafana router log"
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
	fs.StringVar(&o.Loki, "loki", "", "Loki base URL: rules are collected; stream volume from Sprint 3")
	fs.StringVar(&o.DB, "db", "tvt.db", "path to the usage graph database")
	fs.IntVar(&o.Window, "window", 30, "observation window in days")
	fs.Var(&o.PromQueryLogs, "prom-query-log", "Prometheus query log file or glob (repeatable; .gz is read)")
	fs.Var(&o.MimirQueryLogs, "mimir-query-log", "Mimir log file or glob with query-frontend query stats lines (repeatable; .gz is read)")
	fs.Var(&o.LokiQueryLogs, "loki-query-log", "Loki log file or glob with metrics.go query lines (repeatable; .gz is read)")
	fs.DurationVar(&o.ProbeWait, "probe-wait", 30*time.Second, "how long to wait for tvt's probe query to appear in each query log")
	fs.Var(&o.GrafanaLogs, "grafana-log", "Grafana log file or glob with router logging on (repeatable; .gz is read)")
	fs.IntVar(&o.LabelTop, "label-top", 50, "collect per-label cardinality for this many of the largest metrics")
	fs.StringVar(&o.Config, "config", "", "tvt.yaml with external_consumers and ownership (optional)")
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
	// backend, rather than silently dropping an external consumer.
	if o.Config != "" {
		cfg, err := config.Load(o.Config)
		if err != nil {
			return err
		}
		in.External, in.Owners = cfg.ExternalConsumers, cfg.Ownership.Owners
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
	pc := prometheus.New(o.Prom, getenv("TVT_PROM_TOKEN"))
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
	fmt.Fprintf(out, "%-12s %d metrics (%d series), %d labels on the top %d, %d rules\n",
		in.MetricsSource, len(in.Metrics), series, len(in.Labels), top, len(in.Rules))

	// Loki: rules. Each is a consumer with an assumed reference to the
	// streams its selectors name, until the LogQL extractor exists.
	if o.Loki != "" {
		lc := loki.New(o.Loki, getenv("TVT_LOKI_TOKEN"))
		lc.HTTP, lc.Now = scanHTTP, scanNow
		rules, err := lc.Rules(ctx)
		if err != nil {
			return err
		}
		in.Rules = append(in.Rules, rules...)
		fmt.Fprintf(out, "%-12s %s (stream volume is collected from Sprint 3)\n", "loki", plural(len(rules), "rule", "rules"))
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

	// Usage evidence. Each source is recorded, configured or not, so the
	// report can say what the scan couldn't see.
	grafanaLog := graph.SourceCoverage{Source: sourceGrafanaLog}
	if len(o.GrafanaLogs) > 0 {
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
	for _, ql := range qlogs {
		src, queries, err := ql.read(ctx, since, nonce, o.ProbeWait, out)
		if err != nil {
			return err
		}
		src.WindowStart = since
		in.Sources = append(in.Sources, src)
		in.Queries = append(in.Queries, queries...)
	}

	g, err := scan.Build(in)
	if err != nil {
		return err
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

	id, err := save(ctx, o.DB, start, o.Window, g)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "scan %d written to %s: %d signals, %d consumers, %d references, %d dashboard loads, log coverage %s\n",
		id, o.DB, len(g.Signals), len(g.Consumers), len(g.References), loads(g), percent(g.LogCoverage))
	return nil
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
	if err != nil || strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/prometheus") {
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
	return nil
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
// Loki's when the scan reads Loki. Loki's probe is S5 item 5 (docs/tasks/s05-benchmark-and-log-findings.md).
func (o scanOptions) queryLogs(getenv func(string) string) []*queryLog {
	var out []*queryLog
	prom := &queryLog{source: sourcePromQueryLog, flag: "prom-query-log", label: "prom log"}
	if len(o.PromQueryLogs) > 0 {
		prom.reader = &prometheus.QueryLog{Paths: o.PromQueryLogs, Now: scanNow}
		if o.Prom != "" {
			c := prometheus.New(o.Prom, getenv("TVT_PROM_TOKEN"))
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
			c := prometheus.New(mimirAPI(o.Mimir), getenv("TVT_PROM_TOKEN"))
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
