package propose

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Route is one generated reroute of a whole log stream to cold storage
// (S7 item 3, ADRs 0006 and 0009): what a writer puts into a pipeline's
// config, inside a marked block. tvt never cuts lines (ADR 0006).
type Route struct {
	ID       string
	Scan     int64
	Evidence string
	Reason   string
	// Stream is the stream's full label set, as Loki stores it.
	Stream map[string]string
	// Cold is the alias of the output that takes cold streams: one the
	// config already defines (tvt.yaml pipelines[].cold). tvt doesn't
	// invent a storage destination.
	Cold string
	// BelowWarn makes the route a level tier (S10 item 3): only the
	// stream's lines that name a level below warn move, by
	// LevelLinePattern; every other line stays hot.
	BelowWarn bool
	// SampleOneIn makes the route a sample instead (S10 item 4, Alloy
	// only): with no cold tier to reroute to, the stream keeps one line in
	// SampleOneIn, and every line matching the error pattern. Cold is
	// empty then. tvt.yaml tiers.sample_one_in turns it on.
	SampleOneIn int
}

// LevelLinePattern matches a line that names its level below warn as a
// key: logfmt level=debug, JSON "level":"info", lvl or severity too, any
// case. A line that names no level this way stays hot, so a level tier
// in a line-based pipeline moves no more than the line says (Loki's
// detected_level also guesses from bare words, so it may measure more).
// It doesn't parse quotes: a level key inside a quoted message counts.
const LevelLinePattern = `(?i)(?:^|[\s{,])"?(?:level|lvl|severity)"?\s*[=:]\s*"?(?:trace|debug|info)\b`

// fluentRegex is the rewrite_tag rule's regex: no line naming an error
// moves (S8 item 2), and a level tier moves only lines naming a level
// below warn. Onigmo, written without spaces (the rule is space-split).
func (r Route) fluentRegex() string {
	if r.BelowWarn {
		return `^(?!.*(?i:error|exception|traceback|critical|fatal))(?=.*(?i:` + strings.TrimPrefix(LevelLinePattern, "(?i)") + `)).*$`
	}
	return `^(?!.*(?i:error|exception|traceback|critical|fatal)).*$`
}

func (r Route) begin() string {
	return Drop{ID: r.ID, Scan: r.Scan, Evidence: r.Evidence}.begin()
}

// ColdTagPrefix starts the tag a rerouted stream's records get; the cold
// output's match must take it.
const ColdTagPrefix = "tvt.cold."

// FluentBit reroutes r.Stream to the cold output in a Fluent Bit YAML
// config's bytes: a rewrite_tag filter that retags the records of the
// stream's tag to tvt.cold.<tag> and keeps no copy, so they leave the
// Loki output that labels them and reach the cold output instead.
//
// It fails rather than guess: no Loki output whose static labels are
// exactly the stream's, an output that matches by wildcard or regex
// (other streams would move too), labels set from record fields, no
// output aliased r.Cold or one whose match doesn't take the new tag, or
// an input whose records may lack the log key rewrite_tag tests. Existing
// tvt blocks are removed first and every other byte is kept.
func FluentBit(src []byte, r Route) ([]byte, error) {
	return FluentBitRoutes(src, []Route{r})
}

// FluentBitRoutes is FluentBit for several streams: every existing tvt
// block is removed once, then each route is added.
func FluentBitRoutes(src []byte, routes []Route) ([]byte, error) {
	out, _, err := stripBlocks(src, "#")
	if err != nil {
		return nil, err
	}
	for _, r := range routes {
		if out, err = fluentRoute(out, r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func fluentRoute(out []byte, r Route) ([]byte, error) {
	if r.ID == "" || strings.ContainsAny(r.ID+r.Reason, "\n\r") || len(r.Stream) == 0 || r.Cold == "" {
		return nil, fmt.Errorf("propose: route needs a one-line ID and reason, a stream and a cold output")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("propose: not a Fluent Bit YAML config (want a mapping)")
	}
	pipe := value(doc.Content[0], "pipeline")
	if pipe == nil || pipe.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("propose: no pipeline section")
	}
	inputs, outputs := value(pipe, "inputs"), value(pipe, "outputs")
	if inputs == nil || outputs == nil || inputs.Kind != yaml.SequenceNode || outputs.Kind != yaml.SequenceNode || len(inputs.Content) == 0 {
		return nil, fmt.Errorf("propose: pipeline needs inputs and outputs lists")
	}
	// The Loki output that writes the stream.
	var tag string
	for _, o := range outputs.Content {
		if str(o, "name") != "loki" {
			continue
		}
		labels, static := fluentLabels(str(o, "labels"))
		if !maps.Equal(labels, r.Stream) {
			continue
		}
		if !static {
			return nil, fmt.Errorf("propose: the Loki output for the stream sets labels from record fields; rerouting its tag would move other streams")
		}
		if tag != "" {
			return nil, fmt.Errorf("propose: more than one Loki output writes the stream")
		}
		if value(o, "match_regex") != nil || strings.ContainsAny(str(o, "match"), "*") || str(o, "match") == "" {
			return nil, fmt.Errorf("propose: the Loki output for the stream matches by wildcard or regex; rerouting would move other tags' records")
		}
		tag = str(o, "match")
	}
	if tag == "" {
		return nil, fmt.Errorf("propose: no Loki output writes exactly the labels %s", fluentLabelString(r.Stream))
	}
	newTag := ColdTagPrefix + tag
	cold := false
	for _, o := range outputs.Content {
		if str(o, "alias") != r.Cold {
			continue
		}
		if !fluentMatch(str(o, "match"), newTag) && !fluentRegexMatch(str(o, "match_regex"), newTag) {
			return nil, fmt.Errorf("propose: cold output %q doesn't match %s (set its match to %s*)", r.Cold, newTag, ColdTagPrefix)
		}
		cold = true
	}
	if !cold {
		return nil, fmt.Errorf("propose: no output aliased %q for cold streams", r.Cold)
	}
	// Every input with that tag must give records a log key: a tail input
	// with no parser does.
	found := false
	for _, in := range inputs.Content {
		if str(in, "tag") != tag {
			continue
		}
		found = true
		if str(in, "name") != "tail" || value(in, "parser") != nil || value(in, "multiline.parser") != nil || value(in, "key") != nil {
			return nil, fmt.Errorf("propose: input %s for tag %s may not give every record a log key; tvt reroutes only tail inputs without parsers", str(in, "name"), tag)
		}
	}
	if !found {
		return nil, fmt.Errorf("propose: no input tags records %s", tag)
	}
	emitter := "tvt_cold_" + regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(tag, "_")
	item := func(indent string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "%s# %s\n", indent, r.begin())
		if r.Reason != "" {
			fmt.Fprintf(&b, "%s# %s\n", indent, r.Reason)
		}
		fmt.Fprintf(&b, "%s- name: rewrite_tag\n", indent)
		fmt.Fprintf(&b, "%s  match: %s\n", indent, tag)
		fmt.Fprintf(&b, "%s  rule: $log %s %s false\n", indent, r.fluentRegex(), newTag)
		fmt.Fprintf(&b, "%s  emitter_name: %s\n", indent, emitter)
		fmt.Fprintf(&b, "%s# %s\n", indent, endMarker)
		return b.String()
	}
	keyIndent := strings.Repeat(" ", pipe.Content[0].Column-1)
	switch filters := value(pipe, "filters"); {
	case filters == nil:
		text := item(keyIndent + "  ")
		// The filters key goes inside the block, so stripping it leaves
		// no empty key behind.
		lines := strings.SplitAfterN(text, "\n", 2)
		text = lines[0] + keyIndent + "filters:\n" + lines[1]
		out = insertAfterLine(out, pastBlockEnd(out, lastLine(inputs)), text)
	case filters.Kind != yaml.SequenceNode || filters.Style&yaml.FlowStyle != 0 || len(filters.Content) == 0:
		return nil, fmt.Errorf("propose: pipeline filters isn't a block list tvt can append to")
	default:
		out = insertAfterLine(out, pastBlockEnd(out, lastLine(filters)), item(strings.Repeat(" ", filters.Content[0].Column-3)))
	}
	if err := checkFluentRoute(out, tag, newTag, r.fluentRegex()); err != nil {
		return nil, err
	}
	return out, nil
}

// str is a mapping's scalar value for key, "" when absent.
func str(m *yaml.Node, key string) string {
	if v := value(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

// fluentLabels parses the Loki output's labels ("app=shop, level=debug");
// static is false when any value comes from a record field ($level) or a
// label is given without a value.
func fluentLabels(s string) (map[string]string, bool) {
	out := map[string]string{}
	static := true
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(v), "$") || strings.HasPrefix(part, "$") {
			static = false
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, static
}

func fluentLabelString(m map[string]string) string {
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ", ")
}

// fluentMatch is Fluent Bit's match: * matches any run of characters.
func fluentMatch(pattern, tag string) bool {
	if pattern == "" {
		return false
	}
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	return regexp.MustCompile(re).MatchString(tag)
}

func fluentRegexMatch(pattern, tag string) bool {
	if pattern == "" {
		return false
	}
	re, err := regexp.Compile(pattern)
	return err == nil && re.MatchString(tag)
}

// checkFluentRoute re-reads the config: exactly one tvt rewrite_tag for
// tag, retagging to newTag without keeping a copy.
func checkFluentRoute(src []byte, tag, newTag, regex string) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	filters := value(value(doc.Content[0], "pipeline"), "filters")
	n := 0
	if filters != nil {
		for _, f := range filters.Content {
			if str(f, "name") == "rewrite_tag" && str(f, "match") == tag && str(f, "rule") == "$log "+regex+" "+newTag+" false" {
				n++
			}
		}
	}
	if n != 1 {
		return fmt.Errorf("propose: the edited config has %d tvt reroutes for %s, want 1", n, tag)
	}
	return nil
}

// pastBlockEnd moves an insertion point (after line n) past the tvt:end
// lines that close a block ending there, so a new block never lands
// inside another.
func pastBlockEnd(src []byte, n int) int {
	lines := strings.Split(string(src), "\n")
	for n < len(lines) && strings.TrimSpace(lines[n]) == "# "+endMarker {
		n++
	}
	return n
}
