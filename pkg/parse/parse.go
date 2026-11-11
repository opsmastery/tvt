// Package parse extracts which signals a query touches. PromQL uses the
// upstream Prometheus parser (PromQL); LogQL gets tvt's own extractor
// (LogQL, ADR 0005).
//
// Label-level detail matters: dropping a label breaks queries that match on
// it, group by it or join on it, and silently changes queries that use
// `without`. Extractors must report all of those.
package parse

// MatchOp is a label matcher operator.
type MatchOp string

const (
	MatchEqual    MatchOp = "="
	MatchNotEqual MatchOp = "!="
	MatchRegexp   MatchOp = "=~"
	MatchNotRegex MatchOp = "!~"
)

// Matcher is one label matcher in a selector.
type Matcher struct {
	Label string
	Op    MatchOp
	Value string
	// Variable: Value holds a Grafana template variable (as written, e.g.
	// "$cluster"), so the series it selects aren't known. The metric and
	// label are still read exactly, so this alone doesn't make a Ref
	// Assumed.
	Variable bool
}

// Ref is one selector in a query and the labels the query depends on.
type Ref struct {
	// Metric is the metric name, empty for log stream selectors or when the
	// name is only given by a regex matcher on __name__.
	Metric   string
	Matchers []Matcher
	// Grouping are labels in by(...) or on(...)/group_left/group_right.
	Grouping []string
	// Without are labels in without(...) or ignoring(...). Dropping one of
	// these changes the result without an error.
	Without []string
	// Created are labels label_replace, label_join or count_values write
	// over this selector's series. Grouping can name one of them; such a
	// label need not exist on the stored series.
	Created []string
	// Subquery is set when the selector is evaluated inside a subquery.
	Subquery bool
	// AllLabels: the result may depend on every label of the series, so
	// dropping any of them can change it. Set for a vector match without
	// on(...) (default, ignoring, and/or/unless), for a label name taken
	// from a variable, and for every Ref from a query that didn't parse.
	AllLabels bool
	// LogQL only. Filters are the labels the pipeline reads (label
	// filters, unwrap, line_format/label_format, keep, drop). StreamLabels
	// are labels read that can only be stream labels; Extracted are labels
	// read that a parser stage may have produced (a stream label only if
	// the stream carries that name). See LogQL.
	Filters      []string
	StreamLabels []string
	Extracted    []string
	// Assumed: the reference is a guess, not read exactly from the query.
	// Reasons says why (a variable, a parse error).
	Assumed bool
	Reasons []string
}

// Extractor parses a query and returns every selector it references.
type Extractor interface {
	Extract(query string) ([]Ref, error)
}
