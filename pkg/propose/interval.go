package propose

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/grafana/alloy/syntax/ast"
	"github.com/grafana/alloy/syntax/parser"
	"github.com/prometheus/common/model"
	"go.yaml.in/yaml/v3"
)

// IntervalPrefix starts every scrape interval block's ID, so the interval
// writers strip only their own blocks.
const IntervalPrefix = "interval:"

// Interval is a longer scrape interval for one job (S10 item 5): the
// report's intervals row, guarded there (every reader's range keeps 4
// samples, at most 2m for a reader without one, alerts inside their
// detection objectives).
type Interval struct {
	ID       string // interval:<job>
	Scan     int64
	Evidence string
	Reason   string
	Every    time.Duration
}

func (iv Interval) begin() string {
	s := fmt.Sprintf("tvt:begin %s scan=%d", iv.ID, iv.Scan)
	if iv.Evidence != "" {
		s += " evidence=" + iv.Evidence
	}
	return s
}

func (iv Interval) validate() error {
	if !strings.HasPrefix(iv.ID, IntervalPrefix) || strings.ContainsAny(iv.ID, " \n\r") {
		return fmt.Errorf("propose: interval ID %q: want %s<job>", iv.ID, IntervalPrefix)
	}
	if strings.ContainsAny(iv.Reason, "\n\r") {
		return fmt.Errorf("propose: interval %s: reason must be one line", iv.ID)
	}
	if iv.Every < time.Second || iv.Every%time.Second != 0 {
		return fmt.Errorf("propose: interval %s: %s isn't a whole number of seconds", iv.ID, iv.Every)
	}
	return nil
}

func (iv Interval) every() string { return model.Duration(iv.Every).String() }

func isIntervalBlock(begin string) bool { return strings.HasPrefix(begin, IntervalPrefix) }

// PrometheusIntervals sets scrape_interval on each named scrape config in
// a Prometheus config's bytes. The job's own scrape_interval line, if it
// has one, moves into the block as a tvt:original comment, so stripping
// the block restores it; otherwise the block adds the key after the job's
// last line. Existing interval blocks are stripped first and other tvt
// blocks are kept: run it after PrometheusDrops, which strips every block.
func PrometheusIntervals(src []byte, byJob map[string]Interval) ([]byte, error) {
	for _, iv := range byJob {
		if err := iv.validate(); err != nil {
			return nil, err
		}
	}
	out, _, err := stripBlocksIf(src, "#", isIntervalBlock)
	if err != nil {
		return nil, err
	}
	if len(byJob) == 0 {
		return out, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("propose: not a Prometheus config (want a mapping)")
	}
	scrapes := value(doc.Content[0], "scrape_configs")
	if scrapes == nil || scrapes.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("propose: no scrape_configs list")
	}
	type edit struct {
		line    int  // replace this line, or insert after it
		replace bool // line holds the job's scrape_interval
		text    string
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	lines := strings.SplitAfter(string(out), "\n")
	var edits []edit
	for job, iv := range byJob {
		var sc *yaml.Node
		for _, c := range scrapes.Content {
			if c.Kind == yaml.MappingNode {
				if n := value(c, "job_name"); n != nil && n.Value == job {
					sc = c
					break
				}
			}
		}
		if sc == nil {
			return nil, fmt.Errorf("propose: no scrape config with job_name %q", job)
		}
		if sc.Style&yaml.FlowStyle != 0 {
			return nil, fmt.Errorf("propose: job %s is a flow mapping tvt can't edit", job)
		}
		indent := strings.Repeat(" ", sc.Content[0].Column-1)
		var b strings.Builder
		fmt.Fprintf(&b, "%s# %s\n", indent, iv.begin())
		if iv.Reason != "" {
			fmt.Fprintf(&b, "%s# %s\n", indent, iv.Reason)
		}
		for i := 0; i+1 < len(sc.Content); i += 2 {
			k, v := sc.Content[i], sc.Content[i+1]
			if k.Value != "scrape_interval" {
				continue
			}
			if v.Line != k.Line {
				return nil, fmt.Errorf("propose: job %s: scrape_interval isn't on one line", job)
			}
			fmt.Fprintf(&b, "%s# %s%s\n", indent, originalMarker, strings.TrimSpace(lines[k.Line-1]))
			fmt.Fprintf(&b, "%sscrape_interval: %s\n%s# %s\n", indent, iv.every(), indent, endMarker)
			edits = append(edits, edit{line: k.Line, replace: true, text: b.String()})
			b.Reset()
			break
		}
		if b.Len() > 0 {
			fmt.Fprintf(&b, "%sscrape_interval: %s\n%s# %s\n", indent, iv.every(), indent, endMarker)
			// After the job's last node, and after any comments indented
			// under it: a drop block's closing tvt:end follows the last
			// node, and the new key must land outside it.
			after := lastLine(sc)
			for after < len(lines) {
				next := lines[after]
				t := strings.TrimSpace(next)
				if !strings.HasPrefix(t, "#") || len(next)-len(strings.TrimLeft(next, " ")) < len(indent) {
					break
				}
				after++
			}
			edits = append(edits, edit{line: after, text: b.String()})
		}
	}
	// Bottom up, so earlier line numbers stay valid.
	slices.SortStableFunc(edits, func(a, b edit) int { return b.line - a.line })
	for _, e := range edits {
		if e.replace {
			lines[e.line-1] = e.text
		} else {
			lines[e.line-1] += e.text
		}
	}
	out = []byte(strings.Join(lines, ""))
	for job, iv := range byJob {
		if err := checkPromInterval(out, job, iv); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func checkPromInterval(src []byte, job string, iv Interval) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	for _, sc := range value(doc.Content[0], "scrape_configs").Content {
		if n := value(sc, "job_name"); n == nil || n.Value != job {
			continue
		}
		n := 0
		for i := 0; i+1 < len(sc.Content); i += 2 {
			if sc.Content[i].Value == "scrape_interval" {
				n++
				if sc.Content[i+1].Value != iv.every() {
					return fmt.Errorf("propose: job %s: scrape_interval is %s, want %s", job, sc.Content[i+1].Value, iv.every())
				}
			}
		}
		if n != 1 {
			return fmt.Errorf("propose: job %s: %d scrape_interval keys after the edit", job, n)
		}
		return nil
	}
	return fmt.Errorf("propose: job %s is gone after the edit", job)
}

// AlloyIntervals sets scrape_interval on each job's prometheus.scrape
// component (the job is job_name, else the component's full name,
// prometheus.scrape.<label>, as Alloy defaults it and check reads it). Like PrometheusIntervals, the component's own attribute
// line moves into the block as a tvt:original comment; without one the
// block goes at the end of the component's body.
func AlloyIntervals(src []byte, byJob map[string]Interval) ([]byte, error) {
	for _, iv := range byJob {
		if err := iv.validate(); err != nil {
			return nil, err
		}
	}
	out, _, err := stripBlocksIf(src, "//", isIntervalBlock)
	if err != nil {
		return nil, err
	}
	if len(byJob) == 0 {
		return out, nil
	}
	f, err := parser.ParseFile("config.alloy", out)
	if err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	scrapes := map[string]*ast.BlockStmt{}
	for _, st := range f.Body {
		if b, ok := st.(*ast.BlockStmt); ok && strings.Join(b.Name, ".") == "prometheus.scrape" {
			job := "prometheus.scrape." + b.Label
			if v, ok := literalString(attr(b.Body, "job_name")); ok {
				job = v
			}
			scrapes[job] = b
		}
	}
	type edit struct {
		line    int
		replace bool
		text    string
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	lines := strings.SplitAfter(string(out), "\n")
	var edits []edit
	for job, iv := range byJob {
		sc, ok := scrapes[job]
		if !ok {
			return nil, fmt.Errorf("propose: no prometheus.scrape component for job %q", job)
		}
		indent := lineIndent(out, sc.NamePos.Position().Line) + "\t"
		if len(sc.Body) > 0 {
			indent = lineIndent(out, ast.StartPos(sc.Body[0]).Position().Line)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s// %s\n", indent, iv.begin())
		if iv.Reason != "" {
			fmt.Fprintf(&b, "%s// %s\n", indent, iv.Reason)
		}
		done := false
		for _, st := range sc.Body {
			a, ok := st.(*ast.AttributeStmt)
			if !ok || a.Name.Name != "scrape_interval" {
				continue
			}
			start, end := ast.StartPos(a).Position().Line, ast.EndPos(a).Position().Line
			if start != end {
				return nil, fmt.Errorf("propose: prometheus.scrape %q: scrape_interval isn't on one line", sc.Label)
			}
			fmt.Fprintf(&b, "%s// %s%s\n", indent, originalMarker, strings.TrimSpace(lines[start-1]))
			fmt.Fprintf(&b, "%sscrape_interval = %q\n%s// %s\n", indent, iv.every(), indent, endMarker)
			edits = append(edits, edit{line: start, replace: true, text: b.String()})
			done = true
			break
		}
		if !done {
			fmt.Fprintf(&b, "%sscrape_interval = %q\n%s// %s\n", indent, iv.every(), indent, endMarker)
			edits = append(edits, edit{line: sc.RCurlyPos.Position().Line - 1, text: b.String()})
		}
	}
	slices.SortStableFunc(edits, func(a, b edit) int { return b.line - a.line })
	for _, e := range edits {
		if e.replace {
			lines[e.line-1] = e.text
		} else {
			lines[e.line-1] += e.text
		}
	}
	out = []byte(strings.Join(lines, ""))
	f, err = parser.ParseFile("config.alloy", out)
	if err != nil {
		return nil, fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	for _, st := range f.Body {
		b, ok := st.(*ast.BlockStmt)
		if !ok || strings.Join(b.Name, ".") != "prometheus.scrape" {
			continue
		}
		job := "prometheus.scrape." + b.Label
		if v, ok := literalString(attr(b.Body, "job_name")); ok {
			job = v
		}
		iv, ok := byJob[job]
		if !ok {
			continue
		}
		if v, ok := literalString(attr(b.Body, "scrape_interval")); !ok || v != iv.every() {
			return nil, fmt.Errorf("propose: prometheus.scrape %q: scrape_interval is %q after the edit, want %q", b.Label, v, iv.every())
		}
	}
	return out, nil
}
