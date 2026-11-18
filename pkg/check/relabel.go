package check

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// relabelRule is one metric relabel rule, from Prometheus YAML, a
// monitor's metricRelabelings or an Alloy rule block, in Prometheus'
// semantics: regex fully anchored, source values joined with Separator.
type relabelRule struct {
	Source      []string
	Separator   string
	Regex       *regexp.Regexp
	RegexText   string
	Action      string
	Target      string
	Replacement string
	Line        int
	// Unreadable: a field tvt can't take as a literal (an Alloy
	// expression): the rule's effect is unknown.
	Unreadable string
}

func newRule(source []string, sep, regex, action, target, replacement string, line int) (relabelRule, error) {
	if sep == "" {
		sep = ";"
	}
	if regex == "" {
		regex = "(.*)"
	}
	if action == "" {
		action = "replace"
	}
	if replacement == "" && action == "replace" {
		replacement = "$1"
	}
	re, err := regexp.Compile("^(?s:" + regex + ")$")
	if err != nil {
		return relabelRule{}, fmt.Errorf("line %d: regex %q: %v", line, regex, err)
	}
	return relabelRule{Source: source, Separator: sep, Regex: re, RegexText: regex, Action: strings.ToLower(action),
		Target: target, Replacement: replacement, Line: line}, nil
}

// drops: the rule removes series or labels at all (it needs an owner's
// confirmation, and it's what least-lossy and gap rules look at).
func (r relabelRule) drops() bool {
	switch r.Action {
	case "drop", "keep", "labeldrop", "labelkeep", "dropequal", "keepequal":
		return true
	}
	return false
}

func (r relabelRule) text() string {
	switch r.Action {
	case "labeldrop", "labelkeep":
		return fmt.Sprintf("%s %q", r.Action, r.RegexText)
	}
	return fmt.Sprintf("%s on [%s] %q", r.Action, strings.Join(r.Source, ","), shorten(r.RegexText, 80))
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// relabelOutcome is what a rule list does to one protected signal.
type relabelOutcome struct {
	dropped, labelGone parse.Tri
	by                 relabelRule
}

// evalRelabel runs rules over a metric's known labels: __name__, job when
// given, and the protected label (present, value unknown). Other labels
// are unknown. A rule that needs an unknown value may or may not act.
func evalRelabel(rules []relabelRule, metric, job, label string) relabelOutcome {
	type val struct {
		v     string
		known bool
	}
	ls := map[string]val{"__name__": {metric, true}}
	if job != "" {
		ls["job"] = val{job, true}
	}
	if label != "" {
		ls[label] = val{"", false}
	}
	get := func(l string) val {
		if v, ok := ls[l]; ok {
			return v
		}
		return val{"", false}
	}
	out := relabelOutcome{dropped: parse.False, labelGone: parse.False}
	worse := func(cur parse.Tri, t parse.Tri) parse.Tri {
		if cur == parse.True || t == parse.True {
			return parse.True
		}
		if cur == parse.Unknown || t == parse.Unknown {
			return parse.Unknown
		}
		return parse.False
	}
	for _, r := range rules {
		if r.Unreadable != "" {
			if r.drops() || r.Target == "__name__" || r.Target == label {
				out.dropped, out.by = worse(out.dropped, parse.Unknown), r
			}
			continue
		}
		src, known := "", true
		for i, s := range r.Source {
			v := get(s)
			if !v.known {
				known = false
			}
			if i > 0 {
				src += r.Separator
			}
			src += v.v
		}
		switch r.Action {
		case "drop", "keep":
			var hit parse.Tri = parse.Unknown
			if known {
				hit = parse.False
				if r.Regex.MatchString(src) == (r.Action == "drop") {
					hit = parse.True
				}
			}
			if hit != parse.False {
				out.dropped, out.by = worse(out.dropped, hit), r
				if hit == parse.True {
					return out
				}
			}
		case "dropequal", "keepequal":
			out.dropped, out.by = worse(out.dropped, parse.Unknown), r
		case "labeldrop", "labelkeep":
			if label == "" {
				continue
			}
			if r.Regex.MatchString(label) == (r.Action == "labeldrop") {
				out.labelGone, out.by = worse(out.labelGone, parse.True), r
			}
		case "replace":
			if r.Target == "" {
				continue
			}
			if !known {
				if r.Target == "__name__" || r.Target == label {
					ls[r.Target] = val{"", false}
				}
				continue
			}
			m := r.Regex.FindStringSubmatchIndex(src)
			if m == nil {
				continue
			}
			v := string(r.Regex.ExpandString(nil, r.Replacement, src, m))
			if r.Target == label && v == "" {
				out.labelGone, out.by = worse(out.labelGone, parse.True), r
			}
			ls[r.Target] = val{v, true}
		case "labelmap":
			if label != "" && r.Regex.MatchString(label) {
				out.labelGone, out.by = worse(out.labelGone, parse.Unknown), r
			}
		default: // hashmod, lowercase, uppercase: a target value tvt doesn't compute
			if r.Target != "" {
				ls[r.Target] = val{"", false}
			}
		}
	}
	if v := get("__name__"); !v.known || v.v != metric {
		// Renamed: the stored name is gone, as for a drop.
		t := parse.Unknown
		if v.known {
			t = parse.True
		}
		out.dropped = worse(out.dropped, t)
	}
	return out
}

// relabelEffects judges one rule list against a pipeline's protected
// metrics and labels. jobs, when set, are the scrape configs the list
// belongs to: a signal whose jobs the manifest knows is judged only
// there.
func relabelEffects(p report.PipelineDef, path, component string, rules []relabelRule, jobs []string, ix *index, blocks []blockSpan) []effect {
	var out []effect
	for _, r := range rules {
		if r.drops() {
			out = append(out, effect{rule: ruleDropMarker, pipeline: p.Name, file: path, component: component,
				reason: r.text(), at: fmt.Sprintf("line %d", r.Line), block: blockAt(blocks, r.Line)})
		}
	}
	judge := func(s report.ProtectedSignal, metric, label string) {
		js := commonJobs(jobs, s.Jobs)
		if jobs != nil && s.Jobs != nil && len(js) == 0 {
			return // the signal doesn't come through this scrape config
		}
		if len(js) == 0 {
			js = []string{""}
		}
		for _, j := range js {
			o := evalRelabel(rules, metric, j, label)
			t, what := o.dropped, "drops"
			if label != "" && o.dropped == parse.False {
				t, what = o.labelGone, "removes"
			}
			if t == parse.False {
				continue
			}
			may := ""
			if t == parse.Unknown {
				may = "may "
				what = strings.TrimSuffix(what, "s")
			}
			out = append(out, effect{rule: ruleFor(s), pipeline: p.Name, file: path, component: component, signal: s.ID,
				reason: fmt.Sprintf("%s%s %s, %s (%s)", may, what, describe(s), why(s), o.by.text()),
				at:     fmt.Sprintf("line %d", o.by.Line), block: blockAt(blocks, o.by.Line)})
			return
		}
	}
	for _, s := range ix.of(p.Name, "metric") {
		judge(s, s.Name, "")
	}
	// Read but unprotected metrics: a drop is refused when a less lossy
	// action keeps their readers (S8 item 3).
	for _, s := range ix.read[p.Name] {
		if s.Kind != "metric" {
			continue
		}
		js := commonJobs(jobs, s.Jobs)
		if jobs != nil && s.Jobs != nil && len(js) == 0 {
			continue
		}
		if len(js) == 0 {
			js = []string{""}
		}
		for _, j := range js {
			o := evalRelabel(rules, s.Name, j, "")
			if o.dropped == parse.False {
				continue
			}
			out = append(out, effect{rule: RuleLeastLossy, pipeline: p.Name, file: path, component: component, signal: s.ID,
				reason: lessLossy(s) + " (" + o.by.text() + ")", at: fmt.Sprintf("line %d", o.by.Line), block: blockAt(blocks, o.by.Line)})
			break
		}
	}
	for _, s := range ix.of(p.Name, "label") {
		judge(s, s.Parent, s.Name)
	}
	return out
}

func commonJobs(a, b []string) []string {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	var out []string
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
			}
		}
	}
	return out
}
