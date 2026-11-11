package parse

import (
	"strings"
)

// PromVariable returns the Refs of a Grafana Prometheus variable query.
// Besides plain PromQL, Grafana's Prometheus data source has its own
// functions:
//
//	label_values(selector, label)  the label is read on the selector's series
//	label_values(label)            reads a label on every series: no metric
//	label_names() / label_names(selector)
//	metrics(regex)                 metric names matching regex (unanchored, as Grafana applies it)
//	query_result(expr)             plain PromQL
//
// label_values(label) and label_names() depend on no particular metric, so
// they return no Refs. label_names(selector) reads every label name of the
// selector's series, so its Refs are AllLabels. Errors and fallbacks are
// PromQL's.
func PromVariable(query string) ([]Ref, error) {
	q := strings.TrimSpace(query)
	if args, ok := varFunc(q, "label_values"); ok {
		switch len(args) {
		case 1:
			return nil, nil
		case 2:
			refs, err := PromQL(args[0])
			readLabel(refs, strings.TrimSpace(args[1]), false)
			return refs, err
		}
	}
	if args, ok := varFunc(q, "label_names"); ok {
		if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
			return nil, nil
		}
		refs, err := PromQL(args[0])
		for i := range refs {
			refs[i].AllLabels = true
			refs[i].ResultLabels = false // the variable holds names, not series
		}
		return refs, err
	}
	if args, ok := varFunc(q, "metrics"); ok && len(args) == 1 {
		// Grafana filters names with an unanchored regex; a Matcher's regex
		// is anchored, so widen it.
		re := strings.TrimSpace(args[0])
		m := Matcher{Label: "__name__", Op: MatchRegexp, Value: ".*(?:" + re + ").*", Variable: strings.ContainsAny(re, "$[")}
		return []Ref{{Matchers: []Matcher{m}}}, nil
	}
	if args, ok := varFunc(q, "query_result"); ok && len(args) == 1 {
		return PromQL(args[0])
	}
	return PromQL(q)
}

// LogVariable returns the Refs of a Grafana Loki variable query:
// label_values(selector, label) reads the stream label on the selector's
// streams, label_values(label) reads it on every stream (one Ref with no
// matchers). Anything else is read as LogQL.
func LogVariable(query string) ([]Ref, error) {
	q := strings.TrimSpace(query)
	if args, ok := varFunc(q, "label_values"); ok {
		switch len(args) {
		case 1:
			refs := []Ref{{}}
			readLabel(refs, strings.TrimSpace(args[0]), true)
			return refs, nil
		case 2:
			refs, err := LogQL(args[0])
			readLabel(refs, strings.TrimSpace(args[1]), true)
			return refs, err
		}
	}
	return LogQL(q)
}

// readLabel records that a label_values() variable reads label on each Ref:
// as grouping for PromQL, as a stream label for LogQL. A label name from a
// variable could be any label, so the Refs then depend on all of them. No
// other label reaches the variable, so ResultLabels is cleared.
func readLabel(refs []Ref, label string, stream bool) {
	for i := range refs {
		refs[i].ResultLabels = false // the variable holds one label's values
		switch {
		case label == "" || label == "__name__":
		case strings.ContainsAny(label, "$["):
			refs[i].AllLabels, refs[i].Assumed = true, true
			refs[i].Reasons = sortedSet(append(refs[i].Reasons, "label_values() label "+label+" comes from a variable"))
		case stream:
			refs[i].StreamLabels = sortedSet(append(refs[i].StreamLabels, label))
			refs[i].Filters = sortedSet(append(refs[i].Filters, label))
		default:
			refs[i].Grouping = sortedSet(append(refs[i].Grouping, label))
		}
	}
}

// varFunc reports whether q is a call name(...) spanning the whole text and
// returns its arguments, split at top-level commas (outside quotes,
// parentheses, braces and brackets). name() returns no arguments.
func varFunc(q, name string) ([]string, bool) {
	if !strings.HasPrefix(q, name) {
		return nil, false
	}
	rest := strings.TrimSpace(q[len(name):])
	if !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return nil, false
	}
	body := rest[1 : len(rest)-1]
	var args []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case quote != 0:
			if c == '\\' && quote != '`' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
			if depth < 0 {
				return nil, false // name(a)(b): the closing paren isn't ours
			}
		case c == ',' && depth == 0:
			args = append(args, body[start:i])
			start = i + 1
		}
	}
	if depth != 0 || quote != 0 {
		return nil, false
	}
	if last := body[start:]; strings.TrimSpace(last) != "" || len(args) > 0 {
		args = append(args, last)
	}
	return args, true
}
