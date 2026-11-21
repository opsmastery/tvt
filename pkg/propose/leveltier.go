package propose

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Level tiers (S10 item 3, ADR 0010: after collapse, before sampling).
// A stream whose lines below warn nothing reads sends those lines to the
// cold tier and keeps the rest hot. In an OTel Collector the split is two
// filters: filter/tvt-tier-hot, first in the stream's logs pipeline,
// drops the tier's lines there; a new pipeline, logs/tvt-tier, takes the
// same receivers, keeps only the tier's lines (filter/tvt-tier-cold) and
// exports them to the cold exporter (pipelines[].cold). A line is in the
// tier when its service is one of the tier's, its severity number is set
// and below warn (13), its body is a string that doesn't match the error
// guard, and it isn't a record (ADR 0011). Anything else stays hot,
// including a line whose condition fails to evaluate (error_mode: ignore).

const (
	// OtelcolTierHot drops the tier's lines from the stream's pipeline.
	OtelcolTierHot = "filter/tvt-tier-hot"
	// OtelcolTierCold keeps only the tier's lines in the cold pipeline.
	OtelcolTierCold = "filter/tvt-tier-cold"
	// OtelcolTierPipeline is the pipeline that carries the tier to the
	// cold exporter.
	OtelcolTierPipeline = "logs/tvt-tier"
	// TierPrefix starts every level-tier block's ID, so the tier writer
	// strips only its own blocks.
	TierPrefix = "tier:"
	// SeverityWarn is OTLP's WARN severity number: the tier takes lines
	// below it.
	SeverityWarn = 13
)

// Tier sends the lines below warn of some services' logs to the cold
// exporter (S10 item 3).
type Tier struct {
	ID       string // tier:<pipeline>
	Scan     int64
	Evidence string
	Reason   string
	// Services are the service.name values whose lower-level lines move.
	Services []string
	// Records are the record log-event conditions (the manifest's
	// record_matches.log_events): they stay hot whatever their level.
	Records []string
	// Cold is the exporter that takes the tier (tvt.yaml pipelines[].cold):
	// one the config already defines. tvt doesn't invent a destination.
	Cold      string
	Collector string
}

func (t Tier) begin() string {
	s := fmt.Sprintf("tvt:begin %s scan=%d", t.ID, t.Scan)
	if t.Evidence != "" {
		s += " evidence=" + t.Evidence
	}
	return s
}

var otelComponentID = regexp.MustCompile(`^[a-z][a-z0-9_]*(/[A-Za-z0-9_.\-]+)?$`)

func (t Tier) validate() error {
	if !strings.HasPrefix(t.ID, TierPrefix) || strings.ContainsAny(t.ID, " \n\r") {
		return fmt.Errorf("propose: tier ID %q: want %s<name>", t.ID, TierPrefix)
	}
	if strings.ContainsAny(t.Reason, "\n\r") {
		return fmt.Errorf("propose: tier %s: reason must be one line", t.ID)
	}
	if len(t.Services) == 0 {
		return fmt.Errorf("propose: tier %s: no service (an unscoped tier moves every service's lines)", t.ID)
	}
	for _, s := range t.Services {
		if s == "" || strings.ContainsAny(s, "\n\r'") {
			return fmt.Errorf("propose: tier %s: bad service name %q", t.ID, s)
		}
	}
	for _, r := range t.Records {
		if strings.ContainsAny(r, "\n\r'") {
			return fmt.Errorf("propose: tier %s: record condition %q can't be quoted", t.ID, r)
		}
	}
	if !otelComponentID.MatchString(t.Cold) {
		return fmt.Errorf("propose: tier %s: set pipelines[].cold to the exporter that takes the cold tier (got %q)", t.ID, t.Cold)
	}
	if _, err := OTTLFormFor(t.Collector); err != nil {
		return fmt.Errorf("propose: tier %s: %w", t.ID, err)
	}
	return nil
}

// Conditions are the tier's conditions, one per service: a log is in the
// tier when any is true. They're written with log. paths and made bare
// for Collectors older than the context form.
func (t Tier) Conditions() []string {
	services := slices.Clone(t.Services)
	slices.Sort(services)
	services = slices.Compact(services)
	form, _ := OTTLFormFor(t.Collector)
	var out []string
	for _, s := range services {
		c := fmt.Sprintf(`resource.attributes["service.name"] == %s and log.severity_number > 0 and log.severity_number < %d and IsString(log.body) and not IsMatch(log.body, %s)`,
			strconv.Quote(s), SeverityWarn, strconv.Quote(ErrorPattern))
		for _, r := range t.Records {
			c += " and not (" + logContext(r) + ")"
		}
		if !form.Context {
			c = bareLogPaths(c)
		}
		out = append(out, c)
	}
	return out
}

// coldCondition drops whatever isn't in the tier: the cold filter's one
// condition.
func (t Tier) coldCondition() string {
	var parts []string
	for _, c := range t.Conditions() {
		parts = append(parts, "not ("+c+")")
	}
	return strings.Join(parts, " and ")
}

// bareLogPaths rewrites log-context paths (log.body) to bare ones
// (body), outside string literals, for Collectors before the context
// form.
func bareLogPaths(cond string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(cond); i++ {
		c := cond[i]
		if c == '"' && (i == 0 || cond[i-1] != '\\') {
			in = !in
		}
		if !in && strings.HasPrefix(cond[i:], "log.") && (i == 0 || !isIdent(cond[i-1])) {
			i += len("log.") - 1
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isTierBlock(begin string) bool { return strings.HasPrefix(begin, TierPrefix) }

// OtelcolTier adds t to the logs pipeline named pipeline: the hot filter
// first in it, and logs/tvt-tier with the pipeline's receivers and
// processors behind the cold filter, exporting to t.Cold. Existing tier
// blocks (and tvt's tier entries in flow-style lists) are removed first
// and other tvt blocks are kept: run it after OtelcolChanges, which
// strips every block. A nil t only strips.
//
// It fails rather than guess: no processors section, a pipeline that
// isn't a logs pipeline or isn't in the file, a cold exporter the file
// doesn't define or that the pipeline already exports to, or a
// logs/tvt-tier pipeline or tier filter outside a tvt block.
func OtelcolTier(src []byte, pipeline string, t *Tier) ([]byte, error) {
	out, _, err := stripBlocksIf(src, "#", isTierBlock)
	if err != nil {
		return nil, err
	}
	if out, err = stripOtelRefsOf(out, []string{OtelcolTierHot, OtelcolTierCold}); err != nil {
		return nil, err
	}
	if t == nil {
		return out, nil
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	if pipeline != "logs" && !strings.HasPrefix(pipeline, "logs/") {
		return nil, fmt.Errorf("propose: pipeline %q isn't a logs pipeline", pipeline)
	}
	if pipeline == OtelcolTierPipeline {
		return nil, fmt.Errorf("propose: %s is tvt's own tier pipeline", pipeline)
	}
	root, err := otelRoot(out)
	if err != nil {
		return nil, err
	}
	procs := value(root, "processors")
	if procs == nil || procs.Kind != yaml.MappingNode || procs.Style&yaml.FlowStyle != 0 || len(procs.Content) == 0 {
		return nil, fmt.Errorf("propose: no processors section tvt can add to (want a block mapping with at least one processor)")
	}
	for _, id := range []string{OtelcolTierHot, OtelcolTierCold} {
		if value(procs, id) != nil {
			return nil, fmt.Errorf("propose: processors already has %s outside a tvt block: tvt won't overwrite it", id)
		}
	}
	if exps := value(root, "exporters"); exps == nil || value(exps, t.Cold) == nil {
		return nil, fmt.Errorf("propose: no exporter %s for the cold tier (pipelines[].cold names one the config defines)", t.Cold)
	}
	all := value(value(root, "service"), "pipelines")
	if all == nil || all.Kind != yaml.MappingNode || all.Style&yaml.FlowStyle != 0 || len(all.Content) == 0 {
		return nil, fmt.Errorf("propose: no service.pipelines block mapping")
	}
	if value(all, OtelcolTierPipeline) != nil {
		return nil, fmt.Errorf("propose: service.pipelines already has %s outside a tvt block: tvt won't overwrite it", OtelcolTierPipeline)
	}
	p := value(all, pipeline)
	if p == nil || p.Kind != yaml.MappingNode || len(p.Content) == 0 {
		return nil, fmt.Errorf("propose: no service.pipelines.%s", pipeline)
	}
	receivers, err := flowItems(p, "receivers", pipeline)
	if err != nil {
		return nil, err
	}
	if len(receivers) == 0 {
		return nil, fmt.Errorf("propose: service.pipelines.%s has no receivers", pipeline)
	}
	exporters, err := flowItems(p, "exporters", pipeline)
	if err != nil {
		return nil, err
	}
	if slices.Contains(exporters, t.Cold) {
		return nil, fmt.Errorf("propose: %s already exports to %s: a tier there moves nothing", pipeline, t.Cold)
	}
	processors, err := flowItems(p, "processors", pipeline)
	if err != nil {
		return nil, err
	}
	type edit struct {
		line, col int
		text      string
	}
	// 1. The two filters, after the last processor.
	indent := strings.Repeat(" ", procs.Content[0].Column-1)
	var b strings.Builder
	fmt.Fprintf(&b, "%s# %s\n", indent, t.begin())
	if t.Reason != "" {
		fmt.Fprintf(&b, "%s# %s\n", indent, t.Reason)
	}
	fmt.Fprintf(&b, "%s# lines below warn leave the hot tier for %s; the error pattern and records stay (tvt S10 item 3)\n", indent, t.Cold)
	fmt.Fprintf(&b, "%s%s:\n%s  error_mode: ignore\n%s  logs:\n%s    log_record:\n", indent, OtelcolTierHot, indent, indent, indent)
	for _, c := range t.Conditions() {
		fmt.Fprintf(&b, "%s      - '%s'\n", indent, c)
	}
	fmt.Fprintf(&b, "%s%s:\n%s  error_mode: ignore\n%s  logs:\n%s    log_record:\n", indent, OtelcolTierCold, indent, indent, indent)
	fmt.Fprintf(&b, "%s      - '%s'\n", indent, t.coldCondition())
	fmt.Fprintf(&b, "%s# %s\n", indent, endMarker)
	edits := []edit{{line: pastBlockEnd(out, lastLine(procs)), text: b.String()}}
	// 2. The cold pipeline, after the last pipeline.
	pin := strings.Repeat(" ", all.Content[0].Column-1)
	coldProcs := append([]string{OtelcolTierCold}, processors...)
	edits = append(edits, edit{line: pastBlockEnd(out, lastLine(all)), text: fmt.Sprintf("%s# %s\n%s%s:\n%s  receivers: [%s]\n%s  processors: [%s]\n%s  exporters: [%s]\n%s# %s\n",
		pin, t.begin(), pin, OtelcolTierPipeline, pin, strings.Join(receivers, ", "), pin, strings.Join(coldProcs, ", "), pin, t.Cold, pin, endMarker)})
	// 3. The hot filter, first in the pipeline.
	switch pr := value(p, "processors"); {
	case pr == nil:
		in := strings.Repeat(" ", p.Content[0].Column-1)
		edits = append(edits, edit{line: lastLine(p), text: fmt.Sprintf("%s# %s\n%sprocessors: [%s]\n%s# %s\n", in, t.begin(), in, OtelcolTierHot, in, endMarker)})
	case pr.Style&yaml.FlowStyle != 0:
		text := OtelcolTierHot
		if len(pr.Content) > 0 {
			text += ", "
		}
		edits = append(edits, edit{line: pr.Line, col: pr.Column + 1, text: text})
	case len(pr.Content) == 0:
		return nil, fmt.Errorf("propose: service.pipelines.%s.processors is empty", pipeline)
	default:
		first := pr.Content[0]
		in := strings.Repeat(" ", first.Column-3)
		edits = append(edits, edit{line: first.Line - 1, text: fmt.Sprintf("%s# %s\n%s- %s\n%s# %s\n", in, t.begin(), in, OtelcolTierHot, in, endMarker)})
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
	// Re-read: the hot filter leads the pipeline, and the cold pipeline
	// carries the cold filter to the cold exporter.
	root, err = otelRoot(out)
	if err != nil {
		return nil, fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	ps := value(value(root, "service"), "pipelines")
	pr := value(value(ps, pipeline), "processors")
	if pr == nil || len(pr.Content) == 0 || pr.Content[0].Value != OtelcolTierHot {
		return nil, fmt.Errorf("propose: %s isn't first in pipeline %s after the edit", OtelcolTierHot, pipeline)
	}
	cp := value(ps, OtelcolTierPipeline)
	if cp == nil {
		return nil, fmt.Errorf("propose: the edited config lost %s", OtelcolTierPipeline)
	}
	if got, _ := flowItems(cp, "exporters", OtelcolTierPipeline); !slices.Equal(got, []string{t.Cold}) {
		return nil, fmt.Errorf("propose: %s exports to %v after the edit, want [%s]", OtelcolTierPipeline, got, t.Cold)
	}
	return out, nil
}

// flowItems is a pipeline's list under key (receivers, processors,
// exporters) as IDs; absent is empty.
func flowItems(p *yaml.Node, key, pipeline string) ([]string, error) {
	n := value(p, key)
	if n == nil {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("propose: service.pipelines.%s.%s isn't a list", pipeline, key)
	}
	var out []string
	for _, it := range n.Content {
		if it.Kind != yaml.ScalarNode || it.Value == "" {
			return nil, fmt.Errorf("propose: service.pipelines.%s.%s has an entry that isn't a component ID", pipeline, key)
		}
		out = append(out, it.Value)
	}
	return out, nil
}

// OtelcolLogsShipper is the logs pipeline a level tier goes in: the one
// logs pipeline that exports to an exporter (not only to connectors) and
// isn't tvt's tier pipeline or a cold copy (exporting only to cold). ""
// when the file has none; an error when it has several, since tvt won't
// guess which carries the stream.
func OtelcolLogsShipper(src []byte, cold string) (string, error) {
	root, err := otelRoot(src)
	if err != nil {
		return "", err
	}
	connectors := map[string]bool{}
	if cs := value(root, "connectors"); cs != nil && cs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(cs.Content); i += 2 {
			connectors[cs.Content[i].Value] = true
		}
	}
	all := value(value(root, "service"), "pipelines")
	if all == nil || all.Kind != yaml.MappingNode {
		return "", nil
	}
	var found []string
	for i := 0; i+1 < len(all.Content); i += 2 {
		name := all.Content[i].Value
		if (name != "logs" && !strings.HasPrefix(name, "logs/")) || name == OtelcolTierPipeline {
			continue
		}
		exps, err := flowItems(all.Content[i+1], "exporters", name)
		if err != nil {
			return "", err
		}
		ships, coldOnly := false, len(exps) > 0
		for _, e := range exps {
			if !connectors[e] {
				ships = true
			}
			if e != cold {
				coldOnly = false
			}
		}
		if ships && !coldOnly {
			found = append(found, name)
		}
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("propose: logs pipelines %s all ship to exporters; tvt won't guess which carries the stream", strings.Join(found, ", "))
}
