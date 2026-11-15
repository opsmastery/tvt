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

	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
)

type reportOptions struct {
	DB   string
	JSON string
	HTML string
	Top  int
}

func cmdReport(args []string, stdout, stderr io.Writer) int {
	var o reportOptions
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.DB, "db", "tvt.db", "path to the usage graph database (from tvt scan)")
	fs.StringVar(&o.JSON, "json", "report.json", "where to write the JSON report (- for stdout)")
	fs.StringVar(&o.HTML, "html", "report.html", "where to write the HTML report, rendered from the JSON (empty: skip)")
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
	in, err := loadReport(ctx, o.DB)
	if err != nil {
		return err
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
	fmt.Fprintf(out, "; %d evidence gaps; written to %s\n", len(r.Gaps), written)
	return nil
}

func loadReport(ctx context.Context, path string) (report.Input, error) {
	st, err := store.Open(ctx, path)
	if err != nil {
		return report.Input{}, err
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		return report.Input{}, err
	}
	in := report.Input{Scan: report.Scan{
		ID: info.ID, StartedAt: info.StartedAt, WindowDays: info.WindowDays, LogCoverage: info.LogCoverage,
		PriceCard: info.PriceCard, Currency: info.Currency, Approximate: info.PricePreset,
	}}
	if in.Signals, err = st.Signals(ctx, info.ID); err != nil {
		return in, err
	}
	if in.Consumers, err = st.Consumers(ctx, info.ID); err != nil {
		return in, err
	}
	if in.References, err = st.References(ctx, info.ID); err != nil {
		return in, err
	}
	if in.Costs, err = st.Costs(ctx, info.ID); err != nil {
		return in, err
	}
	if in.Sources, err = st.Sources(ctx, info.ID); err != nil {
		return in, err
	}
	if in.Scores, err = st.Scores(ctx, info.ID); err != nil {
		return in, err
	}
	reads, err := st.LastReads(ctx)
	if err != nil {
		return in, err
	}
	for _, r := range reads {
		in.LastReads = append(in.LastReads, report.LastReadOf{SignalID: r.SignalID,
			LastRead: report.LastRead{Consumer: r.ConsumerID, Actor: r.Actor, At: r.At}})
	}
	return in, nil
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
