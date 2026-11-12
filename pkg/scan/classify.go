package scan

import (
	"fmt"
	"regexp"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
)

// classify sets every signal's class, owner and tier (Sprint 2 item 6).
//
//   - hard_keep: a hard-keep consumer (alert rule, Grafana-managed alert,
//     SLO, external consumer) references it, directly or through a
//     recording rule (chains give the alert a reference with Via set).
//   - keep_high: a dashboard viewed in the window references it.
//   - dormant: no consumer references it, and its name or its owner's
//     tier says it may be needed in an incident.
//   - candidate: everything else, ranked by score.Score.
//
// A label signal takes its metric's owner, tier and name rules; a stream
// signal's name rules run over its selector.
func classify(signals []graph.Signal, consumers []graph.Consumer, refs []graph.Reference, owners []config.Owner, since, until time.Time) error {
	om, err := newOwnerMatcher(owners)
	if err != nil {
		return err
	}
	byID := make(map[string]graph.Consumer, len(consumers))
	for _, c := range consumers {
		byID[c.ID] = c
	}
	facts := map[string]*score.Facts{}
	for _, r := range refs {
		f := facts[r.SignalID]
		if f == nil {
			f = &score.Facts{}
			facts[r.SignalID] = f
		}
		c := byID[r.ConsumerID]
		f.Read = true
		f.HardKeep = f.HardKeep || c.Kind.HardKeep()
		if isDashboard(c.Kind) && !c.LastViewed.IsZero() && !c.LastViewed.Before(since) && !c.LastViewed.After(until) {
			f.Viewed = true
		}
	}
	for i := range signals {
		s := &signals[i]
		f := score.Facts{}
		if p := facts[s.ID]; p != nil {
			f = *p
		}
		f.Name = s.Name
		if s.Kind == graph.KindLabel {
			f.Name = s.Parent
		}
		f.Tier = -1
		if o := om.match(*s); o != nil {
			s.Owner = o.Name
			if o.Tier != nil {
				t := *o.Tier
				s.Tier, f.Tier = &t, t
			}
		}
		class, why := score.Classify(f)
		s.Class, s.Dormant = string(class), why
	}
	return nil
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
