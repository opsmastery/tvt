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

	"github.com/Opsmastery-Summit/tvt/pkg/collect/grafana"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/loki"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/prometheus"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
)

// Evidence source names, as stored in scan_sources.
const (
	sourcePromQueryLog = "prometheus query log"
	sourceGrafanaLog   = "grafana router log"
)

// Test seams.
var (
	scanNow  = time.Now
	scanHTTP *http.Client // nil: each collector's default client
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
	// PromQueryLogs and GrafanaLogs are log files or globs on this machine.
	PromQueryLogs stringList
	GrafanaLogs   stringList
	// LabelTop is how many of the largest metrics get per-label cardinality.
	LabelTop int
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
	fs.Var(&o.GrafanaLogs, "grafana-log", "Grafana log file or glob with router logging on (repeatable; .gz is read)")
	fs.IntVar(&o.LabelTop, "label-top", 50, "collect per-label cardinality for this many of the largest metrics")
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

	// Metrics: cardinality, labels on the largest metrics, rules.
	pc := prometheus.New(o.Prom, getenv("TVT_PROM_TOKEN"))
	in.MetricsSource = "prom"
	if o.Mimir != "" {
		pc.BaseURL, pc.Source, in.MetricsSource = mimirAPI(o.Mimir), "mimir", "mimir"
	}
	pc.HTTP, pc.Now = scanHTTP, scanNow
	var err error
	if in.Metrics, err = pc.MetricCardinality(ctx); err != nil {
		return err
	}
	var series int64
	for _, m := range in.Metrics {
		series += m.ActiveSeries
	}
	top := min(o.LabelTop, len(in.Metrics)) // sorted largest first
	for _, m := range in.Metrics[:top] {
		ls, err := pc.LabelCardinality(ctx, m.Metric)
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
		vars += len(d.Variables)
	}
	fmt.Fprintf(out, "%-12s %s, %s, %s (org %d, as %s)\n", "grafana",
		plural(len(in.Dashboards), "dashboard", "dashboards"), plural(queries, "panel query", "panel queries"),
		plural(vars, "variable", "variables"), in.OrgID, login)

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

	queryLog := graph.SourceCoverage{Source: sourcePromQueryLog}
	if len(o.PromQueryLogs) > 0 {
		ql := &prometheus.QueryLog{Paths: o.PromQueryLogs, Now: scanNow}
		if queryLog.Coverage, err = ql.Coverage(ctx, since); err != nil {
			return err
		}
		queryLog.Configured = true
		fmt.Fprintf(out, "%-12s coverage %s (queries are matched from Sprint 2)\n", "query log", percent(queryLog.Coverage))
	} else {
		fmt.Fprintf(out, "%-12s not configured (--prom-query-log)\n", "query log")
	}
	in.Sources = []graph.SourceCoverage{grafanaLog, queryLog}

	g, err := scan.Build(in)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	id, err := save(ctx, o.DB, start, o.Window, g)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "scan %d written to %s: %d signals, %d consumers, %d dashboard loads, log coverage %s\n",
		id, o.DB, len(g.Signals), len(g.Consumers), loads(g), percent(g.LogCoverage))
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
