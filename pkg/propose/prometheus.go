package propose

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Prometheus adds d as a metric_relabel_configs drop rule to the scrape
// configs named in jobs, in a Prometheus config file's bytes (S7 item 3).
// Existing tvt blocks are removed first and every other byte is kept: the
// new block is spliced in at a position taken from the YAML tree, indented
// like its neighbours. The result must load as a Prometheus config.
// A job missing from the file is an error.
func Prometheus(src []byte, jobs []string, d Drop) ([]byte, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("propose: drop %s: no scrape job to put it in", d.ID)
	}
	byJob := make(map[string]Drop, len(jobs))
	for _, j := range jobs {
		byJob[j] = d
	}
	return PrometheusDrops(src, byJob)
}

// PrometheusDrops is Prometheus with a drop per job (tvt propose: each
// scrape config gets a block naming only the metrics it carries). Every
// existing tvt block is removed first, so byJob is the whole proposal.
func PrometheusDrops(src []byte, byJob map[string]Drop) ([]byte, error) {
	if len(byJob) == 0 {
		return nil, fmt.Errorf("propose: no scrape job to put a drop in")
	}
	for _, d := range byJob {
		if err := d.validate(); err != nil {
			return nil, err
		}
	}
	out, _, err := stripBlocks(src, "#")
	if err != nil {
		return nil, err
	}
	jobs := make([]string, 0, len(byJob))
	for j := range byJob {
		jobs = append(jobs, j)
	}
	slices.Sort(jobs)
	// Splice from the bottom up so earlier line numbers stay valid.
	var edits []promEdit
	for _, job := range jobs {
		e, err := prometheusEdit(out, job, byJob[job])
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	slices.SortStableFunc(edits, func(a, b promEdit) int { return b.after - a.after })
	for _, e := range edits {
		out = insertAfterLine(out, e.after, e.text)
	}
	// Re-read what was written: each job must now carry exactly its
	// drop. Full validation (promtool check config) is S7 item 5's; the
	// Prometheus config package would pull every service discovery into
	// the binary (ADR 0004).
	for _, job := range jobs {
		if err := checkPromDrop(out, job, byJob[job]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PrometheusJobs lists the job_names of a Prometheus config's scrape
// configs, in file order.
func PrometheusJobs(src []byte) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("propose: not a Prometheus config (want a mapping)")
	}
	var out []string
	if scrapes := value(doc.Content[0], "scrape_configs"); scrapes != nil {
		for _, sc := range scrapes.Content {
			if sc.Kind == yaml.MappingNode {
				if n := value(sc, "job_name"); n != nil {
					out = append(out, n.Value)
				}
			}
		}
	}
	return out, nil
}

type promEdit struct {
	after int
	text  string
}

func prometheusEdit(src []byte, job string, d Drop) (promEdit, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return promEdit{}, fmt.Errorf("propose: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return promEdit{}, fmt.Errorf("propose: not a Prometheus config (want a mapping)")
	}
	scrapes := value(doc.Content[0], "scrape_configs")
	if scrapes == nil || scrapes.Kind != yaml.SequenceNode {
		return promEdit{}, fmt.Errorf("propose: no scrape_configs list")
	}
	for _, sc := range scrapes.Content {
		if sc.Kind != yaml.MappingNode {
			continue
		}
		if name := value(sc, "job_name"); name == nil || name.Value != job {
			continue
		}
		keyIndent := strings.Repeat(" ", sc.Content[0].Column-1)
		if mrc := value(sc, "metric_relabel_configs"); mrc != nil {
			if mrc.Kind != yaml.SequenceNode || mrc.Style&yaml.FlowStyle != 0 || len(mrc.Content) == 0 {
				return promEdit{}, fmt.Errorf("propose: job %s: metric_relabel_configs isn't a block list tvt can append to", job)
			}
			itemIndent := strings.Repeat(" ", mrc.Content[0].Column-3)
			return promEdit{after: lastLine(mrc), text: promBlock(itemIndent, d, false)}, nil
		}
		return promEdit{after: lastLine(sc), text: promBlock(keyIndent, d, true)}, nil
	}
	return promEdit{}, fmt.Errorf("propose: no scrape config with job_name %q", job)
}

// promBlock is the marked block: a new metric_relabel_configs key with
// one item (withKey), or one item to append to an existing list. indent is
// the key's indentation, or the list item's dash.
func promBlock(indent string, d Drop, withKey bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s# %s\n", indent, d.begin())
	if d.Reason != "" {
		fmt.Fprintf(&b, "%s# %s\n", indent, d.Reason)
	}
	item := indent
	if withKey {
		fmt.Fprintf(&b, "%smetric_relabel_configs:\n", indent)
		item = indent + "  "
	}
	if len(d.Metrics) > 0 {
		fmt.Fprintf(&b, "%s- source_labels: [__name__]\n", item)
		fmt.Fprintf(&b, "%s  regex: %s\n", item, strconv.Quote(d.Regex()))
		fmt.Fprintf(&b, "%s  action: drop\n", item)
	}
	for _, l := range d.Labels {
		// Emptying a label removes it, on these metrics only.
		fmt.Fprintf(&b, "%s- source_labels: [__name__]\n", item)
		fmt.Fprintf(&b, "%s  regex: %s\n", item, strconv.Quote(l.Regex()))
		fmt.Fprintf(&b, "%s  target_label: %s\n", item, l.Label)
		fmt.Fprintf(&b, "%s  replacement: \"\"\n", item)
		fmt.Fprintf(&b, "%s  action: replace\n", item)
	}
	fmt.Fprintf(&b, "%s# %s\n", indent, endMarker)
	return b.String()
}

func value(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// lastLine is the last line a node's content reaches.
func lastLine(n *yaml.Node) int {
	l := n.Line
	if n.Kind == yaml.ScalarNode && (n.Style&(yaml.LiteralStyle|yaml.FoldedStyle)) != 0 {
		l += strings.Count(strings.TrimRight(n.Value, "\n"), "\n") + 1
	}
	for _, c := range n.Content {
		if cl := lastLine(c); cl > l {
			l = cl
		}
	}
	return l
}

// checkPromDrop re-reads src and confirms job's metric_relabel_configs
// holds d's rule.
func checkPromDrop(src []byte, job string, d Drop) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	for _, sc := range value(doc.Content[0], "scrape_configs").Content {
		if n := value(sc, "job_name"); n == nil || n.Value != job {
			continue
		}
		mrc := value(sc, "metric_relabel_configs")
		if mrc == nil {
			break
		}
		has := func(action, regex, target string) bool {
			for _, r := range mrc.Content {
				if str(r, "action") == action && str(r, "regex") == regex && str(r, "target_label") == target {
					return true
				}
			}
			return false
		}
		if len(d.Metrics) > 0 && !has("drop", d.Regex(), "") {
			break
		}
		for _, l := range d.Labels {
			if !has("replace", l.Regex(), l.Label) {
				return fmt.Errorf("propose: job %s: the edited config lost the rule removing %s", job, l.Label)
			}
		}
		return nil
	}
	return fmt.Errorf("propose: job %s: the edited config lost the drop rule", job)
}
