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
	"sort"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
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
	fs.IntVar(&o.Top, "top", 50, "recommendations to keep, highest savings per minute of checking first (0: all)")
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
	r := report.Build(in, o.Top)
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
	if in.RuleCosts, err = st.RuleCosts(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.RuleFindings, err = st.RuleFindings(ctx, info.ID); err != nil {
		return in, info, err
	}
	if in.LineFindings, err = st.LineFindings(ctx, info.ID); err != nil {
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
