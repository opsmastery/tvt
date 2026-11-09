// Package parse extracts which signals a query touches. PromQL uses the
// upstream Prometheus parser; LogQL uses Loki's syntax package (or a vendored
// subset, pending the Phase 0 dependency spike).
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
}

// Extractor parses a query and returns every selector it references.
type Extractor interface {
	Extract(query string) ([]Ref, error)
}
