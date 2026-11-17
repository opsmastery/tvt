package propose

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The fluent/fluent-bit Helm chart (S7 item 3) carries the Fluent Bit
// configuration as classic-format strings in its values: config.inputs,
// config.filters and config.outputs, which the chart joins into
// fluent-bit.conf. tvt checks each route against the joined config (the
// same checks as FluentBitClassicRoutes), then writes the rewrite_tag
// [FILTER] into config.filters. Helm replaces a string rather than
// merging it, so when the overlay doesn't set config.filters, tvt creates
// it starting with the chart's default filters (from defaults), or the
// chart's kubernetes filter would silently go.

// FluentBitHelmRoutes writes routes into a fluent-bit chart values file.
// root is the chart's key in an umbrella chart's values, or "".
func FluentBitHelmRoutes(values, defaults []byte, root string, routes []Route) ([]byte, error) {
	var defs yaml.Node
	if err := yaml.Unmarshal(defaults, &defs); err != nil {
		return nil, fmt.Errorf("propose: chart defaults: %w", err)
	}
	out, _, err := stripBlocks(values, "#")
	if err != nil {
		return nil, err
	}
	path := func(k string) []string {
		p := []string{"config", k}
		if root != "" {
			p = append([]string{root}, p...)
		}
		return p
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	section := func(k string) (string, bool) {
		if n := lookup(&doc, path(k)); n != nil && n.Kind == yaml.ScalarNode {
			return n.Value, true
		}
		if n := lookup(&defs, []string{"config", k}); n != nil && n.Kind == yaml.ScalarNode {
			return n.Value, false
		}
		return "", false
	}
	inputs, _ := section("inputs")
	filters, ownFilters := section("filters")
	outputs, _ := section("outputs")
	joined := []byte(inputs + "\n" + filters + "\n" + outputs)
	var add strings.Builder
	for _, r := range routes {
		tag, err := classicRouteTag(joined, r)
		if err != nil {
			return nil, err
		}
		add.WriteString(classicFilter(r, tag, "    "))
	}
	if ownFilters {
		// Append to the overlay's config.filters block scalar.
		n := lookup(&doc, path("filters"))
		if n.Style&yaml.LiteralStyle == 0 {
			return nil, fmt.Errorf("propose: config.filters isn't a literal block (|) tvt can append to")
		}
		lines := strings.SplitAfter(string(out), "\n")
		// Content starts on the line after the key; its indent is the
		// first non-empty content line's.
		indent, last := "", n.Line
		for i := n.Line; i < len(lines); i++ {
			l := lines[i]
			if strings.TrimSpace(l) == "" {
				continue
			}
			ind := l[:len(l)-len(strings.TrimLeft(l, " "))]
			if indent == "" {
				indent = ind
			}
			if len(ind) < len(indent) {
				break
			}
			last = i + 1
		}
		if indent == "" {
			return nil, fmt.Errorf("propose: config.filters is empty")
		}
		out = insertAfterLine(out, last, indentLines(add.String(), indent))
	} else {
		// Create config.filters: the chart's defaults, then tvt's filters.
		cfg := lookup(&doc, path("")[:len(path(""))-1])
		indent, after := "", strings.Count(string(out), "\n")
		var keys strings.Builder
		switch {
		case cfg != nil && cfg.Kind == yaml.MappingNode && len(cfg.Content) > 0:
			indent = strings.Repeat(" ", cfg.Content[0].Column-1)
			after = pastBlockEnd(out, lastLine(cfg))
		case cfg != nil:
			return nil, fmt.Errorf("propose: config isn't a mapping tvt can add to")
		default:
			// No config mapping: create it, under the root key if that's
			// there, else with it.
			want := path("")[:len(path(""))-1]
			in := ""
			if root != "" {
				if r := lookup(&doc, []string{root}); r != nil {
					if r.Kind != yaml.MappingNode || len(r.Content) == 0 {
						return nil, fmt.Errorf("propose: %s isn't a mapping tvt can add to", root)
					}
					in = strings.Repeat(" ", r.Content[0].Column-1)
					after = pastBlockEnd(out, lastLine(r))
					want = want[1:]
				}
			}
			for _, k := range want {
				fmt.Fprintf(&keys, "%s%s:\n", in, k)
				in += "  "
			}
			indent = in
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s# %s\n", indent, routes[0].begin())
		b.WriteString(keys.String())
		fmt.Fprintf(&b, "%s# the chart's default filters first: Helm replaces the string\n", indent)
		fmt.Fprintf(&b, "%sfilters: |\n", indent)
		body := strings.TrimRight(filters, "\n") + "\n\n" + stripMarkerLines(add.String())
		b.WriteString(indentLines(body, indent+"  "))
		fmt.Fprintf(&b, "%s# %s\n", indent, endMarker)
		out = insertAfterLine(out, after, b.String())
	}
	// Re-read: config.filters holds each route's rule.
	var check yaml.Node
	if err := yaml.Unmarshal(out, &check); err != nil {
		return nil, fmt.Errorf("propose: the edited values don't parse: %w", err)
	}
	n := lookup(&check, path("filters"))
	if n == nil || n.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("propose: config.filters is gone after the edit")
	}
	for _, r := range routes {
		tag, _ := classicRouteTag(joined, r)
		if !strings.Contains(n.Value, "Rule         $log ^.*$ "+ColdTagPrefix+tag+" false") {
			return nil, fmt.Errorf("propose: config.filters lost the reroute of %s", tag)
		}
	}
	return out, nil
}

// indentLines prefixes every non-empty line.
func indentLines(s, indent string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		if strings.TrimSpace(l) == "" {
			b.WriteString(l)
			continue
		}
		b.WriteString(indent + l)
	}
	return b.String()
}

// stripMarkerLines drops tvt:begin and tvt:end lines (inside a block that
// already has its own markers, nested ones would break stripping).
func stripMarkerLines(s string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "#"))
		if strings.HasPrefix(t, "tvt:begin") || t == endMarker {
			continue
		}
		b.WriteString(l)
	}
	return b.String()
}
