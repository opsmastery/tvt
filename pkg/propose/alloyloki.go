package propose

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/grafana/alloy/syntax/ast"
	"github.com/grafana/alloy/syntax/parser"
)

// Alloy stream reroutes (S7 item 3, ADR 0009 for Alloy). Alloy has no
// per-stream route inside loki.process, so tvt splits the stream off
// where the loki.process that ships to Loki forwards it: that
// component's forward_to points at two components tvt owns. tvt_hot (a
// loki.process) drops the stream's lines except the ones matching the
// error pattern (S8 item 2: in case a stream's level label is wrong) and
// forwards everything else to the original targets; tvt_cold (a
// loki.relabel) keeps only the stream and forwards it to the cold
// component pipelines[].cold names. The original forward_to line is kept
// in the block, and stripping the block puts it back.

const (
	alloyHot  = "tvt_hot"
	alloyCold = "tvt_cold"
)

// ErrorPattern is the error-pattern guard (S8 item 2): every log rule tvt
// writes keeps lines matching it, in case a stream's level label is wrong.
const ErrorPattern = "(?i)error|exception|traceback|critical|fatal"

// AlloyRoutes reroutes each route's stream to its cold component (r.Cold
// is a component name such as loki.write.cold, which must exist in the
// file). Existing tvt blocks are removed first. It fails rather than
// guess: no loki.process forwarding to a loki.write, more than one, a
// forward_to that isn't one line of component references, or a cold
// component that isn't in the file. All routes must share one cold
// component.
func AlloyRoutes(src []byte, routes []Route) ([]byte, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("propose: no route")
	}
	cold := routes[0].Cold
	for _, r := range routes {
		if r.ID == "" || strings.ContainsAny(r.ID+r.Reason, "\n\r") || len(r.Stream) == 0 || (r.Cold == "") == (r.SampleOneIn < 2) {
			return nil, fmt.Errorf("propose: route needs a one-line ID and reason, a stream, and a cold component or a sample rate of 1 in 2 or more")
		}
		if r.Cold != cold {
			return nil, fmt.Errorf("propose: routes name different cold components (%q, %q), or mix reroutes and samples", cold, r.Cold)
		}
	}
	out, _, err := stripBlocks(src, "//")
	if err != nil {
		return nil, err
	}
	return AlloyRoutesKeep(out, routes)
}

// AlloyRoutesKeep is AlloyRoutes on a config whose tvt blocks were just
// written (by Alloy, in the same pass): it removes none.
func AlloyRoutesKeep(out []byte, routes []Route) ([]byte, error) {
	if len(routes) == 0 {
		return out, nil
	}
	cold := routes[0].Cold
	f, err := parser.ParseFile("config.alloy", out)
	if err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	ids := map[string]*ast.BlockStmt{}
	var shippers []*ast.BlockStmt
	for _, st := range f.Body {
		b, ok := st.(*ast.BlockStmt)
		if !ok {
			continue
		}
		id := strings.Join(b.Name, ".")
		if b.Label != "" {
			id += "." + b.Label
		}
		ids[id] = b
	}
	for _, st := range f.Body {
		b, ok := st.(*ast.BlockStmt)
		if !ok || strings.Join(b.Name, ".") != "loki.process" {
			continue
		}
		for _, t := range alloyTargets(b.Body) {
			if strings.HasPrefix(t, "loki.write.") {
				shippers = append(shippers, b)
				break
			}
		}
	}
	switch {
	case len(shippers) == 0:
		return nil, fmt.Errorf("propose: no loki.process forwards to a loki.write")
	case len(shippers) > 1:
		return nil, fmt.Errorf("propose: more than one loki.process forwards to a loki.write; tvt won't guess which carries the stream")
	}
	sample := cold == ""
	for _, r := range routes {
		if (r.Cold == "") != sample {
			return nil, fmt.Errorf("propose: routes mix reroutes and samples")
		}
	}
	if _, ok := ids[cold]; !ok && !sample {
		return nil, fmt.Errorf("propose: no component %s for cold streams", cold)
	}
	ship := shippers[0]
	var fwd *ast.AttributeStmt
	for _, st := range ship.Body {
		if a, ok := st.(*ast.AttributeStmt); ok && a.Name.Name == "forward_to" {
			fwd = a
		}
	}
	if fwd == nil {
		return nil, fmt.Errorf("propose: loki.process %q has no forward_to", ship.Label)
	}
	targets, err := forwardTo(fwd.Value)
	if err != nil {
		return nil, fmt.Errorf("propose: loki.process %q: %w", ship.Label, err)
	}
	line := ast.StartPos(fwd).Position().Line
	if ast.EndPos(fwd).Position().Line != line {
		return nil, fmt.Errorf("propose: loki.process %q: forward_to spans lines; write it on one line", ship.Label)
	}
	lines := strings.SplitAfter(string(out), "\n")
	orig := strings.TrimRight(lines[line-1], "\n")
	indent := orig[:len(orig)-len(strings.TrimLeft(orig, " \t"))]
	begin := routes[0].begin()
	// 1. The shipper's forward_to, replaced inside a block that keeps the
	// original line.
	newFwd := fmt.Sprintf("loki.process.%s.receiver, loki.relabel.%s.receiver", alloyHot, alloyCold)
	if sample {
		newFwd = fmt.Sprintf("loki.process.%s.receiver", alloyHot)
	}
	replaced := fmt.Sprintf("%s// %s\n%s// %s%s\n%sforward_to = [%s]\n%s// %s\n",
		indent, begin, indent, originalMarker, strings.TrimSpace(orig), indent, newFwd, indent, endMarker)
	lines[line-1] = replaced
	out = []byte(strings.Join(lines, ""))
	// 2. The two relabel components, at the end of the file.
	var b strings.Builder
	if !strings.HasSuffix(string(out), "\n") {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "// %s\n", begin)
	for _, r := range routes {
		if r.Reason != "" {
			fmt.Fprintf(&b, "// %s: %s\n", fluentLabelString(r.Stream), r.Reason)
		}
	}
	fmt.Fprintf(&b, "loki.process %q {\n", alloyHot)
	for _, r := range routes {
		sel := streamSelector(r.Stream) + " !~ " + strconv.Quote(ErrorPattern)
		if r.BelowWarn {
			// A level tier (S10 item 3): only lines naming a level below
			// warn leave the hot side.
			sel += " |~ " + strconv.Quote(LevelLinePattern)
		}
		if r.SampleOneIn > 1 {
			// S10 item 4: keep one line in N, and every error-pattern line
			// (the selector's !~ leaves them outside the match).
			fmt.Fprintf(&b, "  stage.match {\n    selector = %s\n    stage.sampling {\n      rate = %s\n    }\n  }\n",
				strconv.Quote(sel), strconv.FormatFloat(1/float64(r.SampleOneIn), 'g', 6, 64))
			continue
		}
		fmt.Fprintf(&b, "  stage.match {\n    selector = %s\n    action   = \"drop\"\n  }\n", strconv.Quote(sel))
	}
	fmt.Fprintf(&b, "  forward_to = [%s]\n}\n", strings.Join(receivers(targets), ", "))
	if sample {
		fmt.Fprintf(&b, "// %s\n", endMarker)
		out = append(out, []byte(b.String())...)
		if _, err := parser.ParseFile("config.alloy", out); err != nil {
			return nil, fmt.Errorf("propose: the edited config doesn't parse: %w", err)
		}
		return out, nil
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "loki.relabel %q {\n", alloyCold)
	// keep rules AND together, so one keep per stream would keep nothing
	// when there are several: the cold side keeps any of the streams with
	// one rule over the joined labels of each.
	keys, regex := streamsRegex(routes)
	fmt.Fprintf(&b, "  rule {\n    source_labels = [%s]\n    separator     = \";\"\n    regex         = %s\n    action        = \"keep\"\n  }\n", quoteList(keys), strconv.Quote(regex))
	fmt.Fprintf(&b, "  forward_to = [%s.receiver]\n}\n// %s\n", cold, endMarker)
	out = append(out, []byte(b.String())...)
	if _, err := parser.ParseFile("config.alloy", out); err != nil {
		return nil, fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	return out, nil
}

// streamSelector is a LogQL selector naming exactly the stream's labels.
func streamSelector(stream map[string]string) string {
	keys := slices.Sorted(maps.Keys(stream))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + strconv.Quote(stream[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// streamsRegex is one keep rule's labels (every label any stream has) and
// a regex matching any of the streams over them (a stream lacking a label
// matches it empty).
func streamsRegex(routes []Route) ([]string, string) {
	set := map[string]bool{}
	for _, r := range routes {
		for k := range r.Stream {
			set[k] = true
		}
	}
	keys := slices.Sorted(maps.Keys(set))
	var alts []string
	for _, r := range routes {
		vals := make([]string, len(keys))
		for i, k := range keys {
			vals[i] = regexp.QuoteMeta(r.Stream[k])
		}
		alts = append(alts, strings.Join(vals, ";"))
	}
	slices.Sort(alts)
	if len(alts) == 1 {
		return keys, alts[0]
	}
	return keys, "(?:" + strings.Join(alts, "|") + ")"
}

func quoteList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}

func receivers(targets []string) []string {
	out := make([]string, len(targets))
	copy(out, targets)
	return out
}
