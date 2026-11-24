package main

import (
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/internal/testutil"
	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/parse"
	"github.com/opsmastery/tvt/pkg/report"
	"github.com/opsmastery/tvt/pkg/scan"
	"github.com/opsmastery/tvt/pkg/store"
)

// TestPipelineTargets (S6 item 4, ADR 0007) scans the pipelines state and
// checks every target against ground truth's pipelines: each pipeline's
// example metrics and streams map to it, every metric of its jobs too, the
// reconciler stream is unmapped (a gap, in the report's unmapped streams,
// never proposable), and recording-rule outputs are notes, never unmapped.
func TestPipelineTargets(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "pipelines" {
			st = s
		}
	}
	if st.Name == "" {
		t.Fatal("no pipelines state in benchmark.yaml")
	}
	_, _, gt := scanState(t, st, dirs[st.Name])
	if len(gt.Pipelines) == 0 {
		t.Fatal("ground truth has no pipelines")
	}
	db := benchDB(t, st, dirs[st.Name])
	ctx := context.Background()
	s, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := s.PipelineTargets(ctx, info.ID)
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]graph.PipelineTarget{}
	for _, tg := range targets {
		by[tg.Subject] = tg
	}

	var unmapped gtPipeline
	var outputs []string
	pipelines := map[string]gtPipeline{}
	for name, node := range gt.Pipelines {
		switch name {
		case "unmapped":
			if err := node.Decode(&unmapped); err != nil {
				t.Fatal(err)
			}
		case "rule_outputs":
			if err := node.Decode(&outputs); err != nil {
				t.Fatal(err)
			}
			// The alerting rules' output, written by the ruler like a
			// recording rule's, though ground truth lists only those.
			outputs = append(outputs, "ALERTS", "ALERTS_FOR_STATE")
		default:
			var p gtPipeline
			if err := node.Decode(&p); err != nil {
				t.Fatal(err)
			}
			pipelines[name] = p
		}
	}

	expect := func(subject, pipeline string) {
		t.Helper()
		tg, ok := by[subject]
		switch {
		case !ok:
			t.Errorf("%s: no pipeline target", subject)
		case tg.Pipeline != pipeline:
			t.Errorf("%s: pipeline %q (%s), want %s", subject, tg.Pipeline, tg.Note, pipeline)
		}
	}
	streamSubject := func(sel string) string {
		refs, err := parse.LogQL(sel)
		if err != nil || len(refs) != 1 {
			t.Fatalf("selector %s: %v", sel, err)
		}
		return scan.StreamSubject(parse.CanonicalSelector(refs[0].Matchers))
	}

	// Jobs per metric, from the snapshot's label_metrics.json via the scan's
	// own lookup: every metric of a pipeline's jobs maps to it.
	jobOf := map[string]string{}
	for job, names := range readLabelMetrics(t, dirs[st.Name])["job"] {
		for _, m := range names {
			if prev, ok := jobOf[m]; ok && prev != job {
				jobOf[m] = "" // several jobs: checked by examples only
				continue
			}
			jobOf[m] = job
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pipelines)) {
		p := pipelines[name]
		if len(p.Examples) == 0 || len(p.Jobs) == 0 {
			t.Errorf("ground truth pipeline %s lists no examples or jobs", name)
		}
		for _, m := range p.Examples {
			expect("prom:metric:"+m, name)
		}
		n := 0
		for m, job := range jobOf {
			if slices.Contains(p.Jobs, job) && !slices.Contains(outputs, m) {
				if _, ok := by["prom:metric:"+m]; ok {
					expect("prom:metric:"+m, name)
					n++
				}
			}
		}
		for _, sel := range p.Streams {
			// Every listed stream the selector selects maps to the pipeline.
			refs, _ := parse.LogQL(sel)
			for _, tg := range targets {
				ls, ok := subjectLabels(tg.Subject)
				if !ok {
					continue
				}
				if hit, _ := parse.MatchesLabels(refs[0].Matchers, ls); hit {
					expect(tg.Subject, name)
					n++
				}
			}
		}
		t.Logf("%s: %d signals and streams checked", name, n)
	}

	// The unmapped stream: a target with a note, a gap line, and a report
	// entry; never mapped.
	if len(unmapped.Streams) == 0 {
		t.Fatal("ground truth lists no unmapped stream")
	}
	for _, sel := range unmapped.Streams {
		sub := streamSubject(sel)
		tg, ok := by[sub]
		if !ok || tg.Pipeline != "" || tg.Note == "" {
			t.Errorf("%s: %+v (found %v), want unmapped with a note", sub, tg, ok)
		}
	}
	// Every stream target other than the unmapped ones maps somewhere.
	for _, tg := range targets {
		if !strings.HasPrefix(tg.Subject, "stream:") || tg.Pipeline != "" {
			continue
		}
		if !slices.ContainsFunc(unmapped.Streams, func(sel string) bool { return streamSubject(sel) == tg.Subject }) {
			t.Errorf("%s: unmapped (%s), ground truth maps it", tg.Subject, tg.Note)
		}
	}

	// Recording-rule outputs: a note, never a pipeline, never unmapped.
	for _, m := range outputs {
		tg, ok := by["prom:metric:"+m]
		if !ok {
			continue // not collected in this snapshot
		}
		if tg.Pipeline != "" || tg.Note != scan.PipelineRuleOutput {
			t.Errorf("rule output %s: %+v", m, tg)
		}
	}
	// Every other metric: one pipeline when all its jobs are one pipeline's,
	// "claimed by several" when its jobs span pipelines (up, go_*: ADR
	// 0007, no single change can drop them), no match when it carries no
	// job (a rule's reference to a metric that doesn't exist).
	pipelineOf := map[string]string{}
	for name, p := range pipelines {
		for _, j := range p.Jobs {
			pipelineOf[j] = name
		}
	}
	jobsOf := map[string][]string{}
	for job, names := range readLabelMetrics(t, dirs[st.Name])["job"] {
		for _, m := range names {
			jobsOf[m] = append(jobsOf[m], job)
		}
	}
	var several int
	for _, tg := range targets {
		m, ok := strings.CutPrefix(tg.Subject, "prom:metric:")
		if !ok || slices.Contains(outputs, m) {
			continue
		}
		owners := map[string]bool{}
		for _, j := range jobsOf[m] {
			owners[pipelineOf[j]] = true
		}
		switch len(owners) {
		case 0:
			if tg.Pipeline != "" || tg.Note != scan.PipelineNoMatch {
				t.Errorf("%s (no job): %+v, want no match", tg.Subject, tg)
			}
		case 1:
			want := slices.Collect(maps.Keys(owners))[0]
			if want == "" || tg.Pipeline != want {
				t.Errorf("%s (jobs %v): %+v, want %q", tg.Subject, jobsOf[m], tg, want)
			}
		default:
			several++
			if tg.Pipeline != "" || !strings.HasPrefix(tg.Note, "claimed by several pipelines") {
				t.Errorf("%s (jobs %v): %+v, want claimed by several", tg.Subject, jobsOf[m], tg)
			}
		}
	}
	if several == 0 {
		t.Error("no metric spans pipelines: up and go_* come from jobs on both")
	}
	t.Logf("%d metrics span pipelines", several)

	// The report: rows carry their pipeline, the unmapped stream is listed
	// with its bytes, nothing unmapped is proposable.
	in, _, err := loadReport(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	r := report.Build(in, 50)
	for _, rec := range r.Recommendations {
		if rec.Pipeline == "" || (rec.Pipeline == report.Unmapped) == rec.Proposable {
			t.Errorf("%s: pipeline %q, proposable %v", rec.ID, rec.Pipeline, rec.Proposable)
		}
	}
	var got []string
	for _, u := range r.Pipelines.UnmappedStreams {
		got = append(got, scan.StreamSubject(u.Selector))
		if u.BytesPerDay <= 0 || u.Note == "" {
			t.Errorf("unmapped stream %s: %+v", u.Selector, u)
		}
	}
	var want []string
	for _, sel := range unmapped.Streams {
		want = append(want, streamSubject(sel))
	}
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("report unmapped streams %v, want %v", got, want)
	}
	if len(r.Pipelines.Pipelines) != len(pipelines) {
		t.Errorf("report pipelines %+v, want %d", r.Pipelines.Pipelines, len(pipelines))
	}
	var src graph.SourceCoverage
	for _, sc := range in.Sources {
		if sc.Source == scan.SourcePipelines {
			src = sc
		}
	}
	if !src.Configured || !strings.Contains(src.Gap, "reconciler") {
		t.Errorf("pipelines source %+v: want configured with the reconciler stream's gap", src)
	}
	t.Logf("pipelines source: coverage %.4f, gap %q", src.Coverage, src.Gap)
}

// readLabelMetrics reads the snapshot's recorded label lookup
// (prometheus/label_metrics.json: label -> value -> metric names).
func readLabelMetrics(t *testing.T, dir string) map[string]map[string][]string {
	t.Helper()
	var lm map[string]map[string][]string
	if err := json.Unmarshal(testutil.ReadFile(t, filepath.Join(dir, "prometheus/label_metrics.json")), &lm); err != nil {
		t.Fatal(err)
	}
	return lm
}

// subjectLabels parses a stream subject's canonical selector into labels.
func subjectLabels(subject string) (map[string]string, bool) {
	sel, ok := strings.CutPrefix(subject, "stream:")
	if !ok {
		return nil, false
	}
	refs, err := parse.LogQL(sel)
	if err != nil || len(refs) != 1 {
		return nil, false
	}
	ls := map[string]string{}
	for _, m := range refs[0].Matchers {
		ls[m.Label] = m.Value
	}
	return ls, true
}
