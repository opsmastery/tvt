package check

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// index is the manifest arranged for evaluation.
type index struct {
	byPipeline map[string][]report.ProtectedSignal
	// spans and logEvents pick out record spans and log events.
	spans, logEvents []parse.OTTL
	matchErrs        []string
}

func newIndex(m report.Manifest) *index {
	ix := &index{byPipeline: map[string][]report.ProtectedSignal{}}
	for _, s := range m.Signals {
		ix.byPipeline[s.Pipeline] = append(ix.byPipeline[s.Pipeline], s)
	}
	for _, c := range m.RecordMatches.Spans {
		if o, err := parse.OTTLCondition(c); err == nil {
			ix.spans = append(ix.spans, o)
		} else {
			ix.matchErrs = append(ix.matchErrs, fmt.Sprintf("records.spans %q: %v", c, err))
		}
	}
	for _, c := range m.RecordMatches.LogEvents {
		if o, err := parse.OTTLCondition(c); err == nil {
			ix.logEvents = append(ix.logEvents, o)
		} else {
			ix.matchErrs = append(ix.matchErrs, fmt.Sprintf("records.log_events %q: %v", c, err))
		}
	}
	return ix
}

func (ix *index) of(pipeline, kind string) []report.ProtectedSignal {
	var out []report.ProtectedSignal
	for _, s := range ix.byPipeline[pipeline] {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// why is how a finding names what protects a signal.
func why(s report.ProtectedSignal) string {
	if s.Record != "" {
		return "a record (" + s.Record + ")"
	}
	var ps []string
	for i, p := range s.Protectors {
		if i == 3 {
			ps = append(ps, fmt.Sprintf("%d more", len(s.Protectors)-3))
			break
		}
		ps = append(ps, strings.TrimSpace(p.Kind+" "+p.Name))
	}
	if len(ps) == 0 {
		return s.Class
	}
	return s.Class + ": " + strings.Join(ps, ", ")
}

func ruleFor(s report.ProtectedSignal) string {
	if s.Record != "" {
		return RuleRecord
	}
	return RuleProtected
}

func describe(s report.ProtectedSignal) string {
	if s.Kind == "label" {
		return "label " + s.Name + " on " + s.Parent
	}
	return s.Name
}

// Stream selectors: a protected stream signal is a selector; a pipeline
// step selects with another. mayOverlap proves two disjoint only through
// an equality matcher the other's matchers on that label reject.
type matcher struct {
	parse.Matcher
	re *regexp.Regexp
}

func selector(s string) ([]matcher, bool) {
	refs, err := parse.LogQL(s)
	if err != nil || len(refs) != 1 {
		return nil, false
	}
	var out []matcher
	for _, m := range refs[0].Matchers {
		c := matcher{Matcher: m}
		if m.Op == parse.MatchRegexp || m.Op == parse.MatchNotRegex {
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			if err != nil {
				return nil, false
			}
			c.re = re
		}
		out = append(out, c)
	}
	return out, true
}

func (m matcher) matches(v string) bool {
	switch m.Op {
	case parse.MatchEqual:
		return v == m.Value
	case parse.MatchNotEqual:
		return v != m.Value
	case parse.MatchRegexp:
		return m.re.MatchString(v)
	}
	return !m.re.MatchString(v)
}

func mayOverlap(a, b []matcher) bool {
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

// streamLevel is the level a stream selector pins, if one equality or
// regex matcher on level/severity/detected_level does.
var warnAndAbove = regexp.MustCompile(`(?i)^(warn|warning|error|err|fatal|critical|crit|alert|emerg|panic)$`)

// selectsWarnPlus: the selector can select a warn-and-above stream (or
// pins no level at all).
func selectsWarnPlus(ms []matcher) bool {
	for _, m := range ms {
		switch m.Label {
		case "level", "severity", "detected_level", "severity_text":
			for _, l := range []string{"warn", "warning", "error", "fatal", "critical"} {
				if m.matches(l) {
					return true
				}
			}
			return false
		}
	}
	return true
}

// tvtBlocks finds tvt's own blocks by line: tvt:begin <id> ... tvt:end.
type blockSpan struct {
	id         string
	start, end int // 1-based lines, inclusive
}

func tvtBlocks(src []byte) []blockSpan {
	var out []blockSpan
	var open *blockSpan
	for i, line := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(line)
		t = strings.TrimLeft(t, "#/ ")
		switch {
		case strings.HasPrefix(t, "tvt:begin "):
			f := strings.Fields(strings.TrimPrefix(t, "tvt:begin "))
			if len(f) > 0 {
				open = &blockSpan{id: f[0], start: i + 1}
			}
		case strings.HasPrefix(t, "tvt:end") && open != nil:
			open.end = i + 1
			out = append(out, *open)
			open = nil
		}
	}
	return out
}

func blockAt(bs []blockSpan, line int) string {
	for _, b := range bs {
		if line >= b.start && line <= b.end {
			return b.id
		}
	}
	return ""
}
