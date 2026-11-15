package scan

import (
	"regexp"
	"sort"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// StreamOverlaps maps each stream signal to the other stream signals whose
// selectors select at least one stream in common with it. Rerouting a
// stream signal moves every stream it selects, so whoever reads one of
// those streams through another selector reads it too: a viewed panel on
// {app="shop", level=~"warn|error"} reads the lines of
// {app="shop", component="api", level=~"warn|error"}.
//
// streams are every stream's full label set, from the volume API.
// A selector that selects none of them, or every selector when there are
// none, is compared by its matchers instead: two selectors overlap
// unless one label's matchers can't both hold. A selector tvt can't read
// (loki:stream:*, a parse failure, a regex Go won't compile) overlaps
// every stream signal: unknown means used.
func StreamOverlaps(signals []graph.Signal, streams []map[string]string) map[string][]string {
	type sel struct {
		id string
		ms []compiledMatcher
		ok bool // false: unreadable, overlaps everything
	}
	var sels []sel
	for _, s := range signals {
		if s.Kind != graph.KindStream {
			continue
		}
		x := sel{id: s.ID}
		if s.Name != AllStreams {
			if refs, err := parse.LogQL(s.Name); err == nil && len(refs) == 1 && len(refs[0].Matchers) > 0 {
				x.ms, x.ok = compileMatchers(refs[0].Matchers)
			}
		}
		sels = append(sels, x)
	}
	if len(sels) < 2 {
		return nil
	}
	pairs := map[[2]int]bool{}
	add := func(i, j int) {
		if i != j {
			pairs[[2]int{min(i, j), max(i, j)}] = true
		}
	}
	// Selectors that select a known stream, by stream.
	matchedAny := make([]bool, len(sels))
	for _, labels := range streams {
		var hit []int
		for i, x := range sels {
			if x.ok && matchesAll(x.ms, labels) {
				hit = append(hit, i)
				matchedAny[i] = true
			}
		}
		for a := range hit {
			for b := a + 1; b < len(hit); b++ {
				add(hit[a], hit[b])
			}
		}
	}
	for i, x := range sels {
		for j := i + 1; j < len(sels); j++ {
			y := sels[j]
			switch {
			case !x.ok || !y.ok:
				add(i, j)
			case (!matchedAny[i] || !matchedAny[j]) && mayOverlap(x.ms, y.ms):
				add(i, j)
			}
		}
	}
	out := map[string][]string{}
	for p := range pairs {
		a, b := sels[p[0]].id, sels[p[1]].id
		out[a] = append(out[a], b)
		out[b] = append(out[b], a)
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out
}

// compiledMatcher is a stream matcher with its regex compiled once.
type compiledMatcher struct {
	parse.Matcher
	re *regexp.Regexp
}

func compileMatchers(ms []parse.Matcher) ([]compiledMatcher, bool) {
	out := make([]compiledMatcher, len(ms))
	for i, m := range ms {
		out[i].Matcher = m
		switch m.Op {
		case parse.MatchEqual, parse.MatchNotEqual:
		case parse.MatchRegexp, parse.MatchNotRegex:
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			if err != nil {
				return nil, false
			}
			out[i].re = re
		default:
			return nil, false
		}
	}
	return out, true
}

func (m compiledMatcher) matches(v string) bool {
	switch m.Op {
	case parse.MatchEqual:
		return v == m.Value
	case parse.MatchNotEqual:
		return v != m.Value
	case parse.MatchRegexp:
		return m.re.MatchString(v)
	default: // MatchNotRegex
		return !m.re.MatchString(v)
	}
}

// matchesAll: every matcher holds for the label set (a missing label is "").
func matchesAll(ms []compiledMatcher, labels map[string]string) bool {
	for _, m := range ms {
		if !m.matches(labels[m.Label]) {
			return false
		}
	}
	return true
}

// mayOverlap reports whether some label set could satisfy both selectors.
// It proves them disjoint only through an equality matcher on a label
// that the other selector's matchers on that label reject; anything else
// may overlap.
func mayOverlap(a, b []compiledMatcher) bool {
	for _, x := range a {
		for _, y := range b {
			if x.Label != y.Label {
				continue
			}
			if x.Op == parse.MatchEqual && !y.matches(x.Value) || y.Op == parse.MatchEqual && !x.matches(y.Value) {
				return false
			}
		}
	}
	return true
}

// streamLabelSets are the label sets of the volume API's streams.
func streamLabelSets(vols []collect.StreamVolume) []map[string]string {
	out := make([]map[string]string, len(vols))
	for i, v := range vols {
		out[i] = v.Labels
	}
	return out
}

// StoredStreamLabels are the label sets of a stored scan's stream volumes
// (each selector is a full label set of equality matchers); a selector
// that doesn't parse that way is skipped.
func StoredStreamLabels(vols []graph.StreamVolume) []map[string]string {
	var out []map[string]string
	for _, v := range vols {
		refs, err := parse.LogQL(v.Selector)
		if err != nil || len(refs) != 1 {
			continue
		}
		labels := map[string]string{}
		for _, m := range refs[0].Matchers {
			if m.Op != parse.MatchEqual {
				labels = nil
				break
			}
			labels[m.Label] = m.Value
		}
		if labels != nil {
			out = append(out, labels)
		}
	}
	return out
}
