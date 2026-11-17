package propose

import (
	"os"
	"strings"
	"testing"

	"github.com/grafana/alloy/syntax/ast"
	"github.com/grafana/alloy/syntax/parser"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// onlyAdded checks that every line of before is in after, in order: a
// writer adds its block and changes nothing else.
func onlyAdded(t *testing.T, before, after []byte) []string {
	t.Helper()
	a := strings.Split(string(after), "\n")
	var added []string
	i := 0
	for _, l := range strings.Split(string(before), "\n") {
		for i < len(a) && a[i] != l {
			added = append(added, a[i])
			i++
		}
		if i == len(a) {
			t.Fatalf("line %q of the original is gone or moved", l)
		}
		i++
	}
	return append(added, a[i:]...)
}

var drop = Drop{ID: "prom:metric:loki_ingester_chunk_age_seconds_bucket", Scan: 42, Evidence: "ab12cd34",
	Reason:  "no reader in 30d; 0 rules, 0 panels; $3.10/mo",
	Metrics: []string{"loki_ingester_chunk_age_seconds_bucket", "loki_ingester_chunk_age_seconds_sum"}}

func TestPrometheus(t *testing.T) {
	src := read(t, "lab-scrape.yml")
	out, err := Prometheus(src, []string{"loki", "alertmanager"}, drop)
	if err != nil {
		t.Fatal(err)
	}
	added := onlyAdded(t, src, out)
	want := []string{
		"    # tvt:begin prom:metric:loki_ingester_chunk_age_seconds_bucket scan=42 evidence=ab12cd34",
		"    # no reader in 30d; 0 rules, 0 panels; $3.10/mo",
		"    metric_relabel_configs:",
		"      - source_labels: [__name__]",
		`        regex: "loki_ingester_chunk_age_seconds_bucket|loki_ingester_chunk_age_seconds_sum"`,
		"        action: drop",
		"    # tvt:end",
	}
	if len(added) != 2*len(want) {
		t.Fatalf("added %d lines, want %d (one block per job):\n%s", len(added), 2*len(want), strings.Join(added, "\n"))
	}
	for i, w := range want {
		if added[i] != w || added[len(want)+i] != w {
			t.Errorf("added line %d: %q / %q, want %q", i, added[i], added[len(want)+i], w)
		}
	}
	// The loki block sits right after the loki job, before the grafana job.
	s := string(out)
	if li, gi, bi := strings.Index(s, "job_name: loki"), strings.Index(s, "job_name: grafana"), strings.Index(s, "tvt:begin"); !(li < bi && bi < gi) {
		t.Errorf("block isn't inside the loki job:\n%s", s)
	}
	again, err := Prometheus(out, []string{"loki", "alertmanager"}, drop)
	if err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v): regeneration must be idempotent", err)
	}
	stripped, n, err := stripBlocks(out, "#")
	if err != nil || n != 2 || string(stripped) != string(src) {
		t.Errorf("stripping the blocks gives back the original? n=%d err=%v", n, err)
	}
	if _, err := Prometheus(src, []string{"shop"}, drop); err == nil {
		t.Error("a job the file doesn't scrape was accepted")
	}
}

func TestPrometheusAppends(t *testing.T) {
	src := []byte(`scrape_configs:
- job_name: node
  static_configs:
  - targets: [node:9100]
  metric_relabel_configs:
  - source_labels: [__name__]
    regex: node_scrape_.*
    action: drop
# trailing comment
`)
	out, err := Prometheus(src, []string{"node"}, Drop{ID: "x", Metrics: []string{"node_load15"}})
	if err != nil {
		t.Fatal(err)
	}
	added := onlyAdded(t, src, out)
	want := []string{"  # tvt:begin x scan=0", "  - source_labels: [__name__]", `    regex: "node_load15"`, "    action: drop", "  # tvt:end"}
	if strings.Join(added, "\n") != strings.Join(want, "\n") {
		t.Errorf("added:\n%s\nwant:\n%s", strings.Join(added, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(string(out), "action: drop\n  # tvt:begin") {
		t.Errorf("not appended after the existing rule:\n%s", out)
	}
	if _, err := Prometheus([]byte("scrape_configs:\n- job_name: n\n  metric_relabel_configs: []\n"), []string{"n"}, Drop{ID: "x", Metrics: []string{"a"}}); err == nil {
		t.Error("a flow-style list was edited")
	}
}

func TestAlloy(t *testing.T) {
	src := read(t, "config.alloy")
	out, err := Alloy(src, []string{"shop", "node"}, drop)
	if err != nil {
		t.Fatal(err)
	}
	added := onlyAdded(t, src, out)
	want := []string{
		"  // tvt:begin prom:metric:loki_ingester_chunk_age_seconds_bucket scan=42 evidence=ab12cd34",
		"  // no reader in 30d; 0 rules, 0 panels; $3.10/mo",
		"  rule {",
		`    source_labels = ["__name__"]`,
		`    regex         = "loki_ingester_chunk_age_seconds_bucket|loki_ingester_chunk_age_seconds_sum"`,
		`    action        = "drop"`,
		"  }",
		"  // tvt:end",
	}
	// shop and node both forward to prometheus.relabel "apps": one block.
	if strings.Join(added, "\n") != strings.Join(want, "\n") {
		t.Fatalf("added:\n%s\nwant:\n%s", strings.Join(added, "\n"), strings.Join(want, "\n"))
	}
	f, err := parser.ParseFile("out.alloy", out)
	if err != nil {
		t.Fatal(err)
	}
	var rules []*ast.BlockStmt
	for _, st := range f.Body {
		if b, ok := st.(*ast.BlockStmt); ok && strings.Join(b.Name, ".") == "prometheus.relabel" && b.Label == "apps" {
			for _, s := range b.Body {
				if r, ok := s.(*ast.BlockStmt); ok && r.Name[0] == "rule" {
					rules = append(rules, r)
				}
			}
		}
	}
	if len(rules) != 3 {
		t.Errorf("apps has %d rules, want the lab's 2 plus tvt's, last", len(rules))
	}
	again, err := Alloy(out, []string{"shop"}, drop)
	if err != nil || string(again) != string(out) {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	if _, err := Alloy(src, []string{"loki"}, drop); err == nil {
		t.Error("a job with no prometheus.scrape component was accepted")
	}
	bad := []byte("prometheus.scrape \"x\" {\n  targets = []\n  forward_to = local.thing.list\n}\n")
	if _, err := Alloy(bad, []string{"x"}, Drop{ID: "x", Metrics: []string{"a"}}); err == nil {
		t.Error("a forward_to that isn't a list of references was accepted")
	}
}

func TestStripBlocksRefusesBrokenBlocks(t *testing.T) {
	for _, src := range []string{
		"a\n# tvt:begin x scan=1\nb\n",
		"a\n# tvt:end\n",
		"# tvt:begin x scan=1\n# tvt:begin y scan=1\n# tvt:end\n",
	} {
		if _, _, err := stripBlocks([]byte(src), "#"); err == nil {
			t.Errorf("accepted %q", src)
		}
	}
}

func TestDropValidate(t *testing.T) {
	for _, d := range []Drop{
		{Metrics: []string{"a"}},
		{ID: "x"},
		{ID: "x", Metrics: []string{"a b"}},
		{ID: "x", Metrics: []string{"a"}, Reason: "two\nlines"},
	} {
		if err := d.validate(); err == nil {
			t.Errorf("accepted %+v", d)
		}
	}
	if r := (Drop{Metrics: []string{"b:c", "a", "a"}}).Regex(); r != "a|b:c" {
		t.Errorf("regex %q", r)
	}
}
