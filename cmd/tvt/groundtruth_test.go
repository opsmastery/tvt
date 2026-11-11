package main

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
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
	Labels  map[string]gtEntry `yaml:"labels"`
	Mimir   struct {
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
	var gt groundTruth
	if err := yaml.Unmarshal(testutil.ReadFile(t, "lab/ground-truth.yaml"), &gt); err != nil {
		t.Fatalf("ground-truth.yaml: %v", err)
	}
	for name, r := range gt.Mimir.Rules {
		if r.Source == "" {
			r.Source = "mimir"
			gt.Mimir.Rules[name] = r
		}
	}
	if len(gt.Metrics) == 0 || len(gt.Rules) == 0 || len(gt.Dashboards) == 0 || len(gt.Streams) == 0 || len(gt.Mimir.Rules) == 0 {
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
// its kind (rule:<source>:<namespace>/<group>/<name>; the namespace is the
// ruler's file or namespace, which ground truth doesn't record). It returns
// how many rules it asserted.
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
			if strings.HasPrefix(id, "rule:"+r.Source+":") && strings.HasSuffix(id, "/"+r.Group+"/"+name) && c.Kind == want {
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
	for _, m := range refs[0].Matchers {
		v := labels[m.Label]
		var ok bool
		switch m.Op {
		case parse.MatchEqual:
			ok = v == m.Value
		case parse.MatchNotEqual:
			ok = v != m.Value
		case parse.MatchRegexp, parse.MatchNotRegex:
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			if err != nil {
				t.Fatalf("stream signal %q: %v", sel, err)
			}
			ok = re.MatchString(v) == (m.Op == parse.MatchRegexp)
		}
		if !ok {
			return false
		}
	}
	return true
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

// waitsFor lists ground-truth evidence a later Sprint 2 item collects.
func waitsFor(e gtEntry) string {
	if strings.Contains(e.Source, "Grafana ruler") {
		return "Grafana-managed rules (Sprint 2 item 5)"
	}
	return ""
}

// refsTo returns every reference to a signal with its consumer.
func (g scanGraph) refsTo(signalID string) []graph.Reference {
	var out []graph.Reference
	for _, r := range g.refs {
		if r.SignalID == signalID {
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
		if why := waitsFor(e); why != "" {
			t.Logf("%s (%s): waits for %s", name, e.Class, why)
			continue
		}
		var hard, viewed, dash, read bool
		for _, r := range g.refsTo(src + ":metric:" + name) {
			c := g.consumers[r.ConsumerID]
			read = true
			hard = hard || c.Kind.HardKeep()
			dash = dash || isDashboard(c.Kind)
			viewed = viewed || isDashboard(c.Kind) && !c.LastViewed.IsZero()
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
			ok = !hard && !dash // ad hoc evidence arrives with query logs (item 4)
		case "drop_metric":
			ok = !hard && !dash
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
			t.Errorf("ground-truth metric %s is %s, but references: hard keep %v, dashboard %v (viewed %v), any %v",
				name, e.Class, hard, dash, viewed, read)
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
				if k := g.consumers[r.ConsumerID].Kind; k.HardKeep() || isDashboard(k) {
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

	// Streams: keep_high is read by a viewed dashboard; route_cold by
	// nothing.
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
