package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

type whyOptions struct {
	DB     string
	Config string
	JSON   bool
	Signal string
}

func cmdWhy(args []string, stdout, stderr io.Writer) int {
	var o whyOptions
	fs := flag.NewFlagSet("why", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: tvt why [flags] <signal ID | metric | stream selector | metric{label}>")
		fs.PrintDefaults()
	}
	fs.StringVar(&o.DB, "db", "tvt.db", "path to the usage graph database (from tvt scan)")
	fs.StringVar(&o.Config, "config", "", "tvt.yaml: gaps.acknowledge entries (optional)")
	fs.BoolVar(&o.JSON, "json", false, "print JSON instead of text")
	// Flags may come before or after the signal.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		o.Signal = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return exitUsage
		}
	}
	if o.Signal == "" || fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}
	if err := runWhy(context.Background(), o, stdout); err != nil {
		fmt.Fprintf(stderr, "tvt why: %v\n", err)
		return exitError
	}
	return exitOK
}

// runWhy explains one signal from the latest scan in o.DB. Like report, it
// never creates a database.
func runWhy(ctx context.Context, o whyOptions, out io.Writer) error {
	if _, err := os.Stat(o.DB); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: no such database (run tvt scan first)", o.DB)
	}
	in, err := loadReport(ctx, o.DB)
	if err != nil {
		return err
	}
	if in.Acknowledged, err = acknowledgements(o.Config); err != nil {
		return err
	}
	id, err := report.Resolve(in.Signals, o.Signal)
	if err != nil {
		return err
	}
	w, err := report.Explain(in, id)
	if err != nil {
		return err
	}
	if o.JSON {
		b, err := json.MarshalIndent(w, "", "  ")
		if err != nil {
			return err
		}
		_, err = out.Write(append(b, '\n'))
		return err
	}
	return printWhy(out, w)
}

func printWhy(out io.Writer, w report.Why) error {
	s := w.Signal
	what := s.Kind + " " + s.Name
	if s.Parent != "" {
		what = "label " + s.Name + " of " + s.Parent
	}
	fmt.Fprintf(out, "%s\n  %s, in %s; scan %d (%s, %d-day window)\n", s.ID, what, s.Source, w.Scan.ID, day(w.Scan.StartedAt), w.Scan.WindowDays)
	line := "  class " + w.Class
	if w.Dormant != "" {
		line += " (" + w.Dormant + ")"
	}
	if w.Owner != "" {
		line += "; owner " + w.Owner
	}
	if w.Tier != nil {
		line += fmt.Sprintf(", tier %d", *w.Tier)
	}
	fmt.Fprintln(out, line)
	if w.Scan.Currency != "" {
		fmt.Fprintf(out, "  costs %.2f %s/month\n", w.Monthly, w.Scan.Currency)
	}
	if w.Score != nil {
		fmt.Fprintf(out, "  score: usage %.3g (%s), confidence %.3g, priority %.3g", w.Score.UsageWeight, w.Score.Strongest, w.Score.Confidence, w.Score.Priority)
		if w.Held != "" {
			fmt.Fprintf(out, "; held (%s)", w.Held)
		}
		fmt.Fprintln(out)
	}
	if w.LastRead != nil {
		fmt.Fprintf(out, "  last read %s by %s", day(w.LastRead.At), w.LastRead.Consumer)
		if w.LastRead.Actor != "" {
			fmt.Fprintf(out, " (%s)", w.LastRead.Actor)
		}
		fmt.Fprintln(out)
	} else {
		fmt.Fprintln(out, "  no logged read in any scan tvt kept")
	}
	if w.Row != "" {
		fmt.Fprintf(out, "  report row %s\n", w.Row)
	}

	fmt.Fprintf(out, "\nreaders (%d)\n", len(w.Readers))
	if len(w.Readers) == 0 {
		fmt.Fprintln(out, "  none")
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  EVIDENCE\tEXACT\tSOURCE\tREADER\tFIRST SEEN\tLAST SEEN")
	for _, r := range w.Readers {
		exact := "exact"
		if len(r.Inferred) > 0 {
			exact = "inferred"
		}
		name := r.Consumer
		if r.Name != "" && r.Name != r.Consumer {
			name += " (" + r.Name + ")"
		}
		if r.Actor != "" {
			name += " by " + r.Actor
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", r.Evidence, exact, r.Source, name, day(r.FirstSeen), day(r.LastSeen))
		var notes []string
		if len(r.Labels) > 0 {
			notes = append(notes, "labels "+strings.Join(r.Labels, ", "))
		}
		if r.AllLabels {
			notes = append(notes, "may depend on every label")
		}
		notes = append(notes, r.Inferred...)
		for _, n := range notes {
			fmt.Fprintf(tw, "  \t\t\t  %s\t\t\n", n)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(out, "\nevidence gaps (%d)\n", len(w.Gaps))
	for _, g := range w.Gaps {
		ack := ""
		if g.Acknowledged {
			ack = " (acknowledged: " + g.Reason + ")"
		}
		fmt.Fprintf(out, "  - [%s] %s%s\n", g.Kind, g.Text, ack)
	}
	return nil
}

// day formats a date, or "-" when unknown.
func day(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02")
}
