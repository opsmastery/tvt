package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/collect/prometheus"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/propose"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// replayTolerance is how far a value may move without a note (1%).
const replayTolerance = 0.01

// replayer runs protected queries before a label removal (S8 item 4).
type replayer interface {
	Replay(ctx context.Context, query string) ([]prometheus.Sample, error)
}

// replayLabelRemovals replays, for each label removal, every protected
// query on its metric (alerts, recording rules, SLOs, runbook steps and
// viewed dashboard panels whose text tvt has), as written and without the
// label. A query that goes empty or changes its grouping blocks the
// removal: it's returned as skipped, and nothing for it is written. The
// rest come back with what the replay found, for the patch and PR.
// lines are by pipeline.
func replayLabelRemovals(ctx context.Context, in report.Input, recs []report.Recommendation, rp replayer) (keep []report.Recommendation, skipped []proposal, lines map[string][]string) {
	lines = map[string][]string{}
	exprs := map[string]graph.Consumer{}
	for _, c := range in.Consumers {
		exprs[c.ID] = c
	}
	for _, rec := range recs {
		if rec.Action != report.ActionLabelRemoval || rec.Signal.Source != "prom" {
			keep = append(keep, rec)
			continue
		}
		metric, label := rec.Signal.Parent, rec.Signal.Name
		w, err := report.Explain(in, "prom:metric:"+metric)
		if err != nil {
			keep = append(keep, rec)
			continue
		}
		var blocked []string
		var notes []string
		ran := 0
		seen := map[string]bool{}
		for _, rd := range w.Readers {
			c, ok := exprs[rd.Consumer]
			if !ok || rd.Sweep || seen[c.ID] || c.Expr == "" || !protectiveKind(c.Kind) {
				continue
			}
			seen[c.ID] = true
			asWritten, without, ok, err := propose.ReplayQueries(c.Expr, metric, label)
			if err != nil || !ok {
				continue
			}
			a, errA := rp.Replay(ctx, asWritten)
			b, errB := rp.Replay(ctx, without)
			if errA != nil || errB != nil {
				blocked = append(blocked, fmt.Sprintf("%s: the replay failed (%v)", c.Name, firstErr(errA, errB)))
				continue
			}
			ran++
			d := propose.CompareReplay(samples(a), samples(b), label, replayTolerance, propose.Gauge(metric))
			if d.Blocked != "" {
				blocked = append(blocked, c.Name+": "+d.Blocked)
			}
			for _, n := range d.Notes {
				notes = append(notes, c.Name+": "+n)
			}
		}
		if len(blocked) > 0 {
			sort.Strings(blocked)
			skipped = append(skipped, proposal{Rec: rec, Note: "replay blocks it until the owner accepts (S8 item 4): " + strings.Join(blocked, "; ")})
			continue
		}
		keep = append(keep, rec)
		line := fmt.Sprintf("%s: %d protected queries replayed without %s, results unchanged", rec.ID, ran, label)
		if len(notes) > 0 {
			line = fmt.Sprintf("%s: %d protected queries replayed without %s; %s", rec.ID, ran, label, strings.Join(notes, "; "))
		}
		lines[rec.Pipeline] = append(lines[rec.Pipeline], line)
	}
	return keep, skipped, lines
}

func protectiveKind(k graph.ConsumerKind) bool {
	switch k {
	case graph.ConsumerAlertRule, graph.ConsumerRecordingRule, graph.ConsumerSLO, graph.ConsumerRunbook, graph.ConsumerDashboardPanel:
		return true
	}
	return false
}

func samples(in []prometheus.Sample) []propose.ReplaySample {
	out := make([]propose.ReplaySample, len(in))
	for i, s := range in {
		out[i] = propose.ReplaySample{Labels: s.Labels, Value: s.Value}
	}
	return out
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// newReplayer is the Prometheus (or Mimir's Prometheus API) to replay
// against; TVT_PROM_TOKEN authenticates, as for tvt scan.
func newReplayer(url string) replayer {
	return prometheus.New(url, os.Getenv("TVT_PROM_TOKEN"))
}
