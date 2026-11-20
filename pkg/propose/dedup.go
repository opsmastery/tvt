package propose

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"go.yaml.in/yaml/v3"
)

// OtelcolDedupProcessor is the log_dedup processor tvt owns (S10 item 4).
const OtelcolDedupProcessor = "log_dedup/tvt"

// DedupPrefix starts every collapse block's ID, so the dedup writer
// strips only its own blocks.
const DedupPrefix = "dedup:"

// DedupCountAttribute is where log_dedup puts how many lines one
// emitted record stands for (the processor's default name): a count over
// a collapsed stream sums it instead of counting lines.
const DedupCountAttribute = "log_count"

// DefaultDedupInterval is the collapse window. log_dedup's own default
// is 10s; a minute keeps a line that repeats every few seconds to one
// record a minute while a reader still sees it within the minute.
const DefaultDedupInterval = time.Minute

// dedupSince is the oldest Collector the writer supports: the processor
// is alpha, and its component name became log_dedup (logdedup before).
const dedupSince = "0.122.0"

// Dedup collapses identical log records of some services into one record
// a window with a count (S10 item 4, ADR 0010). Records are excluded by
// condition, never collapsed (ADR 0011); check judges the conditions
// against every read stream and record match.
type Dedup struct {
	ID       string // dedup:<pipeline>
	Scan     int64
	Evidence string
	Reason   string
	// Services are the service.name values whose repeated lines collapse.
	Services []string
	// Records are the record log-event conditions (the manifest's
	// record_matches.log_events), each excluded with and not (...).
	Records   []string
	Interval  time.Duration
	Collector string
}

func (d Dedup) begin() string {
	s := fmt.Sprintf("tvt:begin %s scan=%d", d.ID, d.Scan)
	if d.Evidence != "" {
		s += " evidence=" + d.Evidence
	}
	return s
}

func (d Dedup) validate() error {
	if !strings.HasPrefix(d.ID, DedupPrefix) || strings.ContainsAny(d.ID, " \n\r") {
		return fmt.Errorf("propose: dedup ID %q: want %s<name>", d.ID, DedupPrefix)
	}
	if strings.ContainsAny(d.Reason, "\n\r") {
		return fmt.Errorf("propose: dedup %s: reason must be one line", d.ID)
	}
	if len(d.Services) == 0 {
		return fmt.Errorf("propose: dedup %s: no service to collapse (an unscoped log_dedup collapses every log)", d.ID)
	}
	for _, s := range d.Services {
		if s == "" || strings.ContainsAny(s, "\n\r") {
			return fmt.Errorf("propose: dedup %s: bad service name %q", d.ID, s)
		}
	}
	for _, r := range d.Records {
		if strings.ContainsAny(r, "\n\r'") {
			return fmt.Errorf("propose: dedup %s: record condition %q can't be quoted", d.ID, r)
		}
	}
	if d.Interval != 0 && (d.Interval < time.Second || d.Interval%time.Second != 0) {
		return fmt.Errorf("propose: dedup %s: interval %s isn't a whole number of seconds", d.ID, d.Interval)
	}
	if d.Collector == "" {
		return fmt.Errorf("propose: dedup %s: set pipelines[].version: log_dedup is checked on Collector v%s and later", d.ID, dedupSince)
	}
	v, err := parseVersion(d.Collector)
	if err != nil {
		return fmt.Errorf("propose: dedup %s: %w", d.ID, err)
	}
	if min, _ := parseVersion(dedupSince); less(v, min) {
		return fmt.Errorf("propose: dedup %s: Collector v%s: log_dedup is checked on v%s and later", d.ID, d.Collector, dedupSince)
	}
	return nil
}

// Conditions are the processor's conditions: one per service, records
// excluded. log_dedup collapses a log when any is true.
func (d Dedup) Conditions() []string {
	services := slices.Clone(d.Services)
	slices.Sort(services)
	services = slices.Compact(services)
	var out []string
	for _, s := range services {
		c := `resource.attributes["service.name"] == ` + strconv.Quote(s)
		for _, r := range d.Records {
			c += " and not (" + logContext(r) + ")"
		}
		out = append(out, c)
	}
	return out
}

// logContext qualifies a record match's bare log paths (attributes,
// body, severity) with log., as Collectors from v0.122.0 want them in a
// log-context condition (they rewrite bare ones and log that they did).
// Paths inside quoted strings are left alone.
func logContext(cond string) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(cond); i++ {
		c := cond[i]
		if c == '"' && (i == 0 || cond[i-1] != '\\') {
			inStr = !inStr
		}
		if !inStr && (i == 0 || !isIdent(cond[i-1])) {
			for _, p := range []string{"attributes[", "body", "severity_text", "severity_number"} {
				if strings.HasPrefix(cond[i:], p) && (strings.HasSuffix(p, "[") || i+len(p) == len(cond) || !isIdent(cond[i+len(p)])) {
					b.WriteString("log.")
					break
				}
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isDedupBlock(begin string) bool { return strings.HasPrefix(begin, DedupPrefix) }

// OtelcolDedup adds d as log_dedup/tvt, first in each named logs
// pipeline. Existing dedup blocks (and tvt's entry in a flow-style list)
// are removed first and other tvt blocks are kept: run it after
// OtelcolChanges, which strips every block. A nil d only strips.
func OtelcolDedup(src []byte, pipelines []string, d *Dedup) ([]byte, error) {
	out, _, err := stripBlocksIf(src, "#", isDedupBlock)
	if err != nil {
		return nil, err
	}
	if out, err = stripOtelRefsOf(out, []string{OtelcolDedupProcessor}); err != nil {
		return nil, err
	}
	if d == nil {
		return out, nil
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	interval := d.Interval
	if interval == 0 {
		interval = DefaultDedupInterval
	}
	root, err := otelRoot(out)
	if err != nil {
		return nil, err
	}
	procs := value(root, "processors")
	if procs == nil || procs.Kind != yaml.MappingNode || procs.Style&yaml.FlowStyle != 0 || len(procs.Content) == 0 {
		return nil, fmt.Errorf("propose: no processors section tvt can add to (want a block mapping with at least one processor)")
	}
	if value(procs, OtelcolDedupProcessor) != nil {
		return nil, fmt.Errorf("propose: processors already has %s outside a tvt block: tvt won't overwrite it", OtelcolDedupProcessor)
	}
	all := value(value(root, "service"), "pipelines")
	if all == nil || all.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("propose: no service.pipelines")
	}
	type edit struct {
		line, col int
		text      string
	}
	indent := strings.Repeat(" ", procs.Content[0].Column-1)
	var b strings.Builder
	fmt.Fprintf(&b, "%s# %s\n", indent, d.begin())
	if d.Reason != "" {
		fmt.Fprintf(&b, "%s# %s\n", indent, d.Reason)
	}
	fmt.Fprintf(&b, "%s# one record per identical line a window, with the count in %s: a count over these streams sums it (tvt S10 item 4)\n", indent, DedupCountAttribute)
	fmt.Fprintf(&b, "%s%s:\n%s  interval: %s\n%s  log_count_attribute: %s\n%s  conditions:\n", indent, OtelcolDedupProcessor, indent, model.Duration(interval), indent, DedupCountAttribute, indent)
	for _, c := range d.Conditions() {
		fmt.Fprintf(&b, "%s    - '%s'\n", indent, c)
	}
	fmt.Fprintf(&b, "%s# %s\n", indent, endMarker)
	edits := []edit{{line: pastBlockEnd(out, lastLine(procs)), text: b.String()}}
	names := slices.Clone(pipelines)
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("propose: dedup %s: no logs pipeline to put it in", d.ID)
	}
	for _, name := range names {
		if name != "logs" && !strings.HasPrefix(name, "logs/") {
			return nil, fmt.Errorf("propose: pipeline %q isn't a logs pipeline", name)
		}
		p := value(all, name)
		if p == nil || p.Kind != yaml.MappingNode || len(p.Content) == 0 {
			return nil, fmt.Errorf("propose: no service.pipelines.%s", name)
		}
		pr := value(p, "processors")
		switch {
		case pr == nil:
			in := strings.Repeat(" ", p.Content[0].Column-1)
			edits = append(edits, edit{line: lastLine(p), text: fmt.Sprintf("%s# %s\n%sprocessors: [%s]\n%s# %s\n", in, d.begin(), in, OtelcolDedupProcessor, in, endMarker)})
		case pr.Kind != yaml.SequenceNode:
			return nil, fmt.Errorf("propose: service.pipelines.%s.processors isn't a list", name)
		case pr.Style&yaml.FlowStyle != 0:
			text := OtelcolDedupProcessor
			if len(pr.Content) > 0 {
				text += ", "
			}
			edits = append(edits, edit{line: pr.Line, col: pr.Column + 1, text: text})
		case len(pr.Content) == 0:
			return nil, fmt.Errorf("propose: service.pipelines.%s.processors is empty", name)
		default:
			first := pr.Content[0]
			in := strings.Repeat(" ", first.Column-3)
			edits = append(edits, edit{line: first.Line - 1, text: fmt.Sprintf("%s# %s\n%s- %s\n%s# %s\n", in, d.begin(), in, OtelcolDedupProcessor, in, endMarker)})
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
	// Re-read: the processor exists and leads every named pipeline.
	root, err = otelRoot(out)
	if err != nil {
		return nil, fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	if value(value(root, "processors"), OtelcolDedupProcessor) == nil {
		return nil, fmt.Errorf("propose: the edited config lost %s", OtelcolDedupProcessor)
	}
	for _, name := range names {
		pr := value(value(value(value(root, "service"), "pipelines"), name), "processors")
		if pr == nil || len(pr.Content) == 0 || pr.Content[0].Value != OtelcolDedupProcessor {
			return nil, fmt.Errorf("propose: %s isn't first in pipeline %s after the edit", OtelcolDedupProcessor, name)
		}
	}
	return out, nil
}
