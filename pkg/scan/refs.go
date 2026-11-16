package scan

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// maxVarExpansions caps the selectors one templated matcher expands to;
// past it the matcher counts as matching every value.
const maxVarExpansions = 64

// query is one consumer's query and how to read it.
type query struct {
	consumer graph.Consumer
	expr     string
	lang     string // "promql" or "logql"
	variable bool   // a dashboard variable's query (label_values() etc.)
	vars     map[string]varValues
	shows    []string // labels a panel legend or annotation template shows
	// seen and actor: when a query log last recorded the query, and who
	// sent it then (zero for dashboards and rules).
	seen  time.Time
	actor string
	// allLabels: the consumer depends on every label of what it selects
	// (an external consumer receives the series whole).
	allLabels       bool
	includeRecorded bool
	// collectedOnly: resolve only against collected metrics, as for a
	// query that didn't parse (runbook steps: prose can look like PromQL).
	collectedOnly bool
	// unparsed: the query runs on a datasource tvt doesn't parse; it is
	// read as PromQL against collected metrics only, and every reference
	// is assumed and reads every label, with this as the reason.
	unparsed string
}

// namesMetric: the reference names its metric, by name or with a
// __name__ matcher that selects some names and not every one
// ({__name__="x"}, {__name__=~"node_.+"}).
func namesMetric(r parse.Ref) bool {
	if r.Metric != "" {
		return true
	}
	for _, m := range r.Matchers {
		if m.Label != "__name__" {
			continue
		}
		switch {
		case m.Op == parse.MatchEqual && m.Value != "":
			return true
		case m.Op == parse.MatchRegexp && m.Value != ".*" && m.Value != ".+" && m.Value != "":
			return true
		}
	}
	return false
}

// varValues are the values a dashboard variable can take. any: it can take
// values tvt doesn't know (a textbox, label_values() from a backend, an
// "All" option with a custom value), so a matcher on it matches every value.
type varValues struct {
	values []string
	any    bool
	// escaped: Grafana's Prometheus and Loki datasources regex-escape the
	// values by default (a multi-value or "All" variable), so in a regex
	// matcher they match only themselves.
	escaped bool
	// notMetricName: the variable's values can never be a metric name
	// (query_result() lines), so as a metric name it selects nothing. As a
	// label value it is still unknown (any).
	notMetricName bool
}

// refKey identifies one graph reference.
type refKey struct{ signal, consumer, via string }

// resolver turns parsed queries into references to signals. It knows the
// collected metrics and labels; metric signals for exact names it hasn't
// seen and stream signals are created on demand.
type resolver struct {
	src         string
	metrics     []string            // collected metric names, sorted
	collected   map[string]bool     // collected metric signal IDs
	known       map[string]bool     // collected or created metric signal IDs
	recorded    []string            // recording-rule output names
	labels      map[string][]string // collected label names per metric
	streams     map[string]bool     // stream signal IDs created
	newSignals  []graph.Signal
	refs        map[refKey]*graph.Reference
	direct      map[string][]refKey // consumer ID -> its direct references
	recordedBy  map[string][]string // metric signal ID -> recording rule consumer IDs writing it
	consumerEvd map[string]graph.EvidenceClass
	regexps     map[string]*regexp.Regexp // compiled anchored matcher regexes; nil = invalid
	series      map[string]int64          // collected metric name -> active series
	totalSeries int64
	// sweepShare: an observed query whose selector, naming no metric
	// outright and restricting no label, selects more than this share of
	// the collected series is a sweep.
	sweepShare float64
	// selectorMetrics: label-only selectors resolved by the backend
	// (Inputs.SelectorMetrics).
	selectorMetrics map[string][]string
}

// DefaultSweepShare is the share of series above which a query is a sweep.
const DefaultSweepShare = 0.5

func newResolver(src string, metrics []collect.MetricCardinality, labels []collect.LabelCardinality) *resolver {
	r := &resolver{
		src: src, collected: map[string]bool{}, known: map[string]bool{}, labels: map[string][]string{}, streams: map[string]bool{},
		refs: map[refKey]*graph.Reference{}, direct: map[string][]refKey{}, recordedBy: map[string][]string{},
		consumerEvd: map[string]graph.EvidenceClass{}, regexps: map[string]*regexp.Regexp{},
		series: map[string]int64{}, sweepShare: DefaultSweepShare,
	}
	for _, m := range metrics {
		r.series[m.Metric] += m.ActiveSeries
		r.totalSeries += m.ActiveSeries
		id := r.metricID(m.Metric)
		if !r.known[id] {
			r.known[id] = true
			r.collected[id] = true
			r.metrics = append(r.metrics, m.Metric)
		}
	}
	sort.Strings(r.metrics)
	for _, l := range labels {
		r.labels[l.Metric] = append(r.labels[l.Metric], l.Label)
	}
	return r
}

func (r *resolver) metricID(name string) string { return r.src + ":metric:" + name }

func (r *resolver) labelID(metric, label string) string {
	return r.src + ":label:" + metric + ":" + label
}

// metricSignal returns the ID of a metric signal, creating it for an exact
// name the scan didn't collect (a metric with no series costs nothing, but a
// consumer reading it is still evidence) unless onlyKnown is set, in which
// case it must be a collected metric.
func (r *resolver) metricSignal(name string, onlyKnown bool) (string, bool) {
	id := r.metricID(name)
	if onlyKnown && !r.collected[id] {
		return "", false
	}
	if r.known[id] {
		return id, true
	}
	r.known[id] = true
	r.newSignals = append(r.newSignals, graph.Signal{ID: id, Kind: graph.KindMetric, Name: name, Source: r.src})
	return id, true
}

// recordingRule registers a recording rule's output metric as a signal, so
// readers of it resolve, and through it, to the rule's inputs.
func (r *resolver) recordingRule(consumerID, metric string) {
	id, _ := r.metricSignal(metric, false)
	if len(r.recordedBy[id]) == 0 && !r.collected[id] {
		r.recorded = append(r.recorded, metric)
	}
	r.recordedBy[id] = append(r.recordedBy[id], consumerID)
}

// add records a reference, merging with an existing one for the same key:
// the union of labels and reasons, and the broader flags.
func (r *resolver) add(ref graph.Reference) refKey {
	k, _ := r.merge(ref)
	return k
}

// merge is add that also reports whether the stored reference changed (a
// new key, or new labels, reasons or flags).
func (r *resolver) merge(ref graph.Reference) (refKey, bool) {
	k := refKey{ref.SignalID, ref.ConsumerID, ref.Via}
	cur := r.refs[k]
	// Fresh slices every time: callers copy references built from others,
	// and an append into a shared backing array would change both.
	if cur == nil {
		ref.Labels = sortLabels(append([]graph.LabelRef(nil), ref.Labels...))
		ref.Reasons = sortStrings(append([]string(nil), ref.Reasons...))
		r.refs[k] = &ref
		return k, true
	}
	before := *cur
	cur.Labels = sortLabels(append(append([]graph.LabelRef(nil), cur.Labels...), ref.Labels...))
	cur.Reasons = sortStrings(append(append([]string(nil), cur.Reasons...), ref.Reasons...))
	cur.AllLabels = cur.AllLabels || ref.AllLabels
	cur.Assumed = cur.Assumed || ref.Assumed
	cur.ResultLabels = cur.ResultLabels || ref.ResultLabels
	// A consumer that names the signal as well as sweeping it reads it.
	cur.Sweep = cur.Sweep && ref.Sweep
	if ref.LastSeen.After(cur.LastSeen) {
		cur.LastSeen, cur.Actor = ref.LastSeen, ref.Actor
	}
	// Unions only grow, so equal lengths mean nothing new.
	changed := len(cur.Labels) != len(before.Labels) || len(cur.Reasons) != len(before.Reasons) ||
		cur.AllLabels != before.AllLabels || cur.Assumed != before.Assumed || cur.ResultLabels != before.ResultLabels || cur.Sweep != before.Sweep ||
		!cur.LastSeen.Equal(before.LastSeen)
	return k, changed
}

// resolve parses one query and adds its direct references.
func (r *resolver) resolve(q query) {
	r.consumerEvd[q.consumer.ID] = q.consumer.Kind.Evidence()
	var refs []parse.Ref
	var err error
	switch {
	case q.lang == "logql" && q.variable:
		refs, err = parse.LogVariable(q.expr)
	case q.lang == "logql":
		refs, err = parse.LogQL(q.expr)
	case q.variable:
		refs, err = parse.PromVariable(q.expr)
	default:
		refs, err = parse.PromQL(q.expr)
	}
	// A query that didn't parse yields guesses (the fallback can name "and",
	// "by" or "range"): they resolve only against collected signals and
	// never create one.
	failed := err != nil || q.collectedOnly
	for _, pr := range refs {
		pr.AllLabels = pr.AllLabels || q.allLabels
		if q.unparsed != "" {
			// Only names the query spells out: a wildcard from another
			// language's syntax (TraceQL's { .service.name = "x" }) would
			// otherwise protect every metric.
			if !namesMetric(pr) {
				continue
			}
			pr.Assumed, pr.AllLabels = true, true
			pr.Reasons = append(slices.Clone(pr.Reasons), q.unparsed)
		}
		if q.lang == "logql" {
			r.resolveStream(q, pr)
		} else {
			r.resolveMetric(q, pr, failed)
		}
	}
}

// resolveMetric adds the references of one PromQL selector.
func (r *resolver) resolveMetric(q query, pr parse.Ref, failed bool) {
	base := graph.Reference{
		ConsumerID:   q.consumer.ID,
		Evidence:     q.consumer.Kind.Evidence(),
		LastSeen:     q.seen,
		Actor:        q.actor,
		Labels:       appendUses(promLabels(pr), q.shows, graph.LabelLegend),
		AllLabels:    pr.AllLabels,
		ResultLabels: pr.ResultLabels,
		Assumed:      pr.Assumed,
		Reasons:      pr.Reasons,
	}
	names := []string{pr.Metric}
	if pr.Metric == "" {
		var why []string
		names, why = r.matchNames(pr.Matchers, q.vars, q.includeRecorded && !failed)
		if len(why) > 0 {
			base.Assumed = true
			base.Reasons = append(base.Reasons, why...)
		}
	} else if hasNameMatcher(pr.Matchers) {
		// A name plus __name__ matchers: they can exclude it, or hold a
		// variable tvt can't resolve.
		ok, why := r.namesMatch(pr.Metric, pr.Matchers, q.vars)
		if !ok {
			names = nil
		} else if len(why) > 0 {
			base.Assumed = true
			base.Reasons = append(base.Reasons, why...)
		}
	}
	if why := r.sweep(q, pr, names); why != "" {
		base.Sweep = true
		base.Reasons = append(base.Reasons, why)
	}
	for _, name := range names {
		id, ok := r.metricSignal(name, failed)
		if !ok {
			continue
		}
		ref := base
		ref.SignalID = id
		ref.Labels = append([]graph.LabelRef(nil), base.Labels...)
		ref.Reasons = append([]string(nil), base.Reasons...)
		k := r.add(ref)
		r.direct[q.consumer.ID] = append(r.direct[q.consumer.ID], k)
	}
}

// matchNames returns the metric names a selector without a plain name
// selects, from its __name__ matchers. A selector with none selects every
// metric that has its labels; tvt doesn't know which, so it selects every
// collected metric and optionally recording-rule outputs, assumed. The
// second result holds reasons for an assumed reading.
func (r *resolver) matchNames(ms []parse.Matcher, vars map[string]varValues, includeRecorded bool) ([]string, []string) {
	var reasons []string
	metrics := r.metrics
	if k, ok := SelectorKey(ms); ok && r.selectorMetrics[k] != nil {
		// The backend said which metrics carry these labels: those, still
		// filtered by any __name__ matcher below. Recording-rule outputs
		// it stores are among them.
		metrics = r.selectorMetrics[k]
	} else {
		if !hasNameMatcher(ms) {
			reasons = append(reasons, "selector names no metric, so every metric with its labels")
		}
		if includeRecorded {
			metrics = append(append([]string(nil), r.metrics...), r.recorded...)
			sort.Strings(metrics)
		}
	}
	var out []string
	for _, name := range metrics {
		ok, why := r.namesMatch(name, ms, vars)
		reasons = append(reasons, why...)
		if ok {
			out = append(out, name)
		}
	}
	return out, sortStrings(reasons)
}

// namesMatch reports whether every __name__ matcher accepts name. A matcher
// whose value is a template variable is checked against the variable's
// possible values; one tvt can't resolve (unknown values, a bad regex)
// accepts every name, and the second result says why.
func (r *resolver) namesMatch(name string, ms []parse.Matcher, vars map[string]varValues) (bool, []string) {
	var reasons []string
	for _, m := range ms {
		if m.Label != "__name__" {
			continue
		}
		op, values := m.Op, []string{m.Value}
		if m.Variable && m.Op == parse.MatchEqual && notMetricName(m.Value, vars) {
			return false, reasons // the variable holds a value, never a name
		}
		if m.Variable {
			vs, ok := expandVars(m.Value, vars, isRegex(m.Op))
			if !ok {
				pop, pattern, narrows := varPattern(m.Op, m.Value)
				if !narrows {
					reasons = append(reasons, fmt.Sprintf("metric name matcher %s%s%q has a variable with unknown values, so every metric", m.Label, m.Op, m.Value))
					continue
				}
				reasons = append(reasons, fmt.Sprintf("metric name matcher %s%s%q has a variable with unknown values, so every metric matching %s", m.Label, m.Op, m.Value, pattern))
				vs, op = []string{pattern}, pop
			}
			values = vs
		}
		any := false
		for _, v := range values {
			ok, err := r.matches(op, v, name)
			if err != nil {
				reasons = append(reasons, fmt.Sprintf("metric name matcher %s%s%q is not a valid regex, so every metric", m.Label, m.Op, v))
				ok = true
			}
			if ok {
				any = true
				break
			}
		}
		// Negative matchers must hold for the value actually chosen, which
		// could be any of them: a name is excluded only if every value
		// excludes it, which the loop above already gives.
		if !any {
			return false, reasons
		}
	}
	return true, reasons
}

// sweep says why an observed query's selector is a sweep, or "": it names
// no metric outright, restricts no label, and either matches every metric
// name or selects more than sweepShare of the collected series. Declared
// consumers (rules, SLOs, runbooks, external) never sweep: what they
// evaluate, they depend on.
func (r *resolver) sweep(q query, pr parse.Ref, names []string) string {
	if q.consumer.Kind.Evidence() != graph.EvidenceObserved || pr.Metric != "" || q.unparsed != "" {
		return ""
	}
	for _, m := range pr.Matchers {
		// A variable (a name picked on the dashboard) reads what it's set
		// to, which tvt may not know: unknown means used, never a sweep.
		if m.Variable || m.Label != "__name__" && !matchesAnything(m) {
			return ""
		}
	}
	if !namesMetric(pr) {
		return "a sweep: the query matches every metric name"
	}
	if r.totalSeries == 0 {
		return ""
	}
	var n int64
	for _, name := range names {
		n += r.series[name]
	}
	if share := float64(n) / float64(r.totalSeries); share > r.sweepShare {
		return fmt.Sprintf("a sweep: the query selects %.0f%% of series", share*100)
	}
	return ""
}

// matchesAnything: the matcher restricts nothing a series can have
// (=~".*", =~".+", !="", !~"").
func matchesAnything(m parse.Matcher) bool {
	switch m.Op {
	case parse.MatchRegexp:
		return m.Value == ".*" || m.Value == ".+"
	case parse.MatchNotEqual, parse.MatchNotRegex:
		return m.Value == ""
	}
	return false
}

func hasNameMatcher(ms []parse.Matcher) bool {
	for _, m := range ms {
		if m.Label == "__name__" {
			return true
		}
	}
	return false
}

// matches applies one matcher to a value; regexes are fully anchored, as
// in Prometheus and Loki, and compiled once per scan.
func (r *resolver) matches(op parse.MatchOp, pattern, v string) (bool, error) {
	switch op {
	case parse.MatchEqual:
		return v == pattern, nil
	case parse.MatchNotEqual:
		return v != pattern, nil
	}
	re, seen := r.regexps[pattern]
	if !seen {
		re, _ = regexp.Compile("^(?:" + pattern + ")$")
		r.regexps[pattern] = re
	}
	if re == nil {
		return false, fmt.Errorf("invalid regex %q", pattern)
	}
	return re.MatchString(v) == (op == parse.MatchRegexp), nil
}

// resolveStream adds the reference of one LogQL selector to its stream
// signal. Templated matchers expand to one selector per possible value;
// one with unknown values is dropped from the selector (it matches every
// value), assumed. A selector left with no matchers is AllStreams.
func (r *resolver) resolveStream(q query, pr parse.Ref) {
	base := graph.Reference{
		ConsumerID:   q.consumer.ID,
		Evidence:     q.consumer.Kind.Evidence(),
		LastSeen:     q.seen,
		Actor:        q.actor,
		Labels:       appendUses(logLabels(pr), q.shows, graph.LabelLegend),
		AllLabels:    pr.AllLabels,
		ResultLabels: pr.ResultLabels,
		Assumed:      pr.Assumed,
		Reasons:      pr.Reasons,
	}
	sels := [][]parse.Matcher{nil}
	for _, m := range pr.Matchers {
		if !m.Variable {
			for i := range sels {
				sels[i] = append(sels[i], m)
			}
			continue
		}
		op, values := m.Op, []string(nil)
		vs, ok := expandVars(m.Value, q.vars, isRegex(m.Op))
		if ok && len(sels)*len(vs) <= maxVarExpansions {
			values = vs
		} else {
			base.Assumed = true
			pop, pattern, narrows := varPattern(m.Op, m.Value)
			if !narrows {
				base.Reasons = append(base.Reasons, fmt.Sprintf("matcher %s%s%q has a variable with unknown values, so every value", m.Label, m.Op, m.Value))
				continue
			}
			base.Reasons = append(base.Reasons, fmt.Sprintf("matcher %s%s%q has a variable with unknown values, so %s=~%q", m.Label, m.Op, m.Value, m.Label, pattern))
			op, values = pop, []string{pattern}
		}
		var next [][]parse.Matcher
		for _, sel := range sels {
			for _, v := range values {
				next = append(next, append(append([]parse.Matcher(nil), sel...), parse.Matcher{Label: m.Label, Op: op, Value: v}))
			}
		}
		sels = next
	}
	for _, sel := range sels {
		name := AllStreams
		if len(sel) > 0 {
			name = parse.CanonicalSelector(sel)
		}
		id := "loki:stream:" + name
		if !r.streams[id] {
			r.streams[id] = true
			r.newSignals = append(r.newSignals, graph.Signal{ID: id, Kind: graph.KindStream, Name: name, Source: "loki"})
		}
		ref := base
		ref.SignalID = id
		ref.Labels = append([]graph.LabelRef(nil), base.Labels...)
		ref.Reasons = append([]string(nil), base.Reasons...)
		k := r.add(ref)
		r.direct[q.consumer.ID] = append(r.direct[q.consumer.ID], k)
	}
}

// varRefRe is Grafana's interpolation syntax, as parse accepts it: $var,
// [[var]] / [[var:format]] and ${var} / ${var.field} / ${var:format}.
var varRefRe = regexp.MustCompile(`\$(\w+)|\[\[(\w+?)(?::(\w+))?\]\]|\$\{(\w+)(?:\.([^:^}]+))?(?::([^}]+))?\}`)

// varRef is one variable reference in a matcher value.
type varRef struct{ name, field, format string }

func varRefAt(value string, loc []int) varRef {
	g := func(i int) string {
		if loc[2*i] < 0 {
			return ""
		}
		return value[loc[2*i]:loc[2*i+1]]
	}
	switch {
	case loc[2] >= 0:
		return varRef{name: g(1)}
	case loc[4] >= 0:
		return varRef{name: g(2), format: g(3)}
	}
	return varRef{name: g(4), field: g(5), format: g(6)}
}

func isRegex(op parse.MatchOp) bool { return op == parse.MatchRegexp || op == parse.MatchNotRegex }

// expandVars returns every text value can take with each variable replaced
// by one of its values, formatted as Grafana interpolates it into a regex
// matcher (regex) or an equality matcher. ok is false when a variable is
// unknown, can take values tvt doesn't know, uses a field (${var.text}) or
// a format tvt doesn't model (csv, json, ...), or the expansion is too
// large. Each value of a multi-value variable is expanded on its own:
// Grafana's (a|b) matches exactly the union.
func expandVars(value string, vars map[string]varValues, regex bool) ([]string, bool) {
	locs := varRefRe.FindAllStringSubmatchIndex(value, -1)
	if len(locs) == 0 {
		return []string{value}, true
	}
	out := []string{""}
	last := 0
	for _, loc := range locs {
		ref := varRefAt(value, loc)
		vv, ok := vars[ref.name]
		if !ok || vv.any || len(vv.values) == 0 || len(out)*len(vv.values) > maxVarExpansions || ref.field != "" {
			return nil, false
		}
		var escape bool
		switch ref.format {
		case "":
			escape = vv.escaped
		case "regex":
			escape = true
		case "raw", "pipe": // pipe joins raw values with |: the union again
		default:
			return nil, false
		}
		lit := value[last:loc[0]]
		var next []string
		for _, o := range out {
			for _, v := range vv.values {
				if regex && escape {
					v = regexp.QuoteMeta(v)
				}
				next = append(next, o+lit+v)
			}
		}
		out, last = next, loc[1]
	}
	for i := range out {
		out[i] += value[last:]
	}
	return sortStrings(out), true
}

// varPattern turns a templated matcher whose variables have unknown values
// into the regex it can still promise: literal text stays, each variable
// becomes ".*" (it can be empty). narrows is false when that says nothing
// (only variables, or a negative matcher, which could exclude anything or
// nothing) or can't be trusted: in a regex matcher, literal text with
// regex syntax (as in "[$env]") can mean something else once the variable
// is replaced, so the matcher matches every value.
func varPattern(op parse.MatchOp, value string) (parse.MatchOp, string, bool) {
	if op != parse.MatchEqual && op != parse.MatchRegexp {
		return op, "", false
	}
	var lits []string
	last := 0
	for _, loc := range varRefRe.FindAllStringIndex(value, -1) {
		lits = append(lits, value[last:loc[0]])
		last = loc[1]
	}
	lits = append(lits, value[last:])
	for i, lit := range lits {
		if regexp.QuoteMeta(lit) != lit {
			if op == parse.MatchRegexp {
				return op, "", false
			}
			lits[i] = regexp.QuoteMeta(lit)
		}
	}
	pattern := strings.Join(lits, ".*")
	if strings.Trim(pattern, ".*") == "" {
		return op, "", false
	}
	return parse.MatchRegexp, pattern, true
}

// dashboardVars returns the possible values of a dashboard's variables:
// custom and constant variables list them; metrics(regex) and
// label_values(__name__) give collected metric names; everything else can
// take any value.
func dashboardVars(d collect.Dashboard, metrics []string) map[string]varValues {
	out := map[string]varValues{}
	for _, v := range d.Variables {
		vv := varValues{any: true}
		switch v.Type {
		case "custom", "constant":
			vv = varValues{values: v.Values, any: len(v.Values) == 0}
		case "query":
			if lang, _ := collect.QueryLanguage(v.DatasourceType); lang == "promql" {
				if names, ok := metricNameValues(v.Query, metrics); ok {
					vv = varValues{values: names, any: len(names) == 0}
				} else if queryResultRe.MatchString(v.Query) && (v.Regex == "" || capturesSampleValue(v.Regex)) {
					// query_result() yields whole result lines ({...} value
					// timestamp), which can't be a metric name, and a regex
					// such as {} (.*) .* extracts the sample value: used in an
					// expression it's a value.
					vv = varValues{any: true, notMetricName: true}
				}
			}
		}
		if v.IncludeAll && v.AllValue != "" {
			vv.any = true // a custom "All" value (often .*) can match anything
		}
		vv.escaped = v.Multi || v.IncludeAll
		out[v.Name] = vv
	}
	return out
}

var metricsFuncRe = regexp.MustCompile(`^\s*metrics\((.*)\)\s*$`)
var queryResultRe = regexp.MustCompile(`^\s*query_result\(`)
var nameValuesRe = regexp.MustCompile(`^\s*label_values\(\s*(?:[^,]*,\s*)?__name__\s*\)\s*$`)

// metricNameValues resolves a variable whose values are metric names
// against the collected names.
func metricNameValues(q string, metrics []string) ([]string, bool) {
	if m := metricsFuncRe.FindStringSubmatch(q); m != nil {
		re, err := regexp.Compile(strings.TrimSpace(m[1]))
		if err != nil || strings.Contains(m[1], "$") {
			return nil, false
		}
		var out []string
		for _, n := range metrics {
			if re.MatchString(n) { // Grafana's metrics() regex is unanchored
				out = append(out, n)
			}
		}
		return out, true
	}
	if nameValuesRe.MatchString(q) {
		return metrics, true // a selector would narrow it; every name is a superset
	}
	return nil, false
}

// chains adds, for every reference to a recording rule's output, a
// reference to each of the rule's inputs with Via set to the rule that
// reads that input: metric → recording rule → alert resolves end to end,
// through any depth of rules. The input reference carries the reader's
// evidence class and the labels read along the way (labels the reader
// reads on the output can come from the input).
//
// Each rule's inputs, direct and through other rules, are worked out once
// (closures) and reused by every reader of its output. Walking the chain
// again per reader, as tvt first did, re-walked a shared subtree every
// time a label set below it grew: on Grafana Cloud's Knowledge Graph
// rules (hundreds of rules sharing outputs) that ran for many minutes.
func (r *resolver) chains() {
	closures := r.closures()
	consumers := make([]string, 0, len(r.direct))
	for c := range r.direct {
		consumers = append(consumers, c)
	}
	sort.Strings(consumers)
	for _, c := range consumers {
		for _, k := range append([]refKey(nil), r.direct[c]...) {
			out := *r.refs[k]
			for _, rule := range r.recordedBy[out.SignalID] {
				if rule == c {
					continue // a rule reading its own output depends on nothing new
				}
				for _, in := range closures[rule] {
					if in.Via == c {
						continue // a cycle back through the reader itself
					}
					r.merge(graph.Reference{
						SignalID:   in.SignalID,
						ConsumerID: c,
						Via:        in.Via,
						LastSeen:   out.LastSeen, // the reader's last read, not the rule's
						Actor:      out.Actor,
						Evidence:   r.consumerEvd[c],
						Labels:     append(append([]graph.LabelRef(nil), in.Labels...), out.Labels...),
						AllLabels:  in.AllLabels || out.AllLabels,
						Assumed:    in.Assumed || out.Assumed,
						// A rule's input labels reach the reader only if they
						// survive every rule and the reader.
						ResultLabels: in.ResultLabels && out.ResultLabels,
						Reasons:      append(append([]string(nil), in.Reasons...), out.Reasons...),
						// A sweep of a rule's output sweeps its inputs.
						Sweep: out.Sweep,
					})
				}
			}
		}
	}
}

// closureKey is one input of a rule's closure: the signal and the rule
// that reads it directly.
type closureKey struct{ signal, via string }

// closures returns, for every recording rule, every signal it reads
// directly or through the rules writing its inputs, each with the rule
// that reads it directly (Via), the union of labels read along the way,
// and ResultLabels only when the labels survive every rule. It iterates
// to a fixpoint, so cycles end once a lap adds nothing. Entries are sorted
// by signal and Via.
func (r *resolver) closures() map[string][]graph.Reference {
	var rules []string
	seen := map[string]bool{}
	for _, writers := range r.recordedBy {
		for _, w := range writers {
			if !seen[w] {
				seen[w] = true
				rules = append(rules, w)
			}
		}
	}
	sort.Strings(rules)
	cl := map[string]map[closureKey]*graph.Reference{}
	add := func(rule string, ref graph.Reference) bool {
		m := cl[rule]
		if m == nil {
			m = map[closureKey]*graph.Reference{}
			cl[rule] = m
		}
		k := closureKey{ref.SignalID, ref.Via}
		cur := m[k]
		if cur == nil {
			ref.Labels = sortLabels(append([]graph.LabelRef(nil), ref.Labels...))
			ref.Reasons = sortStrings(append([]string(nil), ref.Reasons...))
			m[k] = &ref
			return true
		}
		n, nr := len(cur.Labels), len(cur.Reasons)
		before := *cur
		cur.Labels = sortLabels(append(append([]graph.LabelRef(nil), cur.Labels...), ref.Labels...))
		cur.Reasons = sortStrings(append(append([]string(nil), cur.Reasons...), ref.Reasons...))
		cur.AllLabels = cur.AllLabels || ref.AllLabels
		cur.Assumed = cur.Assumed || ref.Assumed
		cur.ResultLabels = cur.ResultLabels || ref.ResultLabels
		return len(cur.Labels) != n || len(cur.Reasons) != nr || cur.AllLabels != before.AllLabels ||
			cur.Assumed != before.Assumed || cur.ResultLabels != before.ResultLabels
	}
	snapshot := func(rule string) []graph.Reference {
		out := make([]graph.Reference, 0, len(cl[rule]))
		for _, ref := range cl[rule] {
			out = append(out, *ref)
		}
		return out
	}
	for changed := true; changed; {
		changed = false
		for _, rule := range rules {
			for _, k := range r.direct[rule] {
				in := *r.refs[k]
				in.Via, in.ConsumerID = rule, rule
				changed = add(rule, in) || changed
				for _, writer := range r.recordedBy[in.SignalID] {
					if writer == rule {
						continue // a rule reading its own output depends on nothing new
					}
					for _, deep := range snapshot(writer) {
						changed = add(rule, graph.Reference{
							SignalID:     deep.SignalID,
							ConsumerID:   rule,
							Via:          deep.Via,
							Labels:       append(append([]graph.LabelRef(nil), deep.Labels...), in.Labels...),
							AllLabels:    deep.AllLabels || in.AllLabels,
							Assumed:      deep.Assumed || in.Assumed,
							ResultLabels: deep.ResultLabels && in.ResultLabels,
							Reasons:      append(append([]string(nil), deep.Reasons...), in.Reasons...),
						}) || changed
					}
				}
			}
		}
	}
	out := map[string][]graph.Reference{}
	for _, rule := range rules {
		refs := snapshot(rule)
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].SignalID != refs[j].SignalID {
				return refs[i].SignalID < refs[j].SignalID
			}
			return refs[i].Via < refs[j].Via
		})
		out[rule] = refs
	}
	return out
}

// labelRefs adds, for every reference to a metric, a reference to each
// collected label signal of that metric the consumer reads (all of them for
// an AllLabels reference). Label signals are never created here.
func (r *resolver) labelRefs() {
	prefix := r.src + ":metric:"
	for _, ref := range r.sorted() {
		metric, ok := strings.CutPrefix(ref.SignalID, prefix)
		if !ok {
			continue
		}
		for _, label := range r.labels[metric] {
			var uses []graph.LabelRef
			for _, l := range ref.Labels {
				if l.Name == label {
					uses = append(uses, l)
				}
			}
			if len(uses) == 0 && !ref.AllLabels {
				continue
			}
			lr := ref
			lr.SignalID = r.labelID(metric, label)
			lr.Labels = uses
			r.add(lr)
		}
	}
}

// sorted returns the references ordered by signal, consumer and via.
func (r *resolver) sorted() []graph.Reference {
	out := make([]graph.Reference, 0, len(r.refs))
	for _, ref := range r.refs {
		out = append(out, *ref)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.SignalID != b.SignalID {
			return a.SignalID < b.SignalID
		}
		if a.ConsumerID != b.ConsumerID {
			return a.ConsumerID < b.ConsumerID
		}
		return a.Via < b.Via
	})
	return out
}

func promLabels(pr parse.Ref) []graph.LabelRef {
	var ls []graph.LabelRef
	for _, m := range pr.Matchers {
		if m.Label != "__name__" {
			ls = append(ls, graph.LabelRef{Name: m.Label, Use: graph.LabelMatch})
		}
	}
	ls = appendUses(ls, pr.Grouping, graph.LabelGroup)
	ls = appendUses(ls, pr.Without, graph.LabelWithout)
	return appendUses(ls, pr.Created, graph.LabelCreated)
}

// logLabels is promLabels plus the labels the pipeline reads. The
// extractor lists matcher labels among StreamLabels too; "match" already
// says they are read, so they aren't repeated as "filter".
func logLabels(pr parse.Ref) []graph.LabelRef {
	ls := promLabels(pr)
	matched := map[string]bool{}
	for _, m := range pr.Matchers {
		matched[m.Label] = true
	}
	for _, l := range pr.StreamLabels {
		if !matched[l] {
			ls = appendUses(ls, []string{l}, graph.LabelFilter)
		}
	}
	return appendUses(ls, pr.Extracted, graph.LabelExtracted)
}

func appendUses(ls []graph.LabelRef, names []string, use graph.LabelUse) []graph.LabelRef {
	for _, n := range names {
		if n != "" && n != "__name__" {
			ls = append(ls, graph.LabelRef{Name: n, Use: use})
		}
	}
	return ls
}

func sortLabels(ls []graph.LabelRef) []graph.LabelRef {
	if len(ls) == 0 {
		return nil
	}
	sort.Slice(ls, func(i, j int) bool {
		if ls[i].Name != ls[j].Name {
			return ls[i].Name < ls[j].Name
		}
		return ls[i].Use < ls[j].Use
	})
	out := ls[:1]
	for _, l := range ls[1:] {
		if l != out[len(out)-1] {
			out = append(out, l)
		}
	}
	return out
}

func sortStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	sort.Strings(s)
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

var templateLabelRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// shownLabels returns the label names {{label}} templates (a panel's
// legendFormat, an annotation's title and text) and comma-separated tag
// keys show.
func shownLabels(templates []string, tagKeys string) []string {
	var out []string
	for _, t := range templates {
		for _, m := range templateLabelRe.FindAllStringSubmatch(t, -1) {
			out = append(out, m[1])
		}
	}
	for _, k := range strings.Split(tagKeys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return sortStrings(out)
}

// notMetricName reports whether value is exactly one variable whose
// values can never be a metric name.
func notMetricName(value string, vars map[string]varValues) bool {
	locs := varRefRe.FindAllStringSubmatchIndex(value, -1)
	if len(locs) != 1 || locs[0][0] != 0 || locs[0][1] != len(value) {
		return false
	}
	return vars[varRefAt(value, locs[0]).name].notMetricName
}

// capturesSampleValue reports whether a query_result variable's regex
// captures the sample value: its first capture group comes right after
// the label set's closing brace, as in Grafana's usual {} (.*) .*.
func capturesSampleValue(regex string) bool {
	r := strings.TrimSpace(regex)
	if strings.HasPrefix(r, "/") {
		if i := strings.LastIndex(r, "/"); i > 0 {
			r = r[1:i]
		}
	}
	for i := 0; i < len(r); i++ {
		if r[i] == '\\' {
			i++
			continue
		}
		if r[i] == '(' && !strings.HasPrefix(r[i:], "(?") {
			before := strings.TrimRight(r[:i], " ")
			before = strings.TrimSuffix(before, `\s+`)
			before = strings.TrimSuffix(before, `\s*`)
			return strings.HasSuffix(before, "}")
		}
	}
	return false
}
