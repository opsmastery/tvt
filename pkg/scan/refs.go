package scan

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

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
}

// varValues are the values a dashboard variable can take. any: it can take
// values tvt doesn't know (a textbox, label_values() from a backend, an
// "All" option with a custom value), so a matcher on it matches every value.
type varValues struct {
	values []string
	any    bool
}

// refKey identifies one graph reference.
type refKey struct{ signal, consumer, via string }

// resolver turns parsed queries into references to signals. It knows the
// collected metrics and labels; metric signals for exact names it hasn't
// seen and stream signals are created on demand.
type resolver struct {
	src         string
	metrics     []string            // collected metric names, sorted
	known       map[string]bool     // collected or created metric signal IDs
	labels      map[string][]string // collected label names per metric
	streams     map[string]bool     // stream signal IDs created
	newSignals  []graph.Signal
	refs        map[refKey]*graph.Reference
	direct      map[string][]refKey // consumer ID -> its direct references
	recordedBy  map[string][]string // metric signal ID -> recording rule consumer IDs writing it
	consumerEvd map[string]graph.EvidenceClass
	regexps     map[string]*regexp.Regexp // compiled anchored matcher regexes; nil = invalid
}

func newResolver(src string, metrics []collect.MetricCardinality, labels []collect.LabelCardinality) *resolver {
	r := &resolver{
		src: src, known: map[string]bool{}, labels: map[string][]string{}, streams: map[string]bool{},
		refs: map[refKey]*graph.Reference{}, direct: map[string][]refKey{}, recordedBy: map[string][]string{},
		consumerEvd: map[string]graph.EvidenceClass{}, regexps: map[string]*regexp.Regexp{},
	}
	for _, m := range metrics {
		id := r.metricID(m.Metric)
		if !r.known[id] {
			r.known[id] = true
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
// consumer reading it is still evidence) unless onlyKnown is set.
func (r *resolver) metricSignal(name string, onlyKnown bool) (string, bool) {
	id := r.metricID(name)
	if r.known[id] {
		return id, true
	}
	if onlyKnown {
		return "", false
	}
	r.known[id] = true
	r.newSignals = append(r.newSignals, graph.Signal{ID: id, Kind: graph.KindMetric, Name: name, Source: r.src})
	return id, true
}

// recordingRule registers a recording rule's output metric as a signal, so
// readers of it resolve, and through it, to the rule's inputs.
func (r *resolver) recordingRule(consumerID, metric string) {
	id, _ := r.metricSignal(metric, false)
	r.recordedBy[id] = append(r.recordedBy[id], consumerID)
}

// add records a reference, merging with an existing one for the same key:
// the union of labels and reasons, and the broader flags.
func (r *resolver) add(ref graph.Reference) refKey {
	k := refKey{ref.SignalID, ref.ConsumerID, ref.Via}
	cur := r.refs[k]
	// Fresh slices every time: callers copy references built from others,
	// and an append into a shared backing array would change both.
	if cur == nil {
		ref.Labels = sortLabels(append([]graph.LabelRef(nil), ref.Labels...))
		ref.Reasons = sortStrings(append([]string(nil), ref.Reasons...))
		r.refs[k] = &ref
		return k
	}
	cur.Labels = sortLabels(append(append([]graph.LabelRef(nil), cur.Labels...), ref.Labels...))
	cur.Reasons = sortStrings(append(append([]string(nil), cur.Reasons...), ref.Reasons...))
	cur.AllLabels = cur.AllLabels || ref.AllLabels
	cur.Assumed = cur.Assumed || ref.Assumed
	return k
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
	failed := err != nil
	for _, pr := range refs {
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
		ConsumerID: q.consumer.ID,
		Evidence:   q.consumer.Kind.Evidence(),
		Labels:     promLabels(pr),
		AllLabels:  pr.AllLabels,
		Assumed:    pr.Assumed,
		Reasons:    pr.Reasons,
	}
	names := []string{pr.Metric}
	if pr.Metric == "" {
		var why []string
		names, why = r.matchNames(pr.Matchers, q.vars)
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
	for _, name := range names {
		id, ok := r.metricSignal(name, failed || pr.Metric == "")
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

// matchNames returns the collected metric names a selector without a plain
// name selects, from its __name__ matchers. A selector with none selects
// every metric that has its labels; tvt doesn't know which, so it selects
// every collected metric, assumed. The second result holds reasons for an
// assumed reading.
func (r *resolver) matchNames(ms []parse.Matcher, vars map[string]varValues) ([]string, []string) {
	var reasons []string
	if !hasNameMatcher(ms) {
		reasons = append(reasons, "selector names no metric, so every metric with its labels")
	}
	var out []string
	for _, name := range r.metrics {
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
		if m.Variable {
			vs, ok := expandVars(m.Value, vars)
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
		ConsumerID: q.consumer.ID,
		Evidence:   q.consumer.Kind.Evidence(),
		Labels:     logLabels(pr),
		AllLabels:  pr.AllLabels,
		Assumed:    pr.Assumed,
		Reasons:    pr.Reasons,
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
		vs, ok := expandVars(m.Value, q.vars)
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

var varRefRe = regexp.MustCompile(`\$\{(\w+)(?::[^}]*)?\}|\[\[(\w+)(?::[^\]]*)?\]\]|\$(\w+)`)

// expandVars returns every text value can take with each variable replaced
// by one of its values. ok is false when a variable is unknown, can take
// values tvt doesn't know, or the expansion is too large.
func expandVars(value string, vars map[string]varValues) ([]string, bool) {
	locs := varRefRe.FindAllStringSubmatchIndex(value, -1)
	if len(locs) == 0 {
		return []string{value}, true
	}
	out := []string{""}
	last := 0
	for _, loc := range locs {
		name := ""
		for g := 1; g <= 3; g++ {
			if loc[2*g] >= 0 {
				name = value[loc[2*g]:loc[2*g+1]]
			}
		}
		vv, ok := vars[name]
		if !ok || vv.any || len(vv.values) == 0 || len(out)*len(vv.values) > maxVarExpansions {
			return nil, false
		}
		lit := value[last:loc[0]]
		var next []string
		for _, o := range out {
			for _, v := range vv.values {
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
// nothing), so the matcher matches every value.
func varPattern(op parse.MatchOp, value string) (parse.MatchOp, string, bool) {
	if op != parse.MatchEqual && op != parse.MatchRegexp {
		return op, "", false
	}
	var b strings.Builder
	last := 0
	for _, loc := range varRefRe.FindAllStringIndex(value, -1) {
		lit := value[last:loc[0]]
		if op == parse.MatchEqual {
			lit = regexp.QuoteMeta(lit)
		}
		b.WriteString(lit + ".*")
		last = loc[1]
	}
	lit := value[last:]
	if op == parse.MatchEqual {
		lit = regexp.QuoteMeta(lit)
	}
	b.WriteString(lit)
	pattern := b.String()
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
			if v.DatasourceType == "prometheus" {
				if names, ok := metricNameValues(v.Query, metrics); ok {
					vv = varValues{values: names, any: len(names) == 0}
				}
			}
		}
		if v.IncludeAll && v.AllValue != "" {
			vv.any = true // a custom "All" value (often .*) can match anything
		}
		out[v.Name] = vv
	}
	return out
}

var metricsFuncRe = regexp.MustCompile(`^\s*metrics\((.*)\)\s*$`)
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
// reference to each of the rule's inputs with Via set to that rule: metric
// → recording rule → alert resolves end to end, through any depth of
// rules. The input reference carries the reader's evidence class and the
// labels both read (labels the reader reads on the output can come from the
// input).
func (r *resolver) chains() {
	consumers := make([]string, 0, len(r.direct))
	for c := range r.direct {
		consumers = append(consumers, c)
	}
	sort.Strings(consumers)
	for _, c := range consumers {
		for _, k := range append([]refKey(nil), r.direct[c]...) {
			r.follow(c, *r.refs[k], map[string]bool{c: true})
		}
	}
}

func (r *resolver) follow(consumer string, out graph.Reference, seen map[string]bool) {
	for _, rule := range r.recordedBy[out.SignalID] {
		if seen[rule] {
			continue
		}
		seen[rule] = true
		for _, k := range r.direct[rule] {
			in := *r.refs[k]
			ref := graph.Reference{
				SignalID:   in.SignalID,
				ConsumerID: consumer,
				Via:        rule,
				Evidence:   r.consumerEvd[consumer],
				Labels:     append(append([]graph.LabelRef(nil), in.Labels...), out.Labels...),
				AllLabels:  in.AllLabels || out.AllLabels,
				Assumed:    in.Assumed || out.Assumed,
				Reasons:    append(append([]string(nil), in.Reasons...), out.Reasons...),
			}
			r.add(ref)
			r.follow(consumer, ref, seen)
		}
		delete(seen, rule)
	}
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
