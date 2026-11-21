package check

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// index is the manifest arranged for evaluation.
type index struct {
	byPipeline map[string][]report.ProtectedSignal
	// read are unprotected signals with readers, by pipeline (S8 item 3).
	read map[string][]report.ReadSignal
	// spans and logEvents pick out record spans and log events.
	spans, logEvents []parse.OTTL
	matchErrs        []string
	// dormantTenant is the retention tier's Mimir tenant (S10 item 2).
	dormantTenant string
}

func newIndex(m report.Manifest) *index {
	ix := &index{byPipeline: map[string][]report.ProtectedSignal{}, read: map[string][]report.ReadSignal{}, dormantTenant: m.DormantTenant}
	for _, s := range m.Signals {
		ix.byPipeline[s.Pipeline] = append(ix.byPipeline[s.Pipeline], s)
	}
	for _, s := range m.Read {
		ix.read[s.Pipeline] = append(ix.read[s.Pipeline], s)
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

// The error-pattern guard (S8 item 2): a log rule must keep lines that
// say error, exception, traceback, critical or fatal, in case a stream's
// level label is wrong.
var guardWords = []string{"ERROR", "Exception", "Traceback", "CRITICAL", "fatal"}

// guardedRegex: a regex that matches every guard word (the lines a
// negated filter keeps).
func guardedRegex(re string) bool {
	r, err := regexp.Compile(re)
	if err != nil {
		return false
	}
	for _, w := range guardWords {
		if !r.MatchString("x " + w + " y") {
			return false
		}
	}
	return true
}

// lineFilterGuard: a LogQL selector with a !~ line filter whose regex
// matches every guard word.
var negatedFilter = regexp.MustCompile(`!~\s*"((?:[^"\\]|\\.)*)"`)

func lineFilterGuard(sel string) bool {
	for _, m := range negatedFilter.FindAllStringSubmatch(sel, -1) {
		re, err := strconvUnquote(`"` + m[1] + `"`)
		if err == nil && guardedRegex(re) {
			return true
		}
	}
	return false
}

// Level filters (S10 item 3): a line filter that takes only lines naming
// a level below warn. Judged on samples, as the guard is: it must match a
// debug or info line and none of the warn-and-above or unlevelled ones.
var (
	lowLevelLines = []string{"ts=1 level=debug msg=x", `{"level":"info","msg":"ok"}`}
	warnPlusLines = []string{"level=warn x", "level=WARNING x", `{"level":"error"}`, "lvl=fatal x", "severity=critical x", "WARN bare word", "starting worker", "level=information x",
		`level=warn msg="debug mode, info only"`, "WARN debug info", `{"level":"error","msg":"info"}`}
	positiveFilter = regexp.MustCompile(`\|~\s*"((?:[^"\\]|\\.)*)"`)
)

func levelOnlyRegex(re string) bool {
	r, err := regexp.Compile(re)
	if err != nil {
		return false
	}
	low := false
	for _, l := range lowLevelLines {
		low = low || r.MatchString(l)
	}
	for _, l := range warnPlusLines {
		if r.MatchString(l) {
			return false
		}
	}
	return low
}

// lineFilterLevel: a LogQL selector with a |~ line filter that is a level
// filter.
func lineFilterLevel(sel string) bool {
	for _, m := range positiveFilter.FindAllStringSubmatch(sel, -1) {
		re, err := strconvUnquote(`"` + m[1] + `"`)
		if err == nil && levelOnlyRegex(re) {
			return true
		}
	}
	return false
}

// lookaheadLevel: a Fluent Bit rule whose regex has a positive lookahead
// (?=.*X) with X a level filter.
func lookaheadLevel(rule string) bool {
	for rest := rule; ; {
		i := strings.Index(rest, "(?=.*")
		if i < 0 {
			return false
		}
		rest = rest[i+len("(?=.*"):]
		depth, end := 1, -1
		for j := 0; j < len(rest) && end < 0; j++ {
			switch {
			case rest[j] == '\\':
				j++
			case rest[j] == '(':
				depth++
			case rest[j] == ')':
				if depth--; depth == 0 {
					end = j
				}
			}
		}
		if end > 0 && levelOnlyRegex(rest[:end]) {
			return true
		}
	}
}

// textGuard: a rule's text (a Fluent Bit regex, an OTTL condition) names
// every guard word in a negated form: a negative lookahead or not IsMatch.
func textGuard(s string) bool {
	if !strings.Contains(s, "(?!") && !strings.Contains(s, "not IsMatch") {
		return false
	}
	low := strings.ToLower(s)
	for _, w := range guardWords {
		if !strings.Contains(low, strings.ToLower(w)) {
			return false
		}
	}
	return true
}

var strconvUnquote = strconv.Unquote

// lessLossy names the less lossy action for a read signal a change drops
// (ADR 0010's order), for a least_lossy finding.
func lessLossy(s report.ReadSignal) string {
	if s.Kind == "stream" {
		return fmt.Sprintf("%s has %d readers: cap, collapse, keep warn-and-above only or sample it before rerouting or dropping it (ADR 0010)", s.Name, s.Readers)
	}
	if !s.AllLabels {
		if len(s.Labels) == 0 {
			return fmt.Sprintf("%s has %d readers that use no label: aggregate its labels away rather than drop it (ADR 0010)", s.Name, s.Readers)
		}
		return fmt.Sprintf("%s has %d readers, which use only %s: remove the other labels rather than drop it (ADR 0010)", s.Name, s.Readers, strings.Join(s.Labels, ", "))
	}
	return fmt.Sprintf("%s has %d readers: lengthen its scrape interval, shorten its retention or reroute it to the archive before dropping it (ADR 0010)", s.Name, s.Readers)
}
