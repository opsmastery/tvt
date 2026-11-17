package propose

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Drop is one generated drop of whole metrics (S7 item 3): what a writer
// puts into a pipeline's config, inside a marked block (S7 item 10).
type Drop struct {
	// ID names the block: the recommendation's signal ID, or a stable name
	// when one block drops several metrics.
	ID string
	// Scan is the scan the evidence comes from; Evidence a short hash of
	// it (S9 binds approvals to it); Reason the one-line why.
	Scan     int64
	Evidence string
	Reason   string
	// Metrics are exact metric names; the rule matches nothing else.
	Metrics []string
}

// Regex is the relabel regex that matches exactly d.Metrics (anchored by
// Prometheus and Alloy).
func (d Drop) Regex() string {
	names := slices.Clone(d.Metrics)
	slices.Sort(names)
	names = slices.Compact(names)
	for i, n := range names {
		names[i] = regexp.QuoteMeta(n)
	}
	return strings.Join(names, "|")
}

// begin is the block's first line, after the comment prefix.
func (d Drop) begin() string {
	s := fmt.Sprintf("tvt:begin %s scan=%d", d.ID, d.Scan)
	if d.Evidence != "" {
		s += " evidence=" + d.Evidence
	}
	return s
}

const endMarker = "tvt:end"

// validate refuses a drop no writer should emit.
func (d Drop) validate() error {
	if d.ID == "" || strings.ContainsAny(d.ID, "\n\r") {
		return fmt.Errorf("propose: drop needs a one-line ID")
	}
	if len(d.Metrics) == 0 {
		return fmt.Errorf("propose: drop %s names no metric", d.ID)
	}
	for _, m := range d.Metrics {
		if !metricName.MatchString(m) {
			return fmt.Errorf("propose: drop %s: %q is not a metric name", d.ID, m)
		}
	}
	if strings.ContainsAny(d.Reason, "\n\r") {
		return fmt.Errorf("propose: drop %s: reason must be one line", d.ID)
	}
	return nil
}

var metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// stripBlocks removes every tvt block (a line whose comment starts with
// tvt:begin, through the next line whose comment is tvt:end) so a rerun
// regenerates them from current evidence and leaves everything else
// alone. prefix is the comment leader ("#" or "//"). An unterminated
// block is an error: tvt never guesses where its own text ends.
func stripBlocks(src []byte, prefix string) ([]byte, int, error) {
	lines := strings.SplitAfter(string(src), "\n")
	var out strings.Builder
	in, blocks := false, 0
	for i, l := range lines {
		t := strings.TrimSpace(l)
		isBegin := strings.HasPrefix(t, prefix) && strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(t, prefix)), "tvt:begin")
		isEnd := strings.HasPrefix(t, prefix) && strings.TrimSpace(strings.TrimPrefix(t, prefix)) == endMarker
		switch {
		case in && isEnd:
			in = false
		case in && isBegin:
			return nil, 0, fmt.Errorf("propose: line %d: tvt:begin inside an open tvt block", i+1)
		case in:
		case isBegin:
			in = true
			blocks++
		case isEnd:
			return nil, 0, fmt.Errorf("propose: line %d: tvt:end with no tvt:begin", i+1)
		default:
			out.WriteString(l)
		}
	}
	if in {
		return nil, 0, fmt.Errorf("propose: a tvt block is never closed (no %s %s)", prefix, endMarker)
	}
	return []byte(out.String()), blocks, nil
}

// insertAfterLine inserts text after line n (1-based; 0 inserts at the
// top).
func insertAfterLine(src []byte, n int, text string) []byte {
	lines := strings.SplitAfter(string(src), "\n")
	if n > len(lines) {
		n = len(lines)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(lines[i])
	}
	if n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
		b.WriteString("\n")
	}
	b.WriteString(text)
	for i := n; i < len(lines); i++ {
		b.WriteString(lines[i])
	}
	return []byte(b.String())
}
