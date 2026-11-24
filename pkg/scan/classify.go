package scan

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/opsmastery/tvt/pkg/config"
	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/parse"
	"github.com/opsmastery/tvt/pkg/score"
)

// classify sets every signal's class, owner and tier (Sprint 2 item 6).
//
//   - hard_keep: a hard-keep consumer (alert rule, Grafana-managed alert,
//     SLO, runbook step, external consumer) references it, directly or through a
//     recording rule (chains give the alert a reference with Via set).
//   - keep_high: a dashboard viewed in the window references it.
//   - dormant: no consumer references it, and its name, its owner's
//     tier, or a runbook or postmortem that names it says it may be
//     needed in an incident.
//   - candidate: everything else, ranked by score.Score.
//
// A label signal takes its metric's owner, tier and name rules; a stream
// signal's name rules run over its selector.
func classify(signals []graph.Signal, consumers []graph.Consumer, refs []graph.Reference, overlaps map[string][]string, owners []config.Owner, mentions map[string]string, missed map[string]string, since, until time.Time) error {
	return classifyOwned(signals, consumers, refs, overlaps, owners, OwnerSources{}, nil, mentions, missed, since, until)
}

// classifyOwned is classify with the other owner sources (S9 item 1) and
// each metric's jobs, which name its component.
func classifyOwned(signals []graph.Signal, consumers []graph.Consumer, refs []graph.Reference, overlaps map[string][]string, owners []config.Owner, src OwnerSources, jobs map[string][]string, mentions map[string]string, missed map[string]string, since, until time.Time) error {
	om, err := newOwnerMatcher(owners)
	if err != nil {
		return err
	}
	or := &ownerResolver{cfg: om, src: src, jobs: jobs}
	byID := make(map[string]graph.Consumer, len(consumers))
	for _, c := range consumers {
		byID[c.ID] = c
	}
	facts := map[string]*score.Facts{}
	// resultKept: metrics whose labels a protecting consumer's result keeps
	// (no aggregation removes them, as in rate(x[5m]) > 100). Not a read
	// of any label, but removing one merges series and changes what the
	// alert fires on or the viewed panel shows, so each label of the
	// metric is protected like the metric.
	resultKept := map[string]*score.Facts{}
	for _, r := range refs {
		f := facts[r.SignalID]
		if f == nil {
			f = &score.Facts{}
			facts[r.SignalID] = f
		}
		c := byID[r.ConsumerID]
		if r.Sweep {
			f.Swept = true // weak: keeps nothing on its own
			continue
		}
		f.Read = true
		hard := c.Kind.HardKeep()
		viewed := isDashboard(c.Kind) && !c.LastViewed.IsZero() && !c.LastViewed.Before(since) && !c.LastViewed.After(until)
		f.HardKeep = f.HardKeep || hard
		f.Viewed = f.Viewed || viewed
		if r.ResultLabels && (hard || viewed) {
			k := resultKept[r.SignalID]
			if k == nil {
				k = &score.Facts{}
				resultKept[r.SignalID] = k
			}
			k.HardKeep, k.Viewed = k.HardKeep || hard, k.Viewed || viewed
		}
	}
	for i := range signals {
		s := &signals[i]
		f := score.Facts{}
		if p := facts[s.ID]; p != nil {
			f = *p
		}
		// A stream signal's streams are also read through every
		// overlapping selector (streamOverlaps): those readers protect
		// it too. A sweep still keeps nothing.
		for _, o := range overlaps[s.ID] {
			if p := facts[o]; p != nil {
				f.Read = f.Read || p.Read
				f.HardKeep, f.Viewed = f.HardKeep || p.HardKeep, f.Viewed || p.Viewed
			}
		}
		if s.Kind == graph.KindLabel {
			if k := resultKept[ResultKeptMetric(*s)]; k != nil {
				f.Read = true
				f.HardKeep, f.Viewed = f.HardKeep || k.HardKeep, f.Viewed || k.Viewed
			}
		}
		f.Name = s.Name
		if s.Kind == graph.KindLabel {
			f.Name = s.Parent
		}
		if s.Kind == graph.KindMetric || s.Kind == graph.KindLabel {
			f.Mentioned = mentions[f.Name]
		}
		f.Tier = -1
		if r := or.resolve(*s); r.name != "" {
			s.Owner, s.OwnerSource, s.OwnerInferred = r.name, r.source, r.inferred
			f.Seasonal = missed[r.name]
			if o := r.cfg; o != nil && o.Tier != nil {
				t := *o.Tier
				s.Tier, f.Tier = &t, t
			}
		}
		class, why := score.Classify(f)
		s.Class, s.Dormant = string(class), why
	}
	return nil
}

// ResultKeptMetric is the ID of a label signal's metric: references to it
// with ResultLabels carry the label into their result.
func ResultKeptMetric(s graph.Signal) string {
	return s.Source + ":metric:" + s.Parent
}

func isDashboard(k graph.ConsumerKind) bool {
	return k == graph.ConsumerDashboardPanel || k == graph.ConsumerDashboardVariable || k == graph.ConsumerDashboardAnnotation
}

// ownerMatcher finds a signal's owner: the first tvt.yaml owner whose
// metrics regex matches the metric (a label's parent), or whose stream
// selector's matchers are implied by the stream signal's selector.
type ownerMatcher struct {
	owners  []config.Owner
	metrics [][]*regexp.Regexp
	streams [][][]parse.Matcher
}

func newOwnerMatcher(owners []config.Owner) (*ownerMatcher, error) {
	m := &ownerMatcher{owners: owners}
	for _, o := range owners {
		var res []*regexp.Regexp
		for _, expr := range o.Metrics {
			re, err := config.MetricRegexp(expr)
			if err != nil {
				return nil, fmt.Errorf("scan: owner %q: %w", o.Name, err)
			}
			res = append(res, re)
		}
		var sels [][]parse.Matcher
		for _, sel := range o.Streams {
			ms, err := config.StreamMatchers(sel)
			if err != nil {
				return nil, fmt.Errorf("scan: owner %q: %w", o.Name, err)
			}
			sels = append(sels, ms)
		}
		m.metrics = append(m.metrics, res)
		m.streams = append(m.streams, sels)
	}
	return m, nil
}

func (m *ownerMatcher) match(s graph.Signal) *config.Owner {
	if len(m.owners) == 0 {
		return nil
	}
	var streamMatchers []parse.Matcher
	if s.Kind == graph.KindStream && s.Name != AllStreams {
		if refs, err := parse.LogQL(s.Name); err == nil && len(refs) == 1 {
			streamMatchers = refs[0].Matchers
		}
	}
	for i := range m.owners {
		switch s.Kind {
		case graph.KindMetric, graph.KindLabel:
			name := s.Name
			if s.Kind == graph.KindLabel {
				name = s.Parent
			}
			for _, re := range m.metrics[i] {
				if re.MatchString(name) {
					return &m.owners[i]
				}
			}
		case graph.KindStream:
			if streamMatchers == nil {
				continue
			}
			for _, sel := range m.streams[i] {
				if selectorImplies(streamMatchers, sel) {
					return &m.owners[i]
				}
			}
		}
	}
	return nil
}

// selectorImplies reports whether every stream selected by signal also
// matches owner. It proves constraints from an identical matcher or from an
// equality matcher on the same label; other implications stay unknown.
func selectorImplies(signal, owner []parse.Matcher) bool {
	for _, want := range owner {
		proven := false
		for _, have := range signal {
			if have.Label != want.Label {
				continue
			}
			if have.Op == parse.MatchEqual && matcherMatches(want, have.Value) ||
				have.Op == want.Op && have.Value == want.Value {
				proven = true
				break
			}
		}
		if !proven {
			return false
		}
	}
	return true
}

func matcherMatches(m parse.Matcher, value string) bool {
	switch m.Op {
	case parse.MatchEqual:
		return value == m.Value
	case parse.MatchNotEqual:
		return value != m.Value
	case parse.MatchRegexp, parse.MatchNotRegex:
		re, err := regexp.Compile("^(?:" + m.Value + ")$")
		return err == nil && re.MatchString(value) == (m.Op == parse.MatchRegexp)
	default:
		return false
	}
}

// Mention is an incident write-up's metric-shaped names (parse.Mentions).
type Mention struct {
	// Source says where, as the dormant reason shows it: "postmortem
	// <path>" or "runbook <url>".
	Source string
	Names  []string
}

// mentioned maps each metric name a runbook or postmortem names to the
// first (sorted) source naming it.
func mentioned(in Inputs) map[string]string {
	all := slices.Clone(in.Mentions)
	for _, rb := range in.Runbooks {
		if len(rb.Mentions) > 0 {
			all = append(all, Mention{Source: "runbook " + rb.URL, Names: rb.Mentions})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Source < all[j].Source })
	out := map[string]string{}
	for _, m := range all {
		for _, n := range m.Names {
			if _, ok := out[n]; !ok {
				out[n] = m.Source
			}
		}
	}
	return out
}

// seasons returns a source per season, and for each service the first
// (sorted) season the window [since, until] misses. A season is seen when
// the window touches any of its months; one it misses couldn't show its
// readers, so it is a gap (coverage 0) and its services' unread signals
// are dormant.
func seasons(ss []config.Season, since, until time.Time) ([]graph.SourceCoverage, map[string]string) {
	touched := map[time.Month]bool{}
	for t := time.Date(since.Year(), since.Month(), 1, 0, 0, 0, 0, time.UTC); !t.After(until); t = t.AddDate(0, 1, 0) {
		touched[t.Month()] = true
	}
	sorted := slices.Clone(ss)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var out []graph.SourceCoverage
	missed := map[string]string{}
	for _, se := range sorted {
		src := graph.SourceCoverage{Source: SourceSeasonPrefix + se.Name, Configured: true, Coverage: 1, WindowStart: since}
		seen := false
		for _, m := range se.Months {
			seen = seen || touched[time.Month(m)]
		}
		if !seen {
			names := make([]string, len(se.Months))
			for i, m := range se.Months {
				names[i] = time.Month(m).String()[:3]
			}
			src.Coverage = 0
			src.Gap = fmt.Sprintf("runs in %s, outside the window: readers of %s can't show", strings.Join(names, ", "), strings.Join(se.Services, ", "))
			for _, svc := range se.Services {
				if _, ok := missed[svc]; !ok {
					missed[svc] = se.Name
				}
			}
		}
		out = append(out, src)
	}
	return out, missed
}
