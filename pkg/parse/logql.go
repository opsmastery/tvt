package parse

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/prometheus/prometheus/promql/parser"
)

// LogQL returns one Ref per stream selector in a LogQL query (ADR 0005:
// written from the public LogQL documentation, no Loki code).
//
// The log half (selector and pipeline) is parsed here. The metric half
// follows PromQL closely, so each log query is replaced with a placeholder
// metric and the result goes through the PromQL walker, which supplies
// Grouping, Without, AllLabels and label_replace's Created for each one.
//
// Labels a query reads are split three ways, because only stream labels
// map to loki:stream signals:
//
//   - StreamLabels: the selector's labels, plus labels read before any
//     parser stage, or with no parser stage in the query. These can only
//     be stream labels (or structured metadata).
//   - Extracted: labels read that a parser stage (json, logfmt, regexp,
//     pattern, unpack) may have produced: a name the stage declares, any
//     name after json/logfmt/unpack without parameters, or a *_extracted
//     name. Loki renames a parsed label that collides with a stream label
//     to <name>_extracted, so a name here is a stream label exactly when
//     the stream carries it; resolve against the stream's labels.
//   - neither: labels label_format or label_replace wrote.
//
// Filters lists every label the pipeline reads (label filters, unwrap,
// line_format and label_format templates and sources, keep, drop).
// Line filters read no labels.
//
// Grafana variables are handled as in PromQL. A query that doesn't parse
// returns the error plus an assumed, AllLabels Ref per stream selector
// found in the text, or one Ref with no matchers (every stream) if none is
// found. An empty query returns no Refs and no error.
func LogQL(query string) ([]Ref, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	refs, err := logQL(query)
	if err != nil {
		return logFallback(query, err), err
	}
	return refs, nil
}

func logQL(query string) ([]Ref, error) {
	if !utf8.ValidString(query) {
		return nil, errors.New("logql: query is not valid UTF-8")
	}
	text, vars := substituteGrafanaVars(query)
	promText, logs, err := rewriteLogQueries(text, vars)
	if err != nil {
		return nil, err
	}
	if len(logs) == 0 {
		return nil, errors.New("logql: no stream selector")
	}
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(promText)
	if err != nil {
		return nil, fmt.Errorf("logql: %w", err)
	}
	w := promWalker{vars: vars}
	w.walk(expr, scope{})
	refs := make([]Ref, 0, len(w.refs))
	for _, pr := range w.refs {
		m := logPlaceholderRe.FindStringSubmatch(pr.Metric)
		if m == nil {
			return nil, fmt.Errorf("logql: %q is not a log query", pr.Metric)
		}
		n, _ := strconv.Atoi(m[1])
		if n >= len(logs) {
			return nil, fmt.Errorf("logql: bad placeholder %q", pr.Metric)
		}
		refs = append(refs, logs[n].ref(pr))
	}
	return refs, nil
}

const logPlaceholderPrefix = "__tvtlog"

var logPlaceholderRe = regexp.MustCompile(`^` + logPlaceholderPrefix + `(\d+)__$`)

// logQuery is what the log half of one stream selector and its pipeline
// tells us.
type logQuery struct {
	matchers  []Matcher
	filters   []string
	stream    []string        // labels read that can only be stream labels
	extracted []string        // labels read that a parser may have produced
	formatted map[string]bool // written by label_format
	parsed    map[string]bool // names a parser stage declares
	anyParser bool            // json/logfmt/unpack without parameters ran
	hasParser bool
	hasRange  bool
	reasons   []string
	allLabels bool
	keeps     bool // a keep stage: only the labels it names survive
}

// use records a label the query reads at this point of the pipeline.
func (lq *logQuery) use(label string, vars []grafanaVar, filter bool) {
	if restored, found := restoreVars(label, vars); len(found) > 0 {
		lq.allLabels = true
		lq.reasons = append(lq.reasons, fmt.Sprintf("label %s comes from variable %s", restored, varTexts(found)))
		return
	}
	if isInternalLabel(label) {
		return
	}
	if filter {
		lq.filters = append(lq.filters, label)
	}
	switch {
	case lq.formatted[label]:
	case lq.hasParser && strings.HasSuffix(label, "_extracted"):
		// A parser names a field x_extracted only when the stream already
		// has a label x, so the stream label is read too.
		lq.extracted = append(lq.extracted, label)
		if base := strings.TrimSuffix(label, "_extracted"); base != "" {
			lq.stream = append(lq.stream, base)
		}
	case lq.anyParser || lq.parsed[label]:
		lq.extracted = append(lq.extracted, label)
	default:
		lq.stream = append(lq.stream, label)
	}
}

// useTemplate records the labels a line_format or label_format template
// reads.
func (lq *logQuery) useTemplate(tmpl string, vars []grafanaVar) {
	fields, whole := templateFields(tmpl)
	for _, f := range fields {
		lq.use(f, vars, true)
	}
	if whole {
		lq.allLabels = true
		lq.reasons = append(lq.reasons, "a template reads the whole label set")
	}
}

// declare records a label a parser stage produces.
func (lq *logQuery) declare(label string, vars []grafanaVar) {
	if restored, found := restoreVars(label, vars); len(found) > 0 {
		// Unknown name: anything read after this may be parsed.
		lq.anyParser = true
		lq.reasons = append(lq.reasons, fmt.Sprintf("parsed label %s comes from variable %s", restored, varTexts(found)))
		return
	}
	lq.parsed[label] = true
}

// isInternalLabel: Loki's own labels such as __error__ and __line__.
func isInternalLabel(l string) bool {
	return len(l) > 4 && strings.HasPrefix(l, "__") && strings.HasSuffix(l, "__") && !strings.Contains(l, placeholderPrefix)
}

// ref merges the log half with what the PromQL walker found for the
// placeholder.
func (lq *logQuery) ref(pr Ref) Ref {
	// Grouping and without apply after the whole pipeline. A label_replace
	// output still counts as a read: when its regex doesn't match, the
	// label keeps whatever value the stream or parser gave it.
	final := *lq
	final.stream = slices.Clone(lq.stream)
	final.extracted = slices.Clone(lq.extracted)
	final.formatted = map[string]bool{}
	for k := range lq.formatted {
		final.formatted[k] = true
	}
	for _, l := range append(slices.Clone(pr.Grouping), pr.Without...) {
		final.use(l, nil, false)
	}
	r := Ref{
		Matchers:  lq.matchers,
		Grouping:  pr.Grouping,
		Without:   pr.Without,
		Subquery:  pr.Subquery,
		AllLabels: pr.AllLabels || lq.allLabels,
		// Stream and extracted labels survive the pipeline unless keep
		// narrows them; the metric half decides the rest.
		ResultLabels: pr.ResultLabels && !lq.keeps,
		Filters:      sortedSet(lq.filters),
		Extracted:    sortedSet(final.extracted),
		Reasons:      sortedSet(append(slices.Clone(pr.Reasons), lq.reasons...)),
	}
	var created []string
	created = append(created, pr.Created...)
	for k := range lq.formatted {
		created = append(created, k)
	}
	for k := range lq.parsed {
		created = append(created, k)
	}
	r.Created = sortedSet(created)
	stream := slices.Clone(final.stream)
	for _, m := range lq.matchers {
		stream = append(stream, m.Label)
	}
	r.StreamLabels = sortedSet(stream)
	r.Assumed = len(r.Reasons) > 0
	return r
}

// rewriteLogQueries replaces each log query (a stream selector, its
// pipeline, range and offset) with a placeholder metric, so the rest
// parses as PromQL. LogQL-only functions get PromQL stand-ins and a
// trailing by/without on a range aggregation becomes an enclosing
// aggregation; neither changes which labels are read.
func rewriteLogQueries(text string, vars []grafanaVar) (string, []*logQuery, error) {
	var b strings.Builder
	var logs []*logQuery
	ctx := scanContexts(text)
	last := 0
	for i := 0; i < len(text); i++ {
		if ctx[i] == ctxCode && text[i] == '#' { // comment to end of line
			if nl := strings.IndexByte(text[i:], '\n'); nl >= 0 {
				i += nl
			} else {
				i = len(text)
			}
			continue
		}
		if ctx[i] != ctxCode || text[i] != '{' {
			continue
		}
		lq, end, err := parseLogQuery(text, i, vars)
		if err != nil {
			return "", nil, err
		}
		b.WriteString(text[last:i])
		b.WriteString(logPlaceholderPrefix + strconv.Itoa(len(logs)) + "__")
		if lq.hasRange {
			b.WriteString("[1m]")
		}
		logs = append(logs, lq)
		last = end
		i = end - 1
	}
	b.WriteString(text[last:])
	out := b.String()
	out = logFuncRe.ReplaceAllStringFunc(out, func(m string) string {
		name := strings.TrimRight(strings.TrimSuffix(m, "("), " \t\n")
		if stand, ok := logOnlyFuncs[name]; ok {
			return stand + "("
		}
		return m
	})
	out = trailingGroupRe.ReplaceAllString(out, "max $3 ($4) ($1($2))")
	return out, logs, nil
}

var logFuncRe = regexp.MustCompile(`\b[a-z_]+\s*\(`)

// LogQL functions PromQL lacks, mapped to a PromQL function of the same
// shape.
var logOnlyFuncs = map[string]string{
	"bytes_rate":      "rate",
	"bytes_over_time": "count_over_time",
	"rate_counter":    "rate",
	"first_over_time": "last_over_time",
	"approx_topk":     "topk",
}

// A range aggregation over a log query followed by by/without, e.g.
// sum_over_time(__tvtlog0__[1m]) by (a), which PromQL doesn't allow on a
// function call.
var trailingGroupRe = regexp.MustCompile(`\b([a-z_]+)\(([^()]*` + logPlaceholderPrefix + `\d+__[^()]*)\)\s*(by|without)\s*\(([^()]*)\)`)

// Pipeline stage keywords.
var logStages = map[string]bool{
	"json": true, "logfmt": true, "regexp": true, "pattern": true, "unpack": true,
	"line_format": true, "label_format": true, "drop": true, "keep": true,
	"decolorize": true, "unwrap": true, "distinct": true,
}

// parseLogQuery parses the stream selector at text[start] == '{' and the
// pipeline, range and offset after it, and returns the offset just past
// the log query.
func parseLogQuery(text string, start int, vars []grafanaVar) (*logQuery, int, error) {
	ms, i, err := parseSelector(text, start)
	if err != nil {
		return nil, 0, err
	}
	lq := &logQuery{formatted: map[string]bool{}, parsed: map[string]bool{}}
	for _, m := range ms {
		label, found := restoreVars(m.Label, vars)
		if len(found) > 0 {
			lq.allLabels = true
			lq.reasons = append(lq.reasons, fmt.Sprintf("matcher label %s comes from variable %s", label, varTexts(found)))
			continue
		}
		value, found := restoreVars(m.Value, vars)
		lq.matchers = append(lq.matchers, Matcher{Label: m.Label, Op: m.Op, Value: value, Variable: len(found) > 0})
	}
	for {
		i = skipSpace(text, i)
		if i >= len(text) {
			return lq, i, nil
		}
		rest := text[i:]
		switch {
		case rest[0] == '[': // a range holds a duration: [5m], [1h30m]
			j := skipSpace(text, i+1)
			k := j
			for k < len(text) && (isIdentByte(text[k]) || text[k] == '.') {
				k++
			}
			e := skipSpace(text, k)
			if k == j || e >= len(text) || text[e] != ']' {
				return nil, 0, fmt.Errorf("logql: bad range at offset %d", i)
			}
			lq.hasRange = true
			i = e + 1
		case wordAt(text, i, "offset"):
			i = skipSpace(text, i+len("offset"))
			j := i
			for j < len(text) && (isIdentByte(text[j]) || text[j] == '-' || text[j] == '.') {
				j++
			}
			if j == i {
				return nil, 0, fmt.Errorf("logql: offset without a duration at offset %d", i)
			}
			i = j
		case hasAnyPrefix(rest, "|=", "|~", "|>", "!=", "!~", "!>"):
			i, err = lineFilterArgs(text, i+2)
			if err != nil {
				return nil, 0, err
			}
		case rest[0] == '|':
			i, err = lq.stage(text, i+1, vars)
			if err != nil {
				return nil, 0, err
			}
		default:
			return lq, i, nil
		}
	}
}

func hasAnyPrefix(s string, ps ...string) bool {
	for _, p := range ps {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func wordAt(s string, i int, w string) bool {
	if !strings.HasPrefix(s[i:], w) || (i > 0 && isIdentByte(s[i-1])) {
		return false
	}
	end := i + len(w)
	return end == len(s) || !isIdentByte(s[end])
}

// lineFilterArgs reads a line filter's value ("text", `re` or ip("cidr")),
// and any further `or "text"` values.
func lineFilterArgs(text string, i int) (int, error) {
	for {
		i = skipSpace(text, i)
		switch {
		case i < len(text) && (text[i] == '"' || text[i] == '`'):
			end, err := skipString(text, i)
			if err != nil {
				return 0, err
			}
			i = end
		case wordAt(text, i, "ip"):
			end, err := ipArg(text, i)
			if err != nil {
				return 0, err
			}
			i = end
		default:
			return 0, fmt.Errorf("logql: line filter without a value at offset %d", i)
		}
		j := skipSpace(text, i)
		if !wordAt(text, j, "or") {
			return i, nil
		}
		i = j + len("or")
	}
}

// ipArg reads ip("range") at text[i] and returns the offset past it.
func ipArg(text string, i int) (int, error) {
	j := skipSpace(text, i+len("ip"))
	if j >= len(text) || text[j] != '(' {
		return 0, fmt.Errorf("logql: expected ip(\"...\") at offset %d", i)
	}
	_, end, err := stringAt(text, skipSpace(text, j+1))
	if err != nil {
		return 0, fmt.Errorf("logql: ip(...) at offset %d: %w", i, err)
	}
	end = skipSpace(text, end)
	if end >= len(text) || text[end] != ')' {
		return 0, fmt.Errorf("logql: unclosed ip( at offset %d", i)
	}
	return end + 1, nil
}

// stage parses one pipeline stage after its '|'.
func (lq *logQuery) stage(text string, i int, vars []grafanaVar) (int, error) {
	i = skipSpace(text, i)
	j := i
	for j < len(text) && isIdent(text[j], j == i) {
		j++
	}
	word := text[i:j]
	if !logStages[word] || (j < len(text) && labelOpAt(text, skipSpace(text, j)) != "") {
		return lq.labelFilter(text, i, vars)
	}
	i = skipSpace(text, j)
	switch word {
	case "json", "logfmt":
		lq.hasParser = true
		n, end, err := lq.parserParams(text, i, vars)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			lq.anyParser = true
		}
		return end, nil
	case "unpack":
		lq.hasParser, lq.anyParser = true, true
		return i, nil
	case "regexp", "pattern":
		lq.hasParser = true
		s, end, err := stringAt(text, i)
		if err != nil {
			return 0, fmt.Errorf("logql: %s: %w", word, err)
		}
		names := patternNames(s)
		if word == "regexp" {
			names = regexpNames(s)
		}
		for _, n := range names {
			lq.declare(n, vars)
		}
		return end, nil
	case "line_format":
		s, end, err := stringAt(text, i)
		if err != nil {
			return 0, fmt.Errorf("logql: line_format: %w", err)
		}
		lq.useTemplate(s, vars)
		return end, nil
	case "label_format":
		return lq.labelFormat(text, i, vars)
	case "drop", "keep", "distinct":
		if word == "keep" {
			lq.keeps = true
		}
		return lq.labelList(text, i, vars)
	case "unwrap":
		k := i
		for k < len(text) && isIdent(text[k], k == i) {
			k++
		}
		if k == i {
			return 0, fmt.Errorf("logql: unwrap without a label at offset %d", i)
		}
		label := text[i:k]
		k = skipSpace(text, k)
		if k < len(text) && text[k] == '(' { // bytes(x), duration(x), duration_seconds(x)
			inner := skipSpace(text, k+1)
			m := inner
			for m < len(text) && isIdent(text[m], m == inner) {
				m++
			}
			e := skipSpace(text, m)
			if m == inner || e >= len(text) || text[e] != ')' {
				return 0, fmt.Errorf("logql: unwrap %s( at offset %d", label, k)
			}
			label = text[inner:m]
			k = e + 1
		}
		lq.use(label, vars, true)
		return k, nil
	}
	return i, nil // decolorize
}

// parserParams reads json/logfmt flags and parameters (name="expr" or
// name, comma separated) and returns how many names they declare.
func (lq *logQuery) parserParams(text string, i int, vars []grafanaVar) (int, int, error) {
	n := 0
	for {
		i = skipSpace(text, i)
		if strings.HasPrefix(text[i:], "--") {
			j := i + 2
			for j < len(text) && (isIdentByte(text[j]) || text[j] == '-') {
				j++
			}
			i = j
			continue
		}
		j := i
		for j < len(text) && isIdent(text[j], j == i) {
			j++
		}
		if j == i {
			return n, i, nil
		}
		name := text[i:j]
		k := skipSpace(text, j)
		if k < len(text) && text[k] == '=' && labelOpAt(text, k) == "=" {
			_, end, err := stringAt(text, skipSpace(text, k+1))
			if err != nil {
				return 0, 0, fmt.Errorf("logql: parser parameter %s: %w", name, err)
			}
			k = skipSpace(text, end)
		} else if k < len(text) && text[k] != ',' && text[k] != '|' && text[k] != ')' && text[k] != '[' {
			// Not a parameter list (e.g. "| json | ..." handled above);
			// anything else is a syntax error.
			if labelOpAt(text, k) != "" {
				return 0, 0, fmt.Errorf("logql: unexpected label filter after parser at offset %d", i)
			}
		}
		lq.declare(name, vars)
		n++
		if k < len(text) && text[k] == ',' {
			i = k + 1
			continue
		}
		return n, k, nil
	}
}

// labelFormat reads dst="template" and dst=src items.
func (lq *logQuery) labelFormat(text string, i int, vars []grafanaVar) (int, error) {
	var dsts []string
	for {
		i = skipSpace(text, i)
		j := i
		for j < len(text) && isIdent(text[j], j == i) {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("logql: label_format: expected a label at offset %d", i)
		}
		dst := text[i:j]
		k := skipSpace(text, j)
		if k >= len(text) || text[k] != '=' {
			return 0, fmt.Errorf("logql: label_format %s: expected '='", dst)
		}
		k = skipSpace(text, k+1)
		if k < len(text) && (text[k] == '"' || text[k] == '`') {
			s, end, err := stringAt(text, k)
			if err != nil {
				return 0, err
			}
			lq.useTemplate(s, vars)
			k = end
		} else {
			m := k
			for m < len(text) && isIdent(text[m], m == k) {
				m++
			}
			if m == k {
				return 0, fmt.Errorf("logql: label_format %s: expected a label or template", dst)
			}
			lq.use(text[k:m], vars, true)
			k = m
		}
		dsts = append(dsts, dst)
		k = skipSpace(text, k)
		if k < len(text) && text[k] == ',' {
			i = k + 1
			continue
		}
		// Renames take effect after the stage, so later stages see them.
		for _, d := range dsts {
			if restored, found := restoreVars(d, vars); len(found) > 0 {
				lq.allLabels = true
				lq.reasons = append(lq.reasons, fmt.Sprintf("label_format label %s comes from variable %s", restored, varTexts(found)))
				continue
			}
			lq.formatted[d] = true
		}
		return k, nil
	}
}

// labelList reads keep/drop/distinct items: label or label op "value".
func (lq *logQuery) labelList(text string, i int, vars []grafanaVar) (int, error) {
	for {
		i = skipSpace(text, i)
		j := i
		for j < len(text) && isIdent(text[j], j == i) {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("logql: expected a label at offset %d", i)
		}
		lq.use(text[i:j], vars, true)
		k := skipSpace(text, j)
		if op := labelOpAt(text, k); op != "" {
			_, end, err := stringAt(text, skipSpace(text, k+len(op)))
			if err != nil {
				return 0, err
			}
			k = skipSpace(text, end)
		}
		if k < len(text) && text[k] == ',' {
			i = k + 1
			continue
		}
		return k, nil
	}
}

// labelFilter reads a label filter expression: label op value, combined
// with and, or, ',' and parentheses. It ends at the next '|', a range, or
// a ')' it didn't open.
func (lq *logQuery) labelFilter(text string, i int, vars []grafanaVar) (int, error) {
	depth := 0
	sawLabel := false
	for {
		i = skipSpace(text, i)
		if i >= len(text) {
			break
		}
		c := text[i]
		if c == '|' || c == '[' || (c == ')' && depth == 0) || wordAt(text, i, "offset") {
			break
		}
		switch {
		case c == '(':
			depth++
			i++
			continue
		case c == ')':
			depth--
			i++
			continue
		case c == ',':
			i++
			continue
		case wordAt(text, i, "and") || wordAt(text, i, "or"):
			i = skipIdent(text, i)
			continue
		}
		j := skipIdent(text, i)
		if j == i {
			return 0, fmt.Errorf("logql: label filter: unexpected %q at offset %d", c, i)
		}
		label := text[i:j]
		k := skipSpace(text, j)
		op := labelOpAt(text, k)
		if op == "" {
			return 0, fmt.Errorf("logql: label filter: expected an operator after %q", label)
		}
		lq.use(label, vars, true)
		sawLabel = true
		k = skipSpace(text, k+len(op))
		switch {
		case k < len(text) && (text[k] == '"' || text[k] == '`'):
			end, err := skipString(text, k)
			if err != nil {
				return 0, err
			}
			i = end
		case wordAt(text, k, "ip"):
			end, err := ipArg(text, k)
			if err != nil {
				return 0, fmt.Errorf("logql: label filter %s: %w", label, err)
			}
			i = end
		default: // number, duration or bytes: 5, -1.5, 250ms, 20MB
			m := k
			for m < len(text) && (isIdentByte(text[m]) || text[m] == '.' || text[m] == '-' || text[m] == '+') {
				m++
			}
			if m == k {
				return 0, fmt.Errorf("logql: label filter %s: expected a value", label)
			}
			i = m
		}
	}
	if !sawLabel || depth != 0 {
		return 0, fmt.Errorf("logql: bad label filter at offset %d", i)
	}
	return i, nil
}

func skipIdent(s string, i int) int {
	j := i
	for j < len(s) && isIdent(s[j], j == i) {
		j++
	}
	return j
}

// labelOpAt returns the label filter operator at s[i], or "".
func labelOpAt(s string, i int) string {
	for _, op := range []string{"==", "=~", "!=", "!~", ">=", "<=", "=", ">", "<"} {
		if strings.HasPrefix(s[min(i, len(s)):], op) {
			return op
		}
	}
	return ""
}

// stringAt reads the string literal at s[i] and returns its value.
func stringAt(s string, i int) (string, int, error) {
	if i >= len(s) || (s[i] != '"' && s[i] != '`') {
		return "", 0, fmt.Errorf("expected a string at offset %d", i)
	}
	end, err := skipString(s, i)
	if err != nil {
		return "", 0, err
	}
	v, err := strconv.Unquote(s[i:end])
	if err != nil {
		return "", 0, fmt.Errorf("string at offset %d: %w", i, err)
	}
	return v, end, nil
}

var (
	templateActionRe = regexp.MustCompile(`(?s)\{\{(.*?)\}\}`)
	templateFieldRe  = regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_]*)`)
	templateIndexRe  = regexp.MustCompile(`\bindex\s+\.\s*("(?:[^"\\]|\\.)*"|` + "`[^`]*`" + `)`)
	templateStringRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|` + "`[^`]*`")
	patternCaptureRe = regexp.MustCompile(`<([A-Za-z_][A-Za-z0-9_]*)>`)
	namedGroupRe     = regexp.MustCompile(`\(\?P?<([A-Za-z_][A-Za-z0-9_]*)>`)
)

// templateFields returns the labels a line_format or label_format template
// reads: fields ({{.name}}) and static indexes ({{index . "name"}}). whole
// is true when an action reads the whole label set ({{.}}, {{index . $k}},
// {{range .}}), so any label can change the output.
func templateFields(tmpl string) (fields []string, whole bool) {
	for _, a := range templateActionRe.FindAllStringSubmatch(tmpl, -1) {
		act := templateIndexRe.ReplaceAllStringFunc(a[1], func(m string) string {
			sub := templateIndexRe.FindStringSubmatch(m)
			if v, err := strconv.Unquote(sub[1]); err == nil {
				fields = append(fields, v)
			}
			return " "
		})
		act = templateStringRe.ReplaceAllString(act, `""`)
		for _, f := range templateFieldRe.FindAllStringSubmatch(act, -1) {
			fields = append(fields, f[1])
		}
		for i := 0; i < len(act); i++ {
			if act[i] != '.' || (i > 0 && act[i-1] >= '0' && act[i-1] <= '9') {
				continue
			}
			if i+1 < len(act) && isIdent(act[i+1], true) {
				continue
			}
			whole = true
		}
	}
	return fields, whole
}

func patternNames(p string) []string {
	var out []string
	for _, m := range patternCaptureRe.FindAllStringSubmatch(p, -1) {
		if m[1] != "_" {
			out = append(out, m[1])
		}
	}
	return out
}

func regexpNames(re string) []string {
	var out []string
	if r, err := regexp.Compile(re); err == nil {
		for _, n := range r.SubexpNames() {
			if n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	for _, m := range namedGroupRe.FindAllStringSubmatch(re, -1) {
		out = append(out, m[1])
	}
	return out
}

// logFallback is the assumed result for a query that didn't parse: every
// stream selector the text holds, reading every label, or every stream.
func logFallback(query string, perr error) []Ref {
	reason := "query did not parse: " + firstLine(perr.Error())
	text, vars := placeholderVars(query)
	var refs []Ref
	for _, ms := range scanStreamSelectors(text) {
		r := Ref{AllLabels: true, Assumed: true, Reasons: []string{reason}}
		for _, m := range ms {
			label, found := restoreVars(m.Label, vars)
			if len(found) > 0 {
				r.Reasons = append(r.Reasons, fmt.Sprintf("matcher label %s comes from variable %s", label, varTexts(found)))
				continue
			}
			value, found := restoreVars(m.Value, vars)
			r.Matchers = append(r.Matchers, Matcher{Label: m.Label, Op: m.Op, Value: value, Variable: len(found) > 0})
			r.StreamLabels = append(r.StreamLabels, m.Label)
		}
		r.StreamLabels = sortedSet(r.StreamLabels)
		slices.Sort(r.Reasons)
		refs = append(refs, r)
	}
	if len(refs) == 0 || !utf8.ValidString(query) {
		refs = []Ref{{AllLabels: true, Assumed: true, Reasons: []string{reason + " (no stream selector found, so every stream)"}}}
	}
	return refs
}
