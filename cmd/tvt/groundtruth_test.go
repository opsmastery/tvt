package main

import (
	"context"
	"math"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
)

// groundTruth is the part of tvt-lab's ground-truth.yaml (vendored by
// scripts/capture-fixtures.sh as testdata/lab/ground-truth.yaml) that the
// golden tests assert. Each sprint asserts more of it; nothing asserted
// earlier is dropped.
type groundTruth struct {
	Metrics    map[string]gtEntry `yaml:"metrics"`
	Rules      map[string]gtRule  `yaml:"rules"`
	Dashboards map[string]struct {
		Title  string `yaml:"title"`
		Viewed *bool  `yaml:"viewed"`
	} `yaml:"dashboards"`
	Streams map[string]gtEntry `yaml:"streams"`
	// Declared dependencies outside the rulers (Sprint 2 item 7).
	SLOs map[string]struct {
		Format string   `yaml:"format"`
		File   string   `yaml:"file"`
		Reads  []string `yaml:"reads"`
	} `yaml:"slos"`
	Runbooks map[string]struct {
		Alerts []string `yaml:"alerts"`
		Reads  []string `yaml:"reads"`
	} `yaml:"runbooks"`
	Labels map[string]gtEntry `yaml:"labels"`
	Mimir  struct {
		Rules   map[string]gtRule  `yaml:"rules"`
		Metrics map[string]gtEntry `yaml:"metrics"`
	} `yaml:"mimir"`
}

type gtEntry struct {
	Class  string `yaml:"class"`
	Source string `yaml:"source"` // where the evidence comes from, when not a rule tvt reads
}

type gtRule struct {
	Source string `yaml:"source"`
	Group  string `yaml:"group"`
	Kind   string `yaml:"kind"` // alert or record
	Class  string `yaml:"class"`
}

func loadGroundTruth(t *testing.T) groundTruth {
	t.Helper()
	return parseGroundTruth(t, testutil.ReadFile(t, "lab/ground-truth.yaml"))
}

// parseGroundTruth reads a ground-truth.yaml: the vendored copy for the
// golden tests, the live lab's own for the e2e suite.
func parseGroundTruth(t *testing.T, b []byte) groundTruth {
	t.Helper()
	var gt groundTruth
	if err := yaml.Unmarshal(b, &gt); err != nil {
		t.Fatalf("ground-truth.yaml: %v", err)
	}
	for name, r := range gt.Mimir.Rules {
		if r.Source == "" {
			r.Source = "mimir"
			gt.Mimir.Rules[name] = r
		}
	}
	if len(gt.Metrics) == 0 || len(gt.Rules) == 0 || len(gt.Dashboards) == 0 || len(gt.Streams) == 0 || len(gt.Mimir.Rules) == 0 ||
		len(gt.SLOs) == 0 || len(gt.Runbooks) == 0 {
		t.Fatalf("ground-truth.yaml is missing a section the golden test asserts: %+v", gt)
	}
	return gt
}

// scanGraph is one scan's rows, keyed for lookups.
type scanGraph struct {
	signals   map[string]graph.Signal
	consumers map[string]graph.Consumer
	refs      []graph.Reference
	loads     []graph.DashboardLoad
}

func readGraph(t *testing.T, db string) scanGraph {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	g := scanGraph{signals: map[string]graph.Signal{}, consumers: map[string]graph.Consumer{}}
	sigs, err := st.Signals(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sigs {
		g.signals[s.ID] = s
	}
	cons, err := st.Consumers(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cons {
		g.consumers[c.ID] = c
	}
	if g.refs, err = st.References(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if g.loads, err = st.Loads(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	return g
}

// assertMetrics: every ground-truth metric is a metric signal from src. A
// name with * is a glob that must match at least one signal.
func assertMetrics(t *testing.T, g scanGraph, src string, metrics map[string]gtEntry) {
	t.Helper()
	for name := range metrics {
		if !strings.Contains(name, "*") {
			if s, ok := g.signals[src+":metric:"+name]; !ok || s.Kind != graph.KindMetric {
				t.Errorf("ground-truth metric %s is not a %s signal", name, src)
			}
			continue
		}
		matched := false
		for _, s := range g.signals {
			if s.Kind != graph.KindMetric || s.Source != src {
				continue
			}
			if ok, err := path.Match(name, s.Name); err != nil {
				t.Fatalf("ground-truth metric glob %q: %v", name, err)
			} else if ok {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("ground-truth metric glob %s matches no %s signal", name, src)
		}
	}
}

// assertRules: every ground-truth rule from one of sources is a consumer of
// its kind and name in its group (rule:<source>:<namespace>/<group>/<key>;
// the namespace is the ruler's file or namespace, or a Grafana folder UID,
// which ground truth doesn't record, and a Grafana rule's key is its UID).
// It returns how many rules it asserted.
func assertRules(t *testing.T, g scanGraph, rules map[string]gtRule, sources ...string) int {
	t.Helper()
	var n int
	for name, r := range rules {
		if !contains(sources, r.Source) {
			continue
		}
		n++
		want := map[string]graph.ConsumerKind{"alert": graph.ConsumerAlertRule, "record": graph.ConsumerRecordingRule}[r.Kind]
		if want == "" {
			t.Fatalf("ground-truth rule %s: kind %q, want alert or record", name, r.Kind)
		}
		found := false
		for id, c := range g.consumers {
			if strings.HasPrefix(id, "rule:"+r.Source+":") && strings.Contains(id, "/"+r.Group+"/") && c.Name == name && c.Kind == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ground-truth %s rule %s (group %s) is not a %s consumer", r.Source, name, r.Group, want)
		}
	}
	return n
}

// assertDashboards: a viewed dashboard's panels and variables all carry
// last_viewed and it has browser loads; an unviewed one has neither.
func assertDashboards(t *testing.T, g scanGraph, org string, gt groundTruth) {
	t.Helper()
	for uid, d := range gt.Dashboards {
		if d.Viewed == nil {
			t.Fatalf("ground-truth dashboard %s has no viewed:", uid)
		}
		var n int
		for id, c := range g.consumers {
			if !strings.HasPrefix(id, "panel:"+org+":"+uid+":") && !strings.HasPrefix(id, "var:"+org+":"+uid+":") {
				continue
			}
			n++
			if *d.Viewed && c.LastViewed.IsZero() {
				t.Errorf("%s (%s) is viewed, but consumer %s has no last_viewed", d.Title, uid, id)
			}
			if !*d.Viewed && !c.LastViewed.IsZero() {
				t.Errorf("%s (%s) is never viewed, but consumer %s was viewed at %v", d.Title, uid, id, c.LastViewed)
			}
		}
		if n == 0 {
			t.Errorf("dashboard %s (%s) has no consumers", d.Title, uid)
		}
		var browser int64
		for _, l := range g.loads {
			if l.DashboardUID == uid && l.Browser {
				browser += l.Loads
			}
		}
		if *d.Viewed != (browser > 0) {
			t.Errorf("%s (%s): viewed %v, but %d browser loads", d.Title, uid, *d.Viewed, browser)
		}
	}
}

// assertHardKeepStreams: every hard_keep stream in ground truth (a full
// label set) is matched by the selector of a stream signal that a hard-keep
// consumer references.
func assertHardKeepStreams(t *testing.T, g scanGraph, streams map[string]gtEntry) {
	t.Helper()
	var keys []string
	for k, s := range streams {
		if s.Class == "hard_keep" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		t.Fatal("ground truth has no hard_keep stream")
	}
	sort.Strings(keys)
	for _, key := range keys {
		labels := streamLabels(t, key)
		protected := false
		for _, r := range g.refs {
			s, c := g.signals[r.SignalID], g.consumers[r.ConsumerID]
			if s.Kind == graph.KindStream && c.Kind.HardKeep() && selectorMatches(t, s.Name, labels) {
				protected = true
				break
			}
		}
		if !protected {
			t.Errorf("hard_keep stream %s has no hard-keep reference", key)
		}
	}
}

// selectorMatches reports whether a stream with labels is selected by sel
// (a canonical selector, or scan.AllStreams).
func selectorMatches(t *testing.T, sel string, labels map[string]string) bool {
	t.Helper()
	if sel == scan.AllStreams {
		return true
	}
	refs, err := parse.LogQL(sel)
	if err != nil || len(refs) != 1 {
		t.Fatalf("stream signal %q: %v", sel, err)
	}
	ok, err := parse.MatchesLabels(refs[0].Matchers, labels)
	if err != nil {
		t.Fatalf("stream signal %q: %v", sel, err)
	}
	return ok
}

// fixtureLag lists ground-truth entries the recorded fixtures predate:
// tvt-lab has the seed, but testdata/lab was captured before it. The golden
// test skips them, and fails once a recapture makes them pass, so the list
// can't go stale.
var fixtureLag = map[string]string{}

// groundTruthConflicts lists ground-truth entries that contradict the lab's
// own seeds, to fix in tvt-lab. The golden test skips them and fails once a
// ground-truth update resolves them.
var groundTruthConflicts = map[string]string{
	`{app="shop", component="api", level="debug"}`: `route_cold says "never queried", but Shop Overview panel 8 ` +
		`(sum by (level) (rate({app="shop", component="api"}[$__auto]))) reads every api stream, debug included`,
}

// refsTo returns every reference to a signal with its consumer.
// refsTo is a signal's references, sweeps left out: a sweep (S3 item 6)
// is weak evidence that keeps nothing on its own, so ground truth's reads
// never count it.
func (g scanGraph) refsTo(signalID string) []graph.Reference {
	var out []graph.Reference
	for _, r := range g.refs {
		if r.SignalID == signalID && !r.Sweep {
			out = append(out, r)
		}
	}
	return out
}

func isDashboard(k graph.ConsumerKind) bool {
	return k == graph.ConsumerDashboardPanel || k == graph.ConsumerDashboardVariable || k == graph.ConsumerDashboardAnnotation
}

// assertReferences checks Sprint 2 item 3's ground truth on metric, label
// and stream references: every seeded dependency resolves to the right
// signal, and nothing references what the lab leaves unused.
func assertReferences(t *testing.T, g scanGraph, src string, gt groundTruth) {
	t.Helper()
	// Evidence classes follow the consumer.
	for _, r := range g.refs {
		if want := g.consumers[r.ConsumerID].Kind.Evidence(); r.Evidence != want {
			t.Errorf("reference %s <- %s: evidence %q, want %q", r.SignalID, r.ConsumerID, r.Evidence, want)
		}
	}

	names := make([]string, 0, len(gt.Metrics))
	for name := range gt.Metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := gt.Metrics[name]
		if strings.Contains(name, "*") {
			continue // globs: below
		}
		var hard, viewed, dash, adhoc, read bool
		for _, r := range g.refsTo(src + ":metric:" + name) {
			c := g.consumers[r.ConsumerID]
			read = true
			hard = hard || c.Kind.HardKeep()
			dash = dash || isDashboard(c.Kind)
			viewed = viewed || isDashboard(c.Kind) && !c.LastViewed.IsZero()
			adhoc = adhoc || (c.Kind == graph.ConsumerAdhocQuery || c.Kind == graph.ConsumerAgentQuery) && !r.LastSeen.IsZero()
		}
		var ok bool
		switch e.Class {
		case "hard_keep":
			ok = hard
		case "keep_high":
			ok = viewed
		case "low":
			ok = dash && !viewed && !hard
		case "keep_medium":
			ok = adhoc && !hard && !dash // a query log saw it, and only ad hoc
		case "drop_metric":
			ok = !hard && !dash && !adhoc
		default:
			t.Fatalf("ground-truth metric %s: class %q", name, e.Class)
		}
		lag, lagging := fixtureLag[name]
		switch {
		case lagging && ok:
			t.Errorf("%s now resolves as %s: remove it from fixtureLag", name, e.Class)
		case lagging:
			t.Logf("%s (%s): skipped, %s", name, e.Class, lag)
		case !ok:
			t.Errorf("ground-truth metric %s is %s, but references: hard keep %v, dashboard %v (viewed %v), ad hoc %v, any %v",
				name, e.Class, hard, dash, viewed, adhoc, read)
		}
	}

	// The transitive hard keep: the histogram is protected through the
	// recording rule, with Via set.
	var via bool
	for _, r := range g.refsTo(src + ":metric:shop_http_request_duration_seconds_bucket") {
		if g.consumers[r.ConsumerID].Kind == graph.ConsumerAlertRule && strings.HasSuffix(r.Via, "/route:shop_http_request_duration_seconds:p99_5m") {
			via = true
		}
	}
	if !via {
		t.Error("shop_http_request_duration_seconds_bucket has no alert reference via the p99 recording rule")
	}

	// Glob drop_metric entries: no alert or dashboard reads any metric they
	// match, unless ground truth names it on its own.
	for _, name := range names {
		if e := gt.Metrics[name]; !strings.Contains(name, "*") || e.Class != "drop_metric" {
			continue
		}
		for id, s := range g.signals {
			if s.Kind != graph.KindMetric || s.Source != src {
				continue
			}
			if ok, _ := path.Match(name, s.Name); !ok {
				continue
			}
			if _, named := gt.Metrics[s.Name]; named {
				continue
			}
			for _, r := range g.refsTo(id) {
				if k := g.consumers[r.ConsumerID].Kind; k.HardKeep() || isDashboard(k) || k == graph.ConsumerAdhocQuery || k == graph.ConsumerAgentQuery {
					t.Errorf("%s matches drop_metric glob %s, but %s reads it", s.Name, name, r.ConsumerID)
				}
			}
		}
	}

	// A drop_rule's output is read by nothing.
	for name, r := range gt.Rules {
		if r.Class != "drop_rule" {
			continue
		}
		if refs := g.refsTo(src + ":metric:" + name); len(refs) > 0 {
			t.Errorf("drop_rule %s: its output is read by %+v", name, refs)
		}
	}

	// drop_label: no consumer reads the label, on the label signal or in a
	// metric reference's label detail.
	for key, e := range gt.Labels {
		if e.Class != "drop_label" {
			continue
		}
		metric, label, _ := strings.Cut(key, "/")
		if refs := g.refsTo(src + ":label:" + metric + ":" + label); len(refs) > 0 {
			t.Errorf("drop_label %s is read by %+v", key, refs)
		}
		for _, r := range g.refsTo(src + ":metric:" + metric) {
			for _, l := range r.Labels {
				if l.Name == label {
					t.Errorf("drop_label %s: %s reads it (%s)", key, r.ConsumerID, l.Use)
				}
			}
			if r.AllLabels {
				t.Errorf("drop_label %s: %s depends on every label", key, r.ConsumerID)
			}
		}
	}

	// hard_keep labels: a hard-keep consumer reads the label on its metric
	// (or every label), so no drop_label can be recommended for it.
	for key, e := range gt.Labels {
		if e.Class != "hard_keep" {
			continue
		}
		metric, label, _ := strings.Cut(key, "/")
		var readers []string
		for _, r := range g.refsTo(src + ":metric:" + metric) {
			if !g.consumers[r.ConsumerID].Kind.HardKeep() {
				continue
			}
			for _, l := range r.Labels {
				if l.Name == label {
					readers = append(readers, r.ConsumerID)
				}
			}
			if r.AllLabels {
				readers = append(readers, r.ConsumerID)
			}
		}
		if len(readers) == 0 {
			t.Errorf("hard_keep label %s: no hard-keep consumer reads it", key)
		}
	}

	// Streams: keep_high is read by a viewed dashboard; route_cold by
	// nothing.
	for key := range groundTruthConflicts {
		e, ok := gt.Streams[key]
		if !ok || e.Class != "route_cold" {
			t.Errorf("groundTruthConflicts stream %s is no longer route_cold: remove it from groundTruthConflicts", key)
		}
	}
	for key, e := range gt.Streams {
		labels := streamLabels(t, key)
		var viewed, read bool
		for _, r := range g.refs {
			s, c := g.signals[r.SignalID], g.consumers[r.ConsumerID]
			if s.Kind != graph.KindStream || !selectorMatches(t, s.Name, labels) {
				continue
			}
			read = true
			viewed = viewed || isDashboard(c.Kind) && !c.LastViewed.IsZero()
		}
		switch e.Class {
		case "keep_high":
			if !viewed {
				t.Errorf("keep_high stream %s is on no viewed dashboard", key)
			}
		case "route_cold":
			conflict, known := groundTruthConflicts[key]
			switch {
			case known && !read:
				t.Errorf("route_cold stream %s is no longer referenced: remove it from groundTruthConflicts", key)
			case known:
				t.Logf("%s (route_cold): skipped, %s", key, conflict)
			case read:
				t.Errorf("route_cold stream %s is referenced", key)
			}
		}
	}
}

// assertHardKeepMetrics: each metric is referenced by a hard-keep consumer.
func assertHardKeepMetrics(t *testing.T, g scanGraph, src string, metrics map[string]gtEntry) {
	t.Helper()
	for name, e := range metrics {
		if e.Class != "hard_keep" {
			continue
		}
		var hard bool
		for _, r := range g.refsTo(src + ":metric:" + name) {
			hard = hard || g.consumers[r.ConsumerID].Kind.HardKeep()
		}
		if !hard {
			t.Errorf("hard_keep metric %s has no hard-keep reference", name)
		}
	}
}

func streamLabels(t *testing.T, key string) map[string]string {
	t.Helper()
	refs, err := parse.LogQL(key)
	if err != nil || len(refs) != 1 {
		t.Fatalf("ground-truth stream %s: %v", key, err)
	}
	labels := map[string]string{}
	for _, m := range refs[0].Matchers {
		labels[m.Label] = m.Value
	}
	return labels
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// assertClasses checks Sprint 2 item 6's classification against ground
// truth: every hard keep comes out hard_keep and nothing else does, viewed
// dashboard signals are keep_high (protected), and the rest are ranked
// candidates. A seeded drop that the dormant name rules catch is logged,
// not failed: Sprint 3 decides whether ground truth should call it
// "dormant: confirm" (see the PR).
func assertClasses(t *testing.T, g scanGraph, src string, gt groundTruth) {
	t.Helper()
	want := map[string][]string{
		"hard_keep":   {"hard_keep"},
		"keep_high":   {"keep_high"},
		"keep_medium": {"candidate"},
		"low":         {"candidate"},
		"drop_metric": {"candidate", "dormant"},
		"drop_label":  {"candidate", "dormant"},
	}
	expect := func(s graph.Signal) (gtEntry, bool) {
		switch s.Kind {
		case graph.KindMetric:
			if e, ok := gt.Metrics[s.Name]; ok {
				return e, true
			}
			for name, e := range gt.Metrics {
				if ok, _ := path.Match(name, s.Name); ok && strings.Contains(name, "*") {
					return e, true
				}
			}
		case graph.KindLabel:
			e, ok := gt.Labels[s.Parent+"/"+s.Name]
			return e, ok
		}
		return gtEntry{}, false
	}
	var dormant []string
	ids := make([]string, 0, len(g.signals))
	for id := range g.signals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var checked int
	for _, id := range ids {
		s := g.signals[id]
		if s.Source != src || s.Class == "" {
			if s.Source == src {
				t.Errorf("signal %s has no class", id)
			}
			continue
		}
		e, ok := expect(s)
		if !ok {
			continue
		}
		checked++
		classes, known := want[e.Class]
		if !known {
			t.Fatalf("ground-truth class %q for %s", e.Class, id)
		}
		if !slices.Contains(classes, s.Class) {
			t.Errorf("%s: class %s, ground truth %s (want %v)", id, s.Class, e.Class, classes)
		}
		if s.Class == "dormant" {
			dormant = append(dormant, s.Name+" ("+s.Dormant+")")
		}
	}
	if checked == 0 {
		t.Fatal("no signal matched a ground-truth metric or label")
	}
	if len(dormant) > 0 {
		t.Logf("%d seeded drops the dormant name rules hold back: %s", len(dormant), strings.Join(dormant, ", "))
	}

	// Streams: the selectors that select a hard_keep stream include a
	// hard_keep one; a keep_high stream's include a protected one.
	for key, e := range gt.Streams {
		if e.Class != "hard_keep" && e.Class != "keep_high" {
			continue
		}
		labels := streamLabels(t, key)
		var best string
		for _, s := range g.signals {
			if s.Kind != graph.KindStream || !selectorMatches(t, s.Name, labels) {
				continue
			}
			if s.Class == "hard_keep" || s.Class == "keep_high" && best == "" {
				best = s.Class
			}
		}
		if e.Class == "hard_keep" && best != "hard_keep" || e.Class == "keep_high" && best == "" {
			t.Errorf("stream %s (%s): best selector class %q", key, e.Class, best)
		}
	}
}

// assertDeclared: each ground-truth SLO and runbook is a consumer of its
// kind (SLO IDs carry the file path under labDir), and every metric it
// reads has a declared reference from it (and is a hard keep, checked by
// assertClasses).
func assertDeclared(t *testing.T, g scanGraph, src, labDir string, gt groundTruth) {
	t.Helper()
	check := func(id string, kind graph.ConsumerKind, reads []string) {
		t.Helper()
		c, ok := g.consumers[id]
		if !ok || c.Kind != kind {
			t.Errorf("consumer %s: %+v (found %v), want kind %s", id, c, ok, kind)
			return
		}
		for _, m := range reads {
			found := false
			for _, r := range g.refsTo(src + ":metric:" + m) {
				if r.ConsumerID == id && r.Evidence == graph.EvidenceDeclared {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: no declared reference from %s", m, id)
			}
		}
	}
	for name, s := range gt.SLOs {
		check("slo:"+s.Format+":"+filepath.Join(labDir, s.File)+":"+name, graph.ConsumerSLO, s.Reads)
	}
	for link, rb := range gt.Runbooks {
		check("runbook:"+link, graph.ConsumerRunbook, rb.Reads)
		if c := g.consumers["runbook:"+link]; len(rb.Alerts) > 0 && !strings.Contains(c.Name, strings.Join(rb.Alerts, ", ")) {
			t.Errorf("runbook consumer %q doesn't name its alerts %v", c.Name, rb.Alerts)
		}
	}
}

// assertCosts: the scan used the wanted price card, and every metric with
// series has a monthly cost and the interval its samples are counted at,
// measured (Prometheus targets API) unless assumed is true (Mimir). A
// recording rule's output (a name with a colon) always has its rule's
// evaluation interval.
func assertCosts(t *testing.T, db, src, card string, assumed bool) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.PriceCard != card || info.Currency != "USD" || !info.PricePreset {
		t.Errorf("scan price card %q %q preset %v, want %s (USD, preset)", info.PriceCard, info.Currency, info.PricePreset, card)
	}
	costs, err := st.Costs(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var metrics, measured int
	var total float64
	for _, c := range costs {
		if !strings.HasPrefix(c.SignalID, src+":metric:") || c.ActiveSeries == 0 {
			continue
		}
		metrics++
		total += c.MonthlyCost
		if c.MonthlyCost <= 0 || c.Interval <= 0 {
			t.Errorf("%s: monthly %v interval %v", c.SignalID, c.MonthlyCost, c.Interval)
		}
		name := strings.TrimPrefix(c.SignalID, src+":metric:")
		if c.IntervalAssumed == "" && !strings.Contains(name, ":") { // recording rules: their evaluation interval
			measured++
		} else if c.IntervalAssumed != "" && !assumed && !strings.Contains(c.IntervalAssumed, "target metadata") {
			t.Errorf("%s: interval assumed (%s) on a scan with the targets API", c.SignalID, c.IntervalAssumed)
		}
	}
	if metrics == 0 {
		t.Fatal("no metric costs")
	}
	if assumed && measured > 0 {
		t.Errorf("%d metric intervals measured on a scan without a targets API", measured)
	}
	if !assumed && measured < metrics/2 {
		t.Errorf("only %d of %d metric intervals measured", measured, metrics)
	}
	t.Logf("%d metrics cost %.2f %s/month on %s", metrics, total, info.Currency, card)
}

// assertStreamCosts: the lab's five streams are stored with their bytes,
// and every stream signal costs the streams its selector matches, at the
// card's log price.
func assertStreamCosts(t *testing.T, db string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vols, err := st.StreamVolumes(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 5 || info.VolumeTruncated {
		t.Fatalf("stream volumes %+v (truncated %v)", vols, info.VolumeTruncated)
	}
	costs, err := st.Costs(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var streams int
	for _, c := range costs {
		sel, ok := strings.CutPrefix(c.SignalID, "loki:stream:")
		if !ok {
			continue
		}
		streams++
		var want int64
		for _, v := range vols {
			if selectorMatches(t, sel, streamLabels(t, v.Selector)) {
				want += v.BytesPerDay
			}
		}
		if c.BytesPerDay != want || math.Abs(c.MonthlyCost-float64(want)/1e9*30*0.45) > 1e-9 {
			t.Errorf("%s: %d bytes/day %v/month, want %d", c.SignalID, c.BytesPerDay, c.MonthlyCost, want)
		}
	}
	if streams == 0 {
		t.Fatal("no stream costs")
	}
	t.Logf("%d stream signals priced from %d streams", streams, len(vols))
}

// assertLoggedClients (S3 item 5, Sprint 2 item 15): Grafana's Shop
// Overview queries in the Prometheus log (from 10.89.0.6, no headers) join
// to their panels, and the viewer's direct query (10.89.0.9) stays another
// API client.
func assertLoggedClients(t *testing.T, db string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cons, err := st.Consumers(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	panels := map[string]bool{}
	clients := map[string]int{}
	for _, c := range cons {
		if !strings.HasPrefix(c.ID, "qlog:") {
			continue
		}
		clients[c.Client]++
		if rest, ok := strings.CutPrefix(c.ID, "qlog:prometheus:match:panel:1:shop-overview:"); ok && c.Client == "dashboard" {
			panels[rest[:strings.IndexByte(rest, ':')]] = true
			if c.LastViewed.IsZero() {
				t.Errorf("%s: matched panel without a last run", c.ID)
			}
		}
		if strings.Contains(c.Expr, "node_memory_MemAvailable_bytes") && c.Client != "api" {
			t.Errorf("viewer's direct query: client %q", c.Client)
		}
	}
	for _, p := range []string{"1", "2", "3", "4", "5"} {
		if !panels[p] {
			t.Errorf("shop-overview panel %s: no query-log match (have %v)", p, panels)
		}
	}
	if clients[""] > 0 {
		t.Errorf("%d query-log consumers without a client", clients[""])
	}
	t.Logf("query-log consumers by client: %v", clients)
}

// assertScores: every candidate signal has exactly one score and nothing
// else has one, scores come highest priority first, and priority is cost /
// (1 + weight) × confidence. It returns the scores by signal.
func assertScores(t *testing.T, db string) map[string]graph.Score {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scores, err := st.Scores(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	sigs, err := st.Signals(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]graph.Score{}
	for i, r := range scores {
		if _, dup := by[r.SignalID]; dup {
			t.Errorf("%s scored twice", r.SignalID)
		}
		by[r.SignalID] = r
		if i > 0 && r.Priority > scores[i-1].Priority {
			t.Errorf("scores out of order at %s", r.SignalID)
		}
		if want := r.MonthlyCost / (1 + r.UsageWeight) * r.Confidence; math.Abs(want-r.Priority) > 1e-9 {
			t.Errorf("%s priority %v, want %v", r.SignalID, r.Priority, want)
		}
	}
	var candidates int
	for _, s := range sigs {
		_, scored := by[s.ID]
		if s.Class == string(score.ClassCandidate) {
			candidates++
			if !scored {
				t.Errorf("candidate %s has no score", s.ID)
			}
		} else if scored {
			t.Errorf("%s (%s) was scored", s.ID, s.Class)
		}
	}
	if candidates == 0 {
		t.Error("no candidates")
	}
	return by
}
