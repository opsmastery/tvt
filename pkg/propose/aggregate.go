package propose

import (
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Delta counters (S7 item 8). Removing a per-session label (session.id,
// user.email) from a delta-temporality counter merges every session into
// one series. After delta_to_cumulative that breaks the counter (sessions
// come and go, so the sum falls and looks like a reset); before it, the
// merged deltas must be summed across a window, because sessions export
// separately and delta_to_cumulative keeps one point per stream and
// interval. So tvt writes the ordered pipeline, ahead of
// delta_to_cumulative: delete the labels (transform), sum each stream's
// deltas over a window (interval), then delta_to_cumulative as before.
//
// Checked on a live otelcol-contrib 0.165.0 with two sessions sending 5
// and 3 tokens a round for three rounds: with the window the cumulative
// series reads 8, 16, 24 whether the sessions export together or apart;
// without it, apart, it reads 5, 10, 15 (one session lost each round).

const (
	// OtelcolAggregateProcessor deletes the labels.
	OtelcolAggregateProcessor = "transform/tvt_aggregate"
	// OtelcolWindowProcessor sums the merged deltas over a window.
	OtelcolWindowProcessor = "interval/tvt"
	// aggregateSince is the oldest Collector the ordered pipeline was
	// checked on (context-qualified paths in datapoint statements).
	aggregateSince = "0.122.0"
	// DefaultWindow covers the OTel SDKs' default 60 s export interval.
	DefaultWindow = "60s"
)

// tvtProcessors are every processor tvt writes; a rerun takes them out of
// flow-style processors lists.
var tvtProcessors = []string{OtelcolProcessor, OtelcolAggregateProcessor, OtelcolWindowProcessor, OtelcolDedupProcessor, OtelcolTierHot, OtelcolTierCold}

// Aggregate removes labels from delta counters in an OTel Collector
// pipeline. Labels maps each metric (as Prometheus stores it) to the
// labels to remove (as Prometheus stores them); the collector matches the
// OTel names that translate to them.
type Aggregate struct {
	ID, Evidence, Reason string
	Scan                 int64
	Labels               map[string][]string
	// Buckets maps each metric to labels whose values are bucketed to
	// their class, the leading digit and 00 (S10 item 1): the merged
	// series are summed by the same window.
	Buckets map[string][]string
	// Window is the summing window (default DefaultWindow).
	Window string
	// Collector is the target's version: the ordered pipeline needs
	// v0.122.0 or later.
	Collector string
}

func (a Aggregate) begin() string { return Drop{ID: a.ID, Scan: a.Scan, Evidence: a.Evidence}.begin() }

// statements are the transform's datapoint statements: one delete per
// label, where the metric is one that carries it.
func (a Aggregate) statements() ([]string, error) {
	byLabel := map[string][]string{}
	for m, ls := range a.Labels {
		if !metricName.MatchString(m) {
			return nil, fmt.Errorf("propose: aggregate %s: %q is not a metric name", a.ID, m)
		}
		if _, ok := OtelConditions(m); !ok {
			return nil, fmt.Errorf("propose: aggregate %s: %s may be one series of an OTel histogram", a.ID, m)
		}
		for _, l := range ls {
			if !labelName.MatchString(l) || strings.HasPrefix(l, "__") {
				return nil, fmt.Errorf("propose: aggregate %s: %q is not a label tvt removes", a.ID, l)
			}
			byLabel[l] = append(byLabel[l], m)
		}
	}
	labels := make([]string, 0, len(byLabel))
	for l := range byLabel {
		labels = append(labels, l)
	}
	slices.Sort(labels)
	out, err := a.bucketStatements()
	if err != nil {
		return nil, err
	}
	for _, l := range labels {
		ms := byLabel[l]
		slices.Sort(ms)
		var where []string
		for _, m := range ms {
			cs, _ := OtelConditions(m)
			for _, c := range cs {
				where = append(where, "("+c+")")
			}
		}
		key := "^" + strings.Join(strings.Split(l, "_"), sep) + "$"
		out = append(out, fmt.Sprintf(`delete_matching_keys(datapoint.attributes, "%s") where %s`, key, strings.Join(where, " or ")))
	}
	return out, nil
}

var labelName = metricName

// bucketValue is the class a value buckets to: its leading digit and 00.
// "$$" is the Collector config's escape for "$", so OTTL sees ${1}.
const bucketPattern, bucketReplacement = `^([0-9])[0-9][0-9]$`, `$${1}00`

// bucketStatements rewrite each bucketed label to its class, where the
// metric is one that carries it. The OTel attribute's key is the label's
// name, or the same with dots (http_response_status_code may be
// http.response.status_code): one statement each, a no-op where the key
// is absent.
func (a Aggregate) bucketStatements() ([]string, error) {
	byLabel := map[string][]string{}
	for m, ls := range a.Buckets {
		if !metricName.MatchString(m) {
			return nil, fmt.Errorf("propose: bucket %s: %q is not a metric name", a.ID, m)
		}
		if _, ok := OtelConditions(m); !ok {
			return nil, fmt.Errorf("propose: bucket %s: %s may be one series of an OTel histogram", a.ID, m)
		}
		for _, l := range ls {
			if !labelName.MatchString(l) || strings.HasPrefix(l, "__") || l == "job" || l == "instance" {
				return nil, fmt.Errorf("propose: bucket %s: %q is not a label tvt buckets", a.ID, l)
			}
			byLabel[l] = append(byLabel[l], m)
		}
	}
	labels := make([]string, 0, len(byLabel))
	for l := range byLabel {
		labels = append(labels, l)
	}
	slices.Sort(labels)
	var out []string
	for _, l := range labels {
		ms := byLabel[l]
		slices.Sort(ms)
		var where []string
		for _, m := range ms {
			cs, _ := OtelConditions(m)
			for _, c := range cs {
				where = append(where, "("+c+")")
			}
		}
		keys, err := attributeSpellings(l)
		if err != nil {
			return nil, fmt.Errorf("propose: bucket %s: %w", a.ID, err)
		}
		conds := strings.Join(where, " or ")
		for _, k := range keys {
			// Semantic conventions make status codes ints, which
			// replace_pattern leaves alone (checked live on 0.165.0: an
			// int 503 kept its own series). Prometheus renders an int
			// and its string the same, so casting first changes no label.
			attr := fmt.Sprintf(`datapoint.attributes["%s"]`, k)
			out = append(out,
				fmt.Sprintf(`set(%s, String(%s)) where IsInt(%s) and (%s)`, attr, attr, attr, conds),
				fmt.Sprintf(`replace_pattern(%s, "%s", "%s") where %s`, attr, bucketPattern, bucketReplacement, conds))
		}
	}
	return out, nil
}

// maxSpellingSeparators caps attributeSpellings at 16 keys.
const maxSpellingSeparators = 4

// attributeSpellings lists the OTel attribute keys that Prometheus's
// translation turns into label l: each underscore may have been an
// underscore or a dot (http.response.status_code is stored as
// http_response_status_code). replace_pattern takes one exact key, so a
// bucket writes a statement per spelling; label removal matches them all
// with one delete_matching_keys regex instead. The label as stored comes
// first, then the rest in order.
func attributeSpellings(l string) ([]string, error) {
	parts := strings.Split(l, "_")
	n := len(parts) - 1
	if n > maxSpellingSeparators {
		return nil, fmt.Errorf("%q has %d separators; tvt writes a bucket for at most %d", l, n, maxSpellingSeparators)
	}
	out := make([]string, 0, 1<<n)
	for mask := 0; mask < 1<<n; mask++ {
		var b strings.Builder
		b.WriteString(parts[0])
		for i := 1; i <= n; i++ {
			if mask&(1<<(i-1)) != 0 {
				b.WriteByte('.')
			} else {
				b.WriteByte('_')
			}
			b.WriteString(parts[i])
		}
		out = append(out, b.String())
	}
	return out, nil
}

// otelAddAggregate writes the ordered pipeline into a config with no tvt
// blocks: the two processors, then both listed just before
// delta_to_cumulative in each named pipeline. A pipeline without
// delta_to_cumulative is an error: there's no delta counter to protect,
// and a cumulative counter can't have labels removed in the collector
// this way.
func otelAddAggregate(out []byte, pipelines []string, a Aggregate) ([]byte, error) {
	if a.ID == "" || strings.ContainsAny(a.ID+a.Reason, "\n\r") || len(a.Labels)+len(a.Buckets) == 0 {
		return nil, fmt.Errorf("propose: aggregate needs a one-line ID and reason and at least one label")
	}
	if a.Window == "" {
		a.Window = DefaultWindow
	}
	form, err := OTTLFormFor(a.Collector)
	if err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	if !form.Context {
		return nil, fmt.Errorf("propose: aggregate %s: the ordered pipeline is checked on Collector v%s and later; set pipelines[].version", a.ID, aggregateSince)
	}
	stmts, err := a.statements()
	if err != nil {
		return nil, err
	}
	for _, st := range stmts {
		if err := lintOTTL(st, form); err != nil {
			return nil, err
		}
	}
	root, err := otelRoot(out)
	if err != nil {
		return nil, err
	}
	procs := value(root, "processors")
	if procs == nil || procs.Kind != yaml.MappingNode || procs.Style&yaml.FlowStyle != 0 || len(procs.Content) == 0 {
		return nil, fmt.Errorf("propose: no processors section tvt can add to")
	}
	for _, p := range []string{OtelcolAggregateProcessor, OtelcolWindowProcessor} {
		if value(procs, p) != nil {
			return nil, fmt.Errorf("propose: processors already has %s outside a tvt block: tvt won't overwrite it", p)
		}
	}
	all := value(value(root, "service"), "pipelines")
	if all == nil {
		return nil, fmt.Errorf("propose: no service.pipelines")
	}
	type edit struct {
		line, col int
		text      string
	}
	indent := strings.Repeat(" ", procs.Content[0].Column-1)
	var b strings.Builder
	fmt.Fprintf(&b, "%s# %s\n", indent, a.begin())
	if a.Reason != "" {
		fmt.Fprintf(&b, "%s# %s\n", indent, a.Reason)
	}
	fmt.Fprintf(&b, "%s# delete or bucket the labels, sum over %s, then delta_to_cumulative (tvt S7 item 8, S10 item 1)\n", indent, a.Window)
	fmt.Fprintf(&b, "%s%s:\n%s  error_mode: ignore\n%s  metric_statements:\n%s    - context: datapoint\n%s      statements:\n", indent, OtelcolAggregateProcessor, indent, indent, indent, indent)
	for _, st := range stmts {
		fmt.Fprintf(&b, "%s        - '%s'\n", indent, st)
	}
	fmt.Fprintf(&b, "%s%s:\n%s  interval: %s\n", indent, OtelcolWindowProcessor, indent, a.Window)
	fmt.Fprintf(&b, "%s# %s\n", indent, endMarker)
	edits := []edit{{line: pastBlockEnd(out, lastLine(procs)), text: b.String()}}
	names := slices.Clone(pipelines)
	slices.Sort(names)
	names = slices.Compact(names)
	for _, name := range names {
		p := value(all, name)
		if p == nil {
			return nil, fmt.Errorf("propose: no service.pipelines.%s", name)
		}
		pr := value(p, "processors")
		if pr == nil || pr.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("propose: pipeline %s has no processors list", name)
		}
		at := -1
		for i, it := range pr.Content {
			if it.Value == "delta_to_cumulative" || it.Value == "deltatocumulative" || strings.HasPrefix(it.Value, "delta_to_cumulative/") || strings.HasPrefix(it.Value, "deltatocumulative/") {
				at = i
				break
			}
		}
		if at < 0 {
			return nil, fmt.Errorf("propose: pipeline %s has no delta_to_cumulative processor: tvt removes labels in the collector only from delta counters", name)
		}
		it := pr.Content[at]
		pair := OtelcolAggregateProcessor + ", " + OtelcolWindowProcessor
		if pr.Style&yaml.FlowStyle != 0 {
			edits = append(edits, edit{line: it.Line, col: it.Column, text: pair + ", "})
			continue
		}
		ind := strings.Repeat(" ", it.Column-3)
		edits = append(edits, edit{line: it.Line - 1, text: fmt.Sprintf("%s# %s\n%s- %s\n%s- %s\n%s# %s\n", ind, a.begin(), ind, OtelcolAggregateProcessor, ind, OtelcolWindowProcessor, ind, endMarker)})
	}
	slices.SortStableFunc(edits, func(x, y edit) int {
		if x.line != y.line {
			return y.line - x.line
		}
		return y.col - x.col
	})
	for _, e := range edits {
		if e.col == 0 {
			out = insertAfterLine(out, e.line, e.text)
		} else {
			out = insertAt(out, e.line, e.col, e.text)
		}
	}
	// Re-read: both processors come right before delta_to_cumulative.
	root, err = otelRoot(out)
	if err != nil {
		return nil, fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	all = value(value(root, "service"), "pipelines")
	for _, name := range names {
		var ps []string
		for _, it := range value(value(all, name), "processors").Content {
			ps = append(ps, it.Value)
		}
		i := slices.Index(ps, OtelcolAggregateProcessor)
		if i < 0 || i+2 >= len(ps) || ps[i+1] != OtelcolWindowProcessor || !strings.HasPrefix(strings.ReplaceAll(ps[i+2], "_", ""), "deltatocumulative") {
			return nil, fmt.Errorf("propose: pipeline %s: the ordered pipeline isn't delete, window, delta_to_cumulative", name)
		}
	}
	return out, nil
}
