package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"go.yaml.in/yaml/v3"
)

type confirmOptions struct {
	Report string
	Dir    string
	Owner  string
	All    bool
	// Dismiss and Keep record the other decisions (S9 item 3).
	Dismiss string
	Keep    string
	Review  string
	// Seasonal: the owner says it's read only in these months (S9 item
	// 3): a keep until after the next season.
	Seasonal string
	Now      time.Time
}

// cmdConfirm records owner decisions in a pipeline repo's
// .tvt/decisions.yaml (S9 item 2): a confirm carries the evidence hash
// from the reviewed report, so tvt check can tell when the evidence has
// changed since.
func cmdConfirm(args []string, stdout, stderr io.Writer) int {
	o := confirmOptions{Now: time.Now()}
	fs := flag.NewFlagSet("confirm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: tvt confirm --report r.json --owner NAME [flags] [recommendation or signal]...

Records an owner's decision in the pipeline repo's .tvt/decisions.yaml,
for you to commit on the PR: confirm (the default), --dismiss REASON, or
--keep REASON. A confirm records the evidence hash the report shows; tvt
check refuses the merge once the evidence changes (a new reader, a gap
reopened). --all confirms every recommendation in the repo's
.tvt/proposal.json that --owner owns.

Before confirming, the owner answers one more question: is this signal
read only at certain times of year (a year-end report, a holiday peak)?
If so, record --seasonal MONTHS instead: a keep until after the next
season, when tvt asks again.`)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.Report, "report", "", "the report JSON the owner reviewed (tvt report --json)")
	fs.StringVar(&o.Dir, "dir", ".", "the pipeline repo's checkout (the PR's branch)")
	fs.StringVar(&o.Owner, "owner", "", "who decides (the owner the report names, or your handle)")
	fs.BoolVar(&o.All, "all", false, "confirm every recommendation in .tvt/proposal.json that --owner owns")
	fs.StringVar(&o.Dismiss, "dismiss", "", "dismiss instead, for this reason (tvt won't propose it again before --review)")
	fs.StringVar(&o.Keep, "keep", "", "keep instead, for this reason (for example: read at year end)")
	fs.StringVar(&o.Review, "review", "", "when to ask again, YYYY-MM-DD (dismiss and keep; default 90 days)")
	fs.StringVar(&o.Seasonal, "seasonal", "", "it's read only in these months (11,12 or nov,dec): keep it until after the next season")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if err := runConfirm(o, fs.Args(), stdout); err != nil {
		fmt.Fprintln(stderr, "tvt confirm:", err)
		return exitError
	}
	return exitOK
}

func runConfirm(o confirmOptions, ids []string, out io.Writer) error {
	if o.Report == "" || o.Owner == "" {
		return errors.New("--report and --owner are required")
	}
	n := 0
	for _, s := range []string{o.Dismiss, o.Keep, o.Seasonal} {
		if s != "" {
			n++
		}
	}
	if n > 1 {
		return errors.New("--dismiss, --keep and --seasonal: choose one")
	}
	action, reason := report.DecisionConfirm, ""
	review := ""
	switch {
	case o.Dismiss != "":
		action, reason = report.DecisionDismiss, o.Dismiss
	case o.Keep != "":
		action, reason = report.DecisionKeep, o.Keep
	case o.Seasonal != "":
		months, err := report.ParseMonths(o.Seasonal)
		if err != nil {
			return fmt.Errorf("--seasonal: %w", err)
		}
		if review, err = report.SeasonReview(months, o.Now); err != nil {
			return fmt.Errorf("--seasonal: %w", err)
		}
		action, reason = report.DecisionKeep, "seasonal: read only in "+o.Seasonal
	}
	if action != report.DecisionConfirm && review == "" {
		review = o.Review
		if review == "" {
			review = o.Now.AddDate(0, 0, 90).Format(time.DateOnly)
		}
		if _, err := time.Parse(time.DateOnly, review); err != nil {
			return fmt.Errorf("--review %q: want YYYY-MM-DD", review)
		}
	}
	b, err := os.ReadFile(o.Report)
	if err != nil {
		return err
	}
	var r report.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("%s: %w", o.Report, err)
	}
	if o.All {
		if action != report.DecisionConfirm {
			return errors.New("--all confirms; dismiss and keep name each signal")
		}
		pb, err := os.ReadFile(filepath.Join(o.Dir, filepath.FromSlash(report.ProposalPath)))
		if err != nil {
			return fmt.Errorf("--all needs the PR's %s: %w", report.ProposalPath, err)
		}
		var p report.Proposal
		if err := json.Unmarshal(pb, &p); err != nil {
			return fmt.Errorf("%s: %w", report.ProposalPath, err)
		}
		for _, cs := range p.Pipelines {
			for _, c := range cs {
				if c.Owner == o.Owner {
					ids = append(ids, c.Recommendation)
				}
			}
		}
		if len(ids) == 0 {
			return fmt.Errorf("%s has no recommendation owned by %q", report.ProposalPath, o.Owner)
		}
	}
	if len(ids) == 0 {
		return errors.New("name the recommendations or signals to decide on, or --all")
	}
	path := filepath.Join(o.Dir, filepath.FromSlash(report.DecisionsPath))
	var d report.Decisions
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &d); err != nil {
			return fmt.Errorf("%s: %w", report.DecisionsPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, id := range ids {
		rec, ok := findRec(r, id)
		if !ok {
			return fmt.Errorf("%s isn't a recommendation in %s (scan %d)", id, o.Report, r.Scan.ID)
		}
		ev := rec.EvidenceHash
		if ev == "" {
			ev = report.EvidenceHash(rec.Evidence)
		}
		if rec.Owner != "" && rec.Owner != o.Owner {
			fmt.Fprintf(out, "note: %s names %s as the owner of %s; recording %s's decision\n", o.Report, rec.Owner, rec.ID, o.Owner)
		}
		dec := report.Decision{Signal: rec.Signal.ID, Action: action, Owner: o.Owner, Reason: reason, Evidence: ev,
			At: o.Now.Format(time.DateOnly), Review: review, OwnerOfRecord: rec.Owner}
		// One decision per signal: a new one replaces the last.
		kept := d.Decisions[:0]
		for _, x := range d.Decisions {
			if x.Signal != dec.Signal {
				kept = append(kept, x)
			}
		}
		d.Decisions = append(kept, dec)
		fmt.Fprintf(out, "%s %s (evidence %s)\n", action, rec.ID, ev)
	}
	sort.SliceStable(d.Decisions, func(i, j int) bool { return d.Decisions[i].Signal < d.Decisions[j].Signal })
	body, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	head := "# Owner decisions (tvt S9). Written by tvt confirm; commit this file on the PR.\n# A confirm counts only while the recommendation's evidence still has this hash.\n"
	if err := os.WriteFile(path, append([]byte(head), body...), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s: commit it on the PR's branch\n", path)
	return nil
}

// findRec finds a recommendation by its ID, its signal's ID, or its
// signal's name.
func findRec(r report.Report, id string) (report.Recommendation, bool) {
	for _, rec := range r.Recommendations {
		if rec.ID == id || rec.Signal.ID == id {
			return rec, true
		}
	}
	var hit []report.Recommendation
	for _, rec := range r.Recommendations {
		if rec.Signal.Name == id || rec.Signal.Parent != "" && rec.Signal.Parent+"{"+rec.Signal.Name+"}" == id {
			hit = append(hit, rec)
		}
	}
	if len(hit) == 1 {
		return hit[0], true
	}
	// A dormant signal's question (confirm rows): keep or dismiss it.
	for _, c := range r.Confirm {
		if c.ID == id || c.Signal.ID == id || c.Signal.Name == id {
			return report.Recommendation{ID: c.ID, Signal: c.Signal, Owner: c.Owner, OwnerInferred: c.OwnerInferred, EvidenceHash: c.EvidenceHash}, true
		}
	}
	return report.Recommendation{}, false
}
