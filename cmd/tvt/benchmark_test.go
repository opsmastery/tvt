package main

// The tvt benchmark (S5 item 1): tvt-lab's ground truth, scored against
// frozen fixture snapshots of each lab state (testdata/bench). Two scores,
// kept separate so a gain in one finding class can't hide a loss in
// another:
//
//   - evidence: of the readers, non-readers and query-log sources ground
//     truth expects, the share the scan resolved;
//   - recommendations: precision and recall of what the report raises (a
//     recommendation, or a dormant "confirm" question) against what ground
//     truth says it should raise.
//
// testdata/bench/baseline.json holds the recorded scores. Any score below
// its baseline fails. After a deliberate change (more ground truth, a new
// state, a real improvement), re-record it and commit the diff:
//
//	go test ./cmd/tvt -run TestBenchmark -update-bench

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

var updateBench = flag.Bool("update-bench", false, "re-record testdata/bench/baseline.json from this run")

// benchmark is testdata/bench/benchmark.yaml.
type benchmark struct {
	Version int          `yaml:"version"`
	States  []benchState `yaml:"states"`
}

type benchState struct {
	Name string `yaml:"name"`
	// Dir holds the state's fixtures, laid out like testdata/lab. With
	// Base, Dir is an overlay: its files replace the base state's.
	Dir      string    `yaml:"dir"`
	Base     string    `yaml:"base"`
	ScanTime time.Time `yaml:"scan_time"`
	// Proven: the query-log sources (report source names) whose logs the
	// snapshot holds, so the scan must prove them.
	Proven []string `yaml:"proven"`
	About  string   `yaml:"about"`
}

// benchScore is one state's result, as recorded in baseline.json.
type benchScore struct {
	Evidence        evidenceScore `json:"evidence"`
	Recommendations recScore      `json:"recommendations"`
}

type evidenceScore struct {
	Found  int      `json:"found"`
	Total  int      `json:"total"`
	Score  float64  `json:"score"`
	Missed []string `json:"missed"`
}

type recScore struct {
	TruePositives  int      `json:"true_positives"`
	FalsePositives int      `json:"false_positives"`
	FalseNegatives int      `json:"false_negatives"`
	Precision      float64  `json:"precision"`
	Recall         float64  `json:"recall"`
	Wrong          []string `json:"wrong"`  // raised, or raised as the wrong kind
	Missed         []string `json:"missed"` // should have been raised
}

type benchBaseline struct {
	Version int                   `json:"version"`
	States  map[string]benchScore `json:"states"`
}

// loadBenchmark reads benchmark.yaml and returns its states with the
// directory each one's fixtures are in (an overlay state's laid over its
// base in a temporary directory).
func loadBenchmark(t *testing.T) (benchmark, map[string]string) {
	t.Helper()
	var bm benchmark
	if err := yaml.Unmarshal(testutil.ReadFile(t, "bench/benchmark.yaml"), &bm); err != nil {
		t.Fatal(err)
	}
	if bm.Version == 0 || len(bm.States) == 0 {
		t.Fatalf("benchmark.yaml: no version or states: %+v", bm)
	}
	dirs := map[string]string{}
	for _, st := range bm.States {
		dir := filepath.Join(testutil.Testdata, "bench", st.Dir)
		if st.Base != "" {
			base, ok := dirs[st.Base]
			if !ok {
				t.Fatalf("state %s: base %s is not an earlier state", st.Name, st.Base)
			}
			dir = overlay(t, base, dir)
		}
		dirs[st.Name] = dir
	}
	return bm, dirs
}

func TestBenchmark(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	got := benchBaseline{Version: bm.Version, States: map[string]benchScore{}}
	for _, st := range bm.States {
		t.Run(st.Name, func(t *testing.T) {
			got.States[st.Name] = scoreState(t, st, dirs[st.Name])
		})
	}
	if t.Failed() {
		return
	}

	file := filepath.Join(testutil.Testdata, "bench", "baseline.json")
	if *updateBench {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %s", file)
		return
	}
	var base benchBaseline
	if err := json.Unmarshal(testutil.ReadFile(t, "bench/baseline.json"), &base); err != nil {
		t.Fatalf("baseline.json: %v (record one with -update-bench)", err)
	}
	if base.Version != bm.Version {
		t.Fatalf("baseline.json is for benchmark version %d, benchmark.yaml is version %d: re-record with -update-bench", base.Version, bm.Version)
	}
	for _, st := range bm.States {
		b, ok := base.States[st.Name]
		if !ok {
			t.Errorf("state %s has no baseline: re-record with -update-bench", st.Name)
			continue
		}
		compareScores(t, st.Name, b, got.States[st.Name])
	}
}

// compareScores fails on any score below its baseline, naming what the
// baseline resolved that this run doesn't, and logs improvements.
func compareScores(t *testing.T, state string, base, now benchScore) {
	t.Helper()
	const eps = 1e-9
	check := func(what string, b, n float64, baseMissed, nowMissed []string) {
		t.Helper()
		switch {
		case n < b-eps:
			t.Errorf("%s: %s fell from %.4f to %.4f; newly wrong or missed: %s",
				state, what, b, n, strings.Join(newIn(baseMissed, nowMissed), "; "))
		case n > b+eps:
			t.Logf("%s: %s rose from %.4f to %.4f: re-record the baseline with -update-bench", state, what, b, n)
		}
	}
	check("evidence found", base.Evidence.Score, now.Evidence.Score, base.Evidence.Missed, now.Evidence.Missed)
	check("recommendation precision", base.Recommendations.Precision, now.Recommendations.Precision, base.Recommendations.Wrong, now.Recommendations.Wrong)
	check("recommendation recall", base.Recommendations.Recall, now.Recommendations.Recall, base.Recommendations.Missed, now.Recommendations.Missed)
}

// newIn lists the entries of now that aren't in base.
func newIn(base, now []string) []string {
	had := map[string]bool{}
	for _, s := range base {
		had[s] = true
	}
	var out []string
	for _, s := range now {
		if !had[s] {
			out = append(out, s)
		}
	}
	return out
}

// overlay copies base into a temporary directory, then dir's files over it.
func overlay(t *testing.T, base, dir string) string {
	t.Helper()
	out := t.TempDir()
	for _, src := range []string{base, dir} {
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			dst := filepath.Join(out, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			return os.WriteFile(dst, b, 0o644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// scoreState scans one frozen state, builds its report and scores both.
func scoreState(t *testing.T, st benchState, dir string) benchScore {
	t.Helper()
	g, r, gt := scanState(t, st, dir)
	s := benchScore{
		Evidence:        scoreEvidence(t, g, gt, r, dir, st.Proven),
		Recommendations: scoreRecommendations(t, g, gt, r),
	}
	t.Logf("evidence %d/%d = %.4f; recommendations: precision %.4f, recall %.4f (tp %d, fp %d, fn %d)",
		s.Evidence.Found, s.Evidence.Total, s.Evidence.Score,
		s.Recommendations.Precision, s.Recommendations.Recall,
		s.Recommendations.TruePositives, s.Recommendations.FalsePositives, s.Recommendations.FalseNegatives)
	for _, m := range s.Evidence.Missed {
		t.Logf("  evidence missed: %s", m)
	}
	for _, m := range s.Recommendations.Wrong {
		t.Logf("  wrongly raised: %s", m)
	}
	for _, m := range s.Recommendations.Missed {
		t.Logf("  not raised: %s", m)
	}
	return s
}

// scanState scans one frozen state with the clock at its scan time and
// returns the stored graph, the full report and the state's ground truth.
func scanState(t *testing.T, st benchState, dir string) (scanGraph, report.Report, groundTruth) {
	t.Helper()
	setNowAt(t, st.ScanTime)
	// The capture asked for 200 series (limit=200), so Prometheus marked the
	// answer truncated, which tvt refuses (fail closed). Served as the
	// whole answer, without that warning: a 200-series sample of the label
	// values is enough to make user_id a label signal.
	var recorded map[string]any
	if err := json.Unmarshal(testutil.ReadFile(t, filepath.Join(dir, "prometheus/series_cart_ops.json")), &recorded); err != nil {
		t.Fatal(err)
	}
	delete(recorded, "warnings")
	series, err := json.Marshal(recorded)
	if err != nil {
		t.Fatal(err)
	}
	l := newLabAt(t, dir, map[string]http.HandlerFunc{
		// The recorded series of shop_cart_operations_total, so its user_id
		// label is a signal; two synthetic series for every other metric.
		"/api/v1/series": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("match[]") == `{__name__="shop_cart_operations_total"}` {
				w.Write(series)
				return
			}
			syntheticSeries(w, r)
		},
	})
	db := filepath.Join(t.TempDir(), "tvt.db")
	o := labOptionsAt(dir, l, db)
	o.LabelTop = 5
	o.LokiQueryLogs = stringList{l.lokiLog}
	var out strings.Builder
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	in, _, err := loadReport(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	in.Now = st.ScanTime
	in.LabelDrops = true // the benchmark scores label removals (S6 item 7)
	r := report.Build(in, 0)

	gt := parseGroundTruth(t, testutil.ReadFile(t, filepath.Join(dir, "ground-truth.yaml")))
	return readGraph(t, db), r, gt
}

// tally collects pass/fail items.
type tally struct {
	found, total int
	missed       []string
}

func (y *tally) item(ok bool, name string) {
	y.total++
	if ok {
		y.found++
	} else {
		y.missed = append(y.missed, name)
	}
}

// readers summarizes a signal's references (sweeps left out, as ground
// truth never counts them).
type readers struct{ hard, viewed, dash, adhoc, any bool }

func readersOf(g scanGraph, refs []graph.Reference) readers {
	var r readers
	for _, ref := range refs {
		c := g.consumers[ref.ConsumerID]
		r.any = true
		r.hard = r.hard || c.Kind.HardKeep()
		r.dash = r.dash || isDashboard(c.Kind)
		r.viewed = r.viewed || isDashboard(c.Kind) && !c.LastViewed.IsZero()
		r.adhoc = r.adhoc || (c.Kind == graph.ConsumerAdhocQuery || c.Kind == graph.ConsumerAgentQuery) && !ref.LastSeen.IsZero()
	}
	return r
}

// expectedReaders: does r show what a ground-truth class needs? ok is false
// for a class that says nothing about readers.
func expectedReaders(class string, r readers) (match, ok bool) {
	switch class {
	case "hard_keep":
		return r.hard, true
	case "keep_high":
		return r.viewed, true
	case "keep_medium":
		return r.adhoc && !r.hard && !r.dash, true
	case "low":
		return r.dash && !r.viewed && !r.hard, true
	case "drop_metric", "dormant_confirm", "route_cold", "drop_label":
		return !r.hard && !r.dash && !r.adhoc, true
	}
	return false, false
}

// streamRefs are the references whose stream selector selects a stream
// with labels.
func streamRefs(t *testing.T, g scanGraph, labels map[string]string) []graph.Reference {
	var out []graph.Reference
	for _, ref := range g.refs {
		if s := g.signals[ref.SignalID]; s.Kind == graph.KindStream && !ref.Sweep && selectorMatches(t, s.Name, labels) {
			out = append(out, ref)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// scoreEvidence: every reader (and every non-reader) ground truth names
// resolved, every rule and dashboard found with its viewed state, every
// declared dependency read, and every query-log source the snapshot holds
// proven.
func scoreEvidence(t *testing.T, g scanGraph, gt groundTruth, r report.Report, dir string, proven []string) evidenceScore {
	t.Helper()
	var y tally
	for _, name := range sortedKeys(gt.Metrics) {
		if strings.Contains(name, "*") {
			continue
		}
		e := gt.Metrics[name]
		if _, ok := g.signals["prom:metric:"+name]; !ok {
			y.item(false, "metric "+name+": no signal")
			continue
		}
		match, ok := expectedReaders(e.Class, readersOf(g, g.refsTo("prom:metric:"+name)))
		if ok {
			y.item(match, "metric "+name+" ("+e.Class+")")
		}
	}
	for _, key := range sortedKeys(gt.Labels) {
		e := gt.Labels[key]
		metric, label, _ := strings.Cut(key, "/")
		var read bool
		for _, ref := range g.refsTo("prom:metric:" + metric) {
			if e.Class == "hard_keep" && !g.consumers[ref.ConsumerID].Kind.HardKeep() {
				continue
			}
			read = read || ref.AllLabels
			for _, l := range ref.Labels {
				read = read || l.Name == label
			}
		}
		read = read || len(g.refsTo("prom:label:"+metric+":"+label)) > 0
		switch e.Class {
		case "hard_keep":
			y.item(read, "label "+key+" (hard_keep)")
		case "drop_label":
			y.item(!read, "label "+key+" (drop_label)")
		}
	}
	for _, key := range sortedKeys(gt.Streams) {
		e := gt.Streams[key]
		match, ok := expectedReaders(e.Class, readersOf(g, streamRefs(t, g, streamLabels(t, key))))
		if ok {
			y.item(match, "stream "+key+" ("+e.Class+")")
		}
	}
	for _, name := range sortedKeys(gt.Rules) {
		rule := gt.Rules[name]
		var found bool
		for id, c := range g.consumers {
			if strings.HasPrefix(id, "rule:"+rule.Source+":") && strings.Contains(id, "/"+rule.Group+"/") && c.Name == name {
				found = true
				break
			}
		}
		y.item(found, "rule "+rule.Source+"/"+name)
	}
	for _, uid := range sortedKeys(gt.Dashboards) {
		d := gt.Dashboards[uid]
		var consumers, viewed int
		for id, c := range g.consumers {
			if strings.HasPrefix(id, "panel:1:"+uid+":") || strings.HasPrefix(id, "var:1:"+uid+":") {
				consumers++
				if !c.LastViewed.IsZero() {
					viewed++
				}
			}
		}
		want := d.Viewed != nil && *d.Viewed
		y.item(consumers > 0 && (want && viewed == consumers || !want && viewed == 0), "dashboard "+uid)
	}
	declared := func(id string, reads []string) {
		for _, m := range reads {
			var ok bool
			for _, ref := range g.refsTo("prom:metric:" + m) {
				ok = ok || ref.ConsumerID == id && ref.Evidence == graph.EvidenceDeclared
			}
			y.item(ok, "declared "+id+" reads "+m)
		}
	}
	for _, name := range sortedKeys(gt.SLOs) {
		s := gt.SLOs[name]
		declared("slo:"+s.Format+":"+filepath.Join(dir, s.File)+":"+name, s.Reads)
	}
	for _, link := range sortedKeys(gt.Runbooks) {
		declared("runbook:"+link, gt.Runbooks[link].Reads)
	}
	sources := map[string]report.Source{}
	for _, s := range r.Sources {
		sources[s.Name] = s
	}
	for _, name := range proven {
		s, ok := sources[name]
		y.item(ok && s.Configured && s.Coverage > 0 && s.Gap == "", "source "+name+" proven")
	}
	return evidenceScore{Found: y.found, Total: y.total, Score: ratio(y.found, y.total), Missed: orEmpty(y.missed)}
}

// outcome is what the report does with a signal, or what ground truth says
// it should do.
type outcome string

const (
	outKeep      outcome = "keep"
	outRecommend outcome = "recommend"
	outConfirm   outcome = "confirm"
)

func expectedOutcome(class string) (outcome, bool) {
	switch class {
	case "hard_keep", "keep_high":
		return outKeep, true
	case "keep_medium", "low", "drop_metric", "drop_label", "route_cold", "drop_rule":
		return outRecommend, true
	case "dormant_confirm":
		return outConfirm, true
	}
	return "", false
}

// scoreRecommendations: for every signal ground truth classifies, what the
// report raised (a recommendation or a confirm question) against what it
// should raise. A true positive is raised as the right kind; raising a
// keep, or the wrong kind, is a false positive; not raising it, a false
// negative.
func scoreRecommendations(t *testing.T, g scanGraph, gt groundTruth, r report.Report) recScore {
	t.Helper()
	got := map[string]outcome{}
	for _, rec := range r.Recommendations {
		got[rec.Signal.ID] = outRecommend
	}
	for _, c := range r.Confirm {
		got[c.Signal.ID] = outConfirm
	}
	want := map[string]outcome{} // signal ID -> expected
	set := func(id, class string) {
		if o, ok := expectedOutcome(class); ok {
			want[id] = o
		}
	}
	// Globs first, so a named entry overrides them.
	for _, name := range sortedKeys(gt.Metrics) {
		if !strings.Contains(name, "*") {
			continue
		}
		for id, s := range g.signals {
			if s.Kind != graph.KindMetric || s.Source != "prom" {
				continue
			}
			if _, named := gt.Metrics[s.Name]; named {
				continue
			}
			if ok, _ := path.Match(name, s.Name); ok {
				set(id, gt.Metrics[name].Class)
			}
		}
	}
	for _, name := range sortedKeys(gt.Metrics) {
		if !strings.Contains(name, "*") {
			set("prom:metric:"+name, gt.Metrics[name].Class)
		}
	}
	for key, e := range gt.Labels {
		metric, label, _ := strings.Cut(key, "/")
		set("prom:label:"+metric+":"+label, e.Class)
	}
	for name, rule := range gt.Rules {
		if rule.Kind == "record" {
			set("prom:metric:"+name, rule.Class)
		}
	}
	for key, e := range gt.Streams {
		set("loki:stream:"+canonicalStream(t, key), e.Class)
	}

	var s recScore
	judge := func(id string, w, o outcome) {
		switch {
		case o == w && w != outKeep:
			s.TruePositives++
		case o == w:
		case o != outKeep:
			s.FalsePositives++
			s.Wrong = append(s.Wrong, fmt.Sprintf("%s: %s, want %s", id, o, w))
			if w != outKeep {
				s.FalseNegatives++ // and the right kind wasn't raised
				s.Missed = append(s.Missed, fmt.Sprintf("%s: %s, want %s", id, o, w))
			}
		default:
			s.FalseNegatives++
			s.Missed = append(s.Missed, fmt.Sprintf("%s: not raised, want %s", id, w))
		}
	}
	for _, id := range sortedKeys(want) {
		o := got[id]
		if o == "" {
			o = outKeep
		}
		judge(id, want[id], o)
	}

	// Covering a keep (version 3): a reroute whose selector selects a
	// stream ground truth keeps moves that stream's lines too, whatever
	// the selector's own class. Each such stream is a false positive.
	for _, rec := range r.Recommendations {
		if rec.Signal.Kind != string(graph.KindStream) {
			continue
		}
		for _, key := range sortedKeys(gt.Streams) {
			id := "loki:stream:" + canonicalStream(t, key)
			if id == rec.Signal.ID || want[id] != outKeep {
				continue
			}
			if selectorMatches(t, rec.Signal.Name, streamLabels(t, key)) {
				s.FalsePositives++
				s.Wrong = append(s.Wrong, fmt.Sprintf("%s: %s covers %s, want keep", rec.Signal.ID, outRecommend, id))
			}
		}
	}

	// Line findings (version 2): each ground-truth line pattern is raised
	// as its kind, or not at all; a finding ground truth doesn't list is
	// wrong.
	matched := map[int]bool{}
	for _, name := range sortedKeys(gt.LinePatterns) {
		lp := gt.LinePatterns[name]
		w := outKeep
		if lp.Class == graph.LineRepeated || lp.Class == graph.LineHandoff {
			w = outcome(lp.Class)
		}
		o := outKeep
		for i, f := range r.LineFindings {
			if f.Stream == canonicalStream(t, lp.Stream) && linePatternMatches(lp.Pattern, f.Pattern) {
				o, matched[i] = outcome(f.Kind), true
			}
		}
		judge("line:"+name, w, o)
	}
	for i, f := range r.LineFindings {
		if !matched[i] {
			judge(fmt.Sprintf("line:%s %q", f.Stream, f.Pattern), outKeep, outcome(f.Kind))
		}
	}
	s.Wrong, s.Missed = orEmpty(s.Wrong), orEmpty(s.Missed)
	s.Precision = ratio(s.TruePositives, s.TruePositives+s.FalsePositives)
	s.Recall = ratio(s.TruePositives, s.TruePositives+s.FalseNegatives)
	return s
}

// linePatternMatches: does Loki's pattern got match ground truth's want,
// where "..." in want stands for any text?
func linePatternMatches(want, got string) bool {
	parts := strings.Split(want, "...")
	if len(parts) == 1 {
		return want == got
	}
	if !strings.HasPrefix(got, strings.TrimSpace(parts[0])) {
		return false
	}
	rest := got[len(strings.TrimSpace(parts[0])):]
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		i := strings.Index(rest, p)
		if i < 0 {
			return false
		}
		rest = rest[i+len(p):]
	}
	return strings.HasSuffix(got, strings.TrimSpace(parts[len(parts)-1]))
}

func canonicalStream(t *testing.T, key string) string {
	t.Helper()
	labels := streamLabels(t, key)
	parts := make([]string, 0, len(labels))
	for _, k := range sortedKeys(labels) {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// orEmpty keeps an empty list a list in baseline.json.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ratio is n/d rounded to 4 places, 1 when there is nothing to score.
func ratio(n, d int) float64 {
	if d == 0 {
		return 1
	}
	return math.Round(float64(n)/float64(d)*1e4) / 1e4
}

// TestScanLineFindings (S5 items 3-4): the before-deploy scan reads the
// largest streams' patterns. It reports the startup burst, an identical
// warn line, as a repeated line with its rate and saving, and hands the
// info stream's successful health checks (Loki's three probe patterns,
// merged) to zeroreads; neither makes its stream a recommendation.
func TestScanLineFindings(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	st := bm.States[slices.IndexFunc(bm.States, func(s benchState) bool { return s.Name == "before-deploy" })]
	_, r, _ := scanState(t, st, dirs[st.Name])
	const warn = `{app="shop", component="api", level="warn"}`
	const info = `{app="shop", component="api", level="info"}`
	var found, handoff bool
	for _, f := range r.LineFindings {
		if f.Kind == graph.LineHandoff {
			if f.Stream != info || f.Pattern != `method=GET route=/healthz status=200 duration_ms=<_> probe=<_> replica=<_> request_id=<_> user=-` {
				t.Errorf("hand-off %s in %s: %q", f.ID, f.Stream, f.Pattern)
				continue
			}
			handoff = true
			// tvt-lab: about 64% of the info stream's bytes.
			if f.Share < 0.55 || f.Share > 0.75 || f.Savings.Monthly <= 0 || !strings.Contains(f.Action, "zeroreads") || f.Handoff != report.ZeroreadsURL {
				t.Errorf("healthz hand-off %+v", f)
			}
			continue
		}
		if f.Stream != warn || !strings.Contains(f.Pattern, "key=cache.ttl") {
			t.Errorf("repeated line %s in %s: %q", f.ID, f.Stream, f.Pattern)
			continue
		}
		found = true
		if f.Lines != 10000 || f.Share < 0.5 || f.Share > 0.6 || f.PerSecond <= 0 || f.Savings.Monthly <= 0 || !f.Savings.Approximate || f.Handoff != report.ZeroreadsURL {
			t.Errorf("burst finding %+v", f)
		}
	}
	if !found || !handoff {
		t.Fatalf("burst found %v, healthz hand-off %v: %+v", found, handoff, r.LineFindings)
	}
	// No reroute may cover either stream, through its own selector or a
	// wider one: the on-call query's {app="shop", component="api",
	// level=~"warn|error"} selects the warn stream, which Shop Overview
	// panel 6 shows (fixed in the S5 debug pass: overlapping selectors
	// share readers).
	for _, stream := range []string{warn, info} {
		labels := streamLabels(t, stream)
		for _, rec := range r.Recommendations {
			if rec.Signal.Kind == string(graph.KindStream) && selectorMatches(t, rec.Signal.Name, labels) {
				t.Errorf("recommendation %s (%s) covers %s, which has a line finding", rec.ID, rec.Action, stream)
			}
		}
	}
}
