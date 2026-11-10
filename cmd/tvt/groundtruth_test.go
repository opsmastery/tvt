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
	Mimir   struct {
		Rules   map[string]gtRule  `yaml:"rules"`
		Metrics map[string]gtEntry `yaml:"metrics"`
	} `yaml:"mimir"`
}

type gtEntry struct {
	Class string `yaml:"class"`
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
		refs, err := parse.LogStreamSelectors(key)
		if err != nil || len(refs) != 1 {
			t.Fatalf("ground-truth stream %s: %v", key, err)
		}
		labels := map[string]string{}
		for _, m := range refs[0].Matchers {
			labels[m.Label] = m.Value
		}
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
	refs, err := parse.LogStreamSelectors(sel)
	if err != nil {
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

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
