package propose

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// OtelcolProcessor is the filter processor tvt owns in an OpenTelemetry
// Collector config: one per file, listed first in each metrics pipeline it
// drops from, so the metrics are dropped before any other processor spends
// work on them.
const OtelcolProcessor = "filter/tvt"

// otelName is an OpenTelemetry instrument name (the API's syntax: a letter,
// then letters, digits, _ . - /, at most 255).
var otelName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\-/]{0,254}$`)

// Otelcol adds d as a filter processor dropping d.Metrics, named exactly,
// to an OpenTelemetry Collector config's bytes, and lists it first in the
// processors of each metrics pipeline in pipelines (S7 item 3). d.Metrics
// are names as the pipeline's receivers emit them (OTel names, not the
// Prometheus names a backend shows after translation).
//
// Existing tvt blocks, and tvt's entry in a flow-style processors list,
// are removed first; the new text is spliced into the original bytes, so
// every other byte is kept. The processor uses error_mode: ignore, so a
// condition that fails to evaluate keeps the data. The result is re-read
// and must carry the processor in every named pipeline. Full validation
// (otelcol validate) is S7 item 5's.
//
// It fails rather than guess: no processors section, a pipeline that
// isn't in the file or isn't a metrics pipeline, or a processors value
// that isn't a list.
func Otelcol(src []byte, pipelines []string, d Drop) ([]byte, error) {
	if err := d.validateOtel(); err != nil {
		return nil, err
	}
	form, err := OTTLFormFor(d.Collector)
	if err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	for _, c := range otelConditions(d) {
		if err := lintOTTL(c, form); err != nil {
			return nil, err
		}
	}
	if len(pipelines) == 0 {
		return nil, fmt.Errorf("propose: drop %s: no pipeline to put it in", d.ID)
	}
	out, _, err := stripBlocks(src, "#")
	if err != nil {
		return nil, err
	}
	if out, err = stripOtelRefs(out); err != nil {
		return nil, err
	}
	root, err := otelRoot(out)
	if err != nil {
		return nil, err
	}
	procs := value(root, "processors")
	if procs == nil || procs.Kind != yaml.MappingNode || procs.Style&yaml.FlowStyle != 0 || len(procs.Content) == 0 {
		return nil, fmt.Errorf("propose: no processors section tvt can add to (want a block mapping with at least one processor)")
	}
	if value(procs, OtelcolProcessor) != nil {
		return nil, fmt.Errorf("propose: processors already has %s outside a tvt block: tvt won't overwrite it", OtelcolProcessor)
	}
	service := value(root, "service")
	var all *yaml.Node
	if service != nil {
		all = value(service, "pipelines")
	}
	if all == nil || all.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("propose: no service.pipelines")
	}

	// Edits, applied bottom up so earlier positions stay valid: an
	// insertion after a line, or bytes into a line at a column.
	type edit struct {
		line, col int // col 0: insert text after line; else at (line, col)
		text      string
	}
	edits := []edit{{line: lastLine(procs), text: otelBlock(strings.Repeat(" ", procs.Content[0].Column-1), d)}}
	names := slices.Clone(pipelines)
	slices.Sort(names)
	names = slices.Compact(names)
	for _, name := range names {
		if name != "metrics" && !strings.HasPrefix(name, "metrics/") {
			return nil, fmt.Errorf("propose: pipeline %q isn't a metrics pipeline", name)
		}
		p := value(all, name)
		if p == nil || p.Kind != yaml.MappingNode || len(p.Content) == 0 {
			return nil, fmt.Errorf("propose: no service.pipelines.%s", name)
		}
		pr := value(p, "processors")
		switch {
		case pr == nil:
			indent := strings.Repeat(" ", p.Content[0].Column-1)
			edits = append(edits, edit{line: lastLine(p), text: fmt.Sprintf("%s# %s\n%sprocessors: [%s]\n%s# %s\n", indent, d.begin(), indent, OtelcolProcessor, indent, endMarker)})
		case pr.Kind != yaml.SequenceNode:
			return nil, fmt.Errorf("propose: service.pipelines.%s.processors isn't a list", name)
		case pr.Style&yaml.FlowStyle != 0:
			// [a, b] -> [filter/tvt, a, b]; no room for marker lines, so
			// stripOtelRefs takes the entry out again on a rerun.
			text := OtelcolProcessor
			if len(pr.Content) > 0 {
				text += ", "
			}
			edits = append(edits, edit{line: pr.Line, col: pr.Column + 1, text: text})
		case len(pr.Content) == 0:
			return nil, fmt.Errorf("propose: service.pipelines.%s.processors is empty", name)
		default:
			first := pr.Content[0]
			indent := strings.Repeat(" ", first.Column-3)
			edits = append(edits, edit{line: first.Line - 1, text: fmt.Sprintf("%s# %s\n%s- %s\n%s# %s\n", indent, d.begin(), indent, OtelcolProcessor, indent, endMarker)})
		}
	}
	slices.SortStableFunc(edits, func(a, b edit) int {
		if a.line != b.line {
			return b.line - a.line
		}
		return b.col - a.col
	})
	for _, e := range edits {
		if e.col == 0 {
			out = insertAfterLine(out, e.line, e.text)
		} else {
			out = insertAt(out, e.line, e.col, e.text)
		}
	}
	if err := checkOtelDrop(out, names, d); err != nil {
		return nil, err
	}
	return out, nil
}

// otelBlock is the marked processor definition. indent is the
// processors' keys' indentation.
func otelBlock(indent string, d Drop) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s# %s\n", indent, d.begin())
	if d.Reason != "" {
		fmt.Fprintf(&b, "%s# %s\n", indent, d.Reason)
	}
	fmt.Fprintf(&b, "%s%s:\n", indent, OtelcolProcessor)
	fmt.Fprintf(&b, "%s  error_mode: ignore\n", indent)
	fmt.Fprintf(&b, "%s  metrics:\n", indent)
	fmt.Fprintf(&b, "%s    metric:\n", indent)
	for _, c := range otelConditions(d) {
		fmt.Fprintf(&b, "%s      - '%s'\n", indent, c)
	}
	fmt.Fprintf(&b, "%s# %s\n", indent, endMarker)
	return b.String()
}

// otelConditions are the filter's OTTL conditions, one exact name each
// (the filter drops a metric when any matches).
func otelConditions(d Drop) []string {
	names := slices.Clone(d.Metrics)
	slices.Sort(names)
	names = slices.Compact(names)
	var out []string
	for _, n := range names {
		if d.PromNames {
			cs, _ := OtelConditions(n)
			out = append(out, cs...)
			continue
		}
		out = append(out, fmt.Sprintf(`metric.name == "%s"`, n))
	}
	// An unparseable version fails in Otelcol before this is used.
	if form, err := OTTLFormFor(d.Collector); err == nil && !form.Context {
		for i, c := range out {
			out[i] = barePaths(c)
		}
	}
	return out
}

// validateOtel is validate with OTel instrument names.
func (d Drop) validateOtel() error {
	if d.ID == "" || strings.ContainsAny(d.ID, "\n\r") {
		return fmt.Errorf("propose: drop needs a one-line ID")
	}
	if len(d.Metrics) == 0 {
		return fmt.Errorf("propose: drop %s names no metric", d.ID)
	}
	for _, m := range d.Metrics {
		if d.PromNames {
			if !metricName.MatchString(m) {
				return fmt.Errorf("propose: drop %s: %q is not a metric name", d.ID, m)
			}
			if _, ok := OtelConditions(m); !ok {
				return fmt.Errorf("propose: drop %s: %s may be one series of an OTel histogram, which a collector can't drop alone", d.ID, m)
			}
			continue
		}
		if !otelName.MatchString(m) {
			return fmt.Errorf("propose: drop %s: %q is not an OpenTelemetry metric name", d.ID, m)
		}
	}
	if strings.ContainsAny(d.Reason, "\n\r") {
		return fmt.Errorf("propose: drop %s: reason must be one line", d.ID)
	}
	return nil
}

func otelRoot(src []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("propose: not an OpenTelemetry Collector config (want a mapping)")
	}
	return doc.Content[0], nil
}

// stripOtelRefs removes tvt's processor from flow-style processors lists
// (block-list entries sit in marked blocks, which stripBlocks removed).
func stripOtelRefs(src []byte) ([]byte, error) {
	root, err := otelRoot(src)
	if err != nil {
		return nil, err
	}
	service := value(root, "service")
	if service == nil {
		return src, nil
	}
	all := value(service, "pipelines")
	if all == nil || all.Kind != yaml.MappingNode {
		return src, nil
	}
	type cut struct{ line, col, n int }
	var cuts []cut
	lines := strings.SplitAfter(string(src), "\n")
	for i := 1; i < len(all.Content); i += 2 {
		pr := value(all.Content[i], "processors")
		if pr == nil || pr.Kind != yaml.SequenceNode || pr.Style&yaml.FlowStyle == 0 {
			continue
		}
		for _, item := range pr.Content {
			if item.Value != OtelcolProcessor {
				continue
			}
			// The entry and the ", " after it, as tvt wrote them.
			l := lines[item.Line-1]
			start := item.Column - 1
			n := len(OtelcolProcessor)
			rest := l[start+n:]
			trimmed := strings.TrimLeft(rest, " ")
			if strings.HasPrefix(trimmed, ",") {
				n += len(rest) - len(trimmed) + 1
				n += len(trimmed[1:]) - len(strings.TrimLeft(trimmed[1:], " "))
			}
			cuts = append(cuts, cut{item.Line, start, n})
		}
	}
	slices.SortFunc(cuts, func(a, b cut) int {
		if a.line != b.line {
			return b.line - a.line
		}
		return b.col - a.col
	})
	for _, c := range cuts {
		l := lines[c.line-1]
		lines[c.line-1] = l[:c.col] + l[c.col+c.n:]
	}
	return []byte(strings.Join(lines, "")), nil
}

// insertAt inserts text into line (1-based) before byte column col
// (1-based).
func insertAt(src []byte, line, col int, text string) []byte {
	lines := strings.SplitAfter(string(src), "\n")
	l := lines[line-1]
	lines[line-1] = l[:col-1] + text + l[col-1:]
	return []byte(strings.Join(lines, ""))
}

// checkOtelDrop re-reads src: the processor holds d's conditions and
// comes first in every named pipeline.
func checkOtelDrop(src []byte, pipelines []string, d Drop) error {
	root, err := otelRoot(src)
	if err != nil {
		return fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	var got struct {
		Processors map[string]struct {
			ErrorMode string `yaml:"error_mode"`
			Metrics   struct {
				Metric []string `yaml:"metric"`
			} `yaml:"metrics"`
		} `yaml:"processors"`
		Service struct {
			Pipelines map[string]struct {
				Processors []string `yaml:"processors"`
			} `yaml:"pipelines"`
		} `yaml:"service"`
	}
	if err := root.Decode(&got); err != nil {
		return fmt.Errorf("propose: the edited config doesn't decode: %w", err)
	}
	f, ok := got.Processors[OtelcolProcessor]
	if !ok || f.ErrorMode != "ignore" || !slices.Equal(f.Metrics.Metric, otelConditions(d)) {
		return fmt.Errorf("propose: the edited config lost %s's conditions", OtelcolProcessor)
	}
	for _, p := range pipelines {
		if ps := got.Service.Pipelines[p].Processors; len(ps) == 0 || ps[0] != OtelcolProcessor {
			return fmt.Errorf("propose: pipeline %s: %s isn't its first processor", p, OtelcolProcessor)
		}
	}
	return nil
}

// OtelcolPipelines lists a collector config's metrics pipelines (metrics
// and metrics/<name>), sorted.
func OtelcolPipelines(src []byte) ([]string, error) {
	root, err := otelRoot(src)
	if err != nil {
		return nil, err
	}
	var out []string
	if service := value(root, "service"); service != nil {
		if all := value(service, "pipelines"); all != nil && all.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(all.Content); i += 2 {
				if n := all.Content[i].Value; n == "metrics" || strings.HasPrefix(n, "metrics/") {
					out = append(out, n)
				}
			}
		}
	}
	slices.Sort(out)
	return out, nil
}
