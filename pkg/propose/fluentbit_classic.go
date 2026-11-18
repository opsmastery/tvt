package propose

import (
	"fmt"
	"maps"
	"regexp"
	"strings"
)

// Fluent Bit's classic configuration format ([INPUT], [FILTER] and
// [OUTPUT] sections of "Key Value" lines): the same stream reroute as
// FluentBitRoutes, a rewrite_tag [FILTER] per stream, written as a
// section before the first [OUTPUT].

// fluentSection is one [SECTION] of a classic config.
type fluentSection struct {
	kind       string // INPUT, FILTER, OUTPUT, SERVICE, PARSER...
	start, end int    // 0-based line indexes, end exclusive
	keys       map[string]string
	indent     string
}

func (s fluentSection) get(k string) string { return s.keys[strings.ToLower(k)] }

var fluentHeader = regexp.MustCompile(`^\s*\[([A-Za-z_]+)\]\s*$`)

func parseFluentClassic(src []byte) ([]fluentSection, []string, error) {
	lines := strings.SplitAfter(string(src), "\n")
	var out []fluentSection
	cur := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if m := fluentHeader.FindStringSubmatch(l); m != nil {
			if cur >= 0 {
				out[cur].end = i
			}
			out = append(out, fluentSection{kind: strings.ToUpper(m[1]), start: i, end: len(lines), keys: map[string]string{}})
			cur = len(out) - 1
			continue
		}
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "@") {
			if strings.HasPrefix(strings.ToUpper(t), "@INCLUDE") {
				return nil, nil, fmt.Errorf("propose: @INCLUDE: tvt reads one file; list the included files in the pipeline's files")
			}
			continue
		}
		if cur < 0 {
			return nil, nil, fmt.Errorf("propose: line %d is outside any section", i+1)
		}
		k, v, _ := strings.Cut(t, " ")
		out[cur].keys[strings.ToLower(k)] = strings.TrimSpace(v)
		if out[cur].indent == "" {
			out[cur].indent = l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		}
	}
	return out, lines, nil
}

// FluentBitClassicRoutes is FluentBitRoutes for the classic format.
func FluentBitClassicRoutes(src []byte, routes []Route) ([]byte, error) {
	out, _, err := stripBlocks(src, "#")
	if err != nil {
		return nil, err
	}
	for _, r := range routes {
		if out, err = fluentClassicRoute(out, r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func fluentClassicRoute(src []byte, r Route) ([]byte, error) {
	if r.ID == "" || strings.ContainsAny(r.ID+r.Reason, "\n\r") || len(r.Stream) == 0 || r.Cold == "" {
		return nil, fmt.Errorf("propose: route needs a one-line ID and reason, a stream and a cold output")
	}
	secs, lines, err := parseFluentClassic(src)
	if err != nil {
		return nil, err
	}
	var tag string
	firstOutput := -1
	for _, s := range secs {
		if s.kind != "OUTPUT" {
			continue
		}
		if firstOutput < 0 {
			firstOutput = s.start
		}
		if !strings.EqualFold(s.get("name"), "loki") {
			continue
		}
		labels, static := fluentLabels(s.get("labels"))
		if !maps.Equal(labels, r.Stream) {
			continue
		}
		if !static {
			return nil, fmt.Errorf("propose: the Loki output for the stream sets labels from record fields; rerouting its tag would move other streams")
		}
		if tag != "" {
			return nil, fmt.Errorf("propose: more than one Loki output writes the stream")
		}
		if s.get("match_regex") != "" || strings.Contains(s.get("match"), "*") || s.get("match") == "" {
			return nil, fmt.Errorf("propose: the Loki output for the stream matches by wildcard or regex; rerouting would move other tags' records")
		}
		tag = s.get("match")
	}
	if tag == "" {
		return nil, fmt.Errorf("propose: no Loki output writes exactly the labels %s", fluentLabelString(r.Stream))
	}
	newTag := ColdTagPrefix + tag
	cold := false
	for _, s := range secs {
		if s.kind == "OUTPUT" && s.get("alias") == r.Cold {
			if !fluentMatch(s.get("match"), newTag) && !fluentRegexMatch(s.get("match_regex"), newTag) {
				return nil, fmt.Errorf("propose: cold output %q doesn't match %s (set its Match to %s*)", r.Cold, newTag, ColdTagPrefix)
			}
			cold = true
		}
	}
	if !cold {
		return nil, fmt.Errorf("propose: no output aliased %q for cold streams", r.Cold)
	}
	found := false
	for _, s := range secs {
		if s.kind != "INPUT" || s.get("tag") != tag {
			continue
		}
		found = true
		if !strings.EqualFold(s.get("name"), "tail") || s.get("parser") != "" || s.get("multiline.parser") != "" || s.get("key") != "" {
			return nil, fmt.Errorf("propose: input %s for tag %s may not give every record a log key; tvt reroutes only tail inputs without parsers", s.get("name"), tag)
		}
	}
	if !found {
		return nil, fmt.Errorf("propose: no input tags records %s", tag)
	}
	indent := "    "
	for _, s := range secs {
		if s.indent != "" {
			indent = s.indent
			break
		}
	}
	text := classicFilter(r, tag, indent)
	var b strings.Builder
	b.WriteString(text)
	// Before the first [OUTPUT], after any comment lines heading it.
	at := firstOutput
	for at > 0 && strings.HasPrefix(strings.TrimSpace(lines[at-1]), "#") && !strings.Contains(lines[at-1], endMarker) {
		at--
	}
	lines = append(lines[:at], append([]string{b.String()}, lines[at:]...)...)
	out := []byte(strings.Join(lines, ""))
	// Re-read: exactly one rewrite_tag of this tag to the cold tag.
	secs, _, err = parseFluentClassic(out)
	if err != nil {
		return nil, fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	n := 0
	for _, s := range secs {
		if s.kind == "FILTER" && strings.EqualFold(s.get("name"), "rewrite_tag") && s.get("match") == tag && s.get("rule") == "$log ^(?!.*(?i:error|exception|traceback|critical|fatal)).*$ "+newTag+" false" {
			n++
		}
	}
	if n != 1 {
		return nil, fmt.Errorf("propose: the edited config has %d tvt reroutes for %s, want 1", n, tag)
	}
	return out, nil
}

// classicFilter is the marked rewrite_tag [FILTER] section for r's tag.
func classicFilter(r Route, tag, indent string) string {
	newTag := ColdTagPrefix + tag
	emitter := "tvt_cold_" + regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(tag, "_")
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", r.begin())
	if r.Reason != "" {
		fmt.Fprintf(&b, "# %s\n", r.Reason)
	}
	fmt.Fprintf(&b, "[FILTER]\n%sName         rewrite_tag\n%sMatch        %s\n%sRule         $log ^(?!.*(?i:error|exception|traceback|critical|fatal)).*$ %s false\n%sEmitter_Name %s\n\n# %s\n",
		indent, indent, tag, indent, newTag, indent, emitter, endMarker)
	return b.String()
}

// classicRouteTag runs FluentBitClassicRoutes' checks for r on src and
// returns the tag the route retags, without writing anything.
func classicRouteTag(src []byte, r Route) (string, error) {
	out, err := fluentClassicRoute(src, r)
	if err != nil {
		return "", err
	}
	secs, _, err := parseFluentClassic(out)
	if err != nil {
		return "", err
	}
	for _, s := range secs {
		if s.kind == "FILTER" && strings.EqualFold(s.get("name"), "rewrite_tag") && strings.HasSuffix(s.get("rule"), " false") && strings.Contains(s.get("rule"), ColdTagPrefix) {
			return s.get("match"), nil
		}
	}
	return "", fmt.Errorf("propose: no reroute written")
}
