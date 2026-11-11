package parse

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/parse/internal/corpus"
)

// TestPromQLCorpus parses every expression in the corpus (kube-prometheus
// rules and dashboards, plus the lab's dashboards and rules) and fails if
// the parse rate drops below testdata/promql-corpus/baseline. Raise the
// baseline when the rate improves.
func TestPromQLCorpus(t *testing.T) {
	exprs := loadCorpus(t)
	if len(exprs) < 500 {
		t.Fatalf("corpus has %d expressions; expected the kube-prometheus set", len(exprs))
	}
	var failed []string
	for _, e := range exprs {
		refs, err := PromQL(e.Expr)
		if err != nil {
			failed = append(failed, e.Source+": "+err.Error())
			continue
		}
		if len(refs) == 0 && !onlyLiterals(e.Expr) {
			t.Errorf("%s: parsed with no selectors: %q", e.Source, e.Expr)
		}
	}
	rate := float64(len(exprs)-len(failed)) / float64(len(exprs))
	b, err := os.ReadFile("testdata/promql-corpus/baseline")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	t.Logf("PromQL corpus parse rate %.4f (%d/%d), baseline %.4f", rate, len(exprs)-len(failed), len(exprs), baseline)
	for i, f := range failed {
		if i == 20 {
			t.Logf("... and %d more", len(failed)-20)
			break
		}
		t.Logf("parse failure: %s", f)
	}
	if rate < baseline {
		t.Errorf("parse rate %.4f is below the baseline %.4f", rate, baseline)
	}
}

func onlyLiterals(q string) bool {
	return strings.Trim(q, "0123456789.+-*/() \n\tvectorscalr") == ""
}

func loadCorpus(t *testing.T) []corpus.Expr {
	t.Helper()
	var exprs []corpus.Expr
	b, err := os.ReadFile("testdata/promql-corpus/kube-prometheus.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &exprs); err != nil {
		t.Fatal(err)
	}
	lab := filepath.Join("..", "..", "testdata", "lab")
	dashboards, _ := filepath.Glob(filepath.Join(lab, "grafana", "dashboard_*.json"))
	for _, f := range dashboards {
		var v any
		readJSON(t, f, &v)
		exprs = append(exprs, corpus.DashboardExprs("lab/"+filepath.Base(f), v)...)
	}
	for _, src := range []string{"prometheus", "mimir"} {
		var rules struct {
			Data struct {
				Groups []struct {
					Name  string `json:"name"`
					Rules []struct {
						Name  string `json:"name"`
						Query string `json:"query"`
					} `json:"rules"`
				} `json:"groups"`
			} `json:"data"`
		}
		readJSON(t, filepath.Join(lab, src, "rules.json"), &rules)
		for _, g := range rules.Data.Groups {
			for _, r := range g.Rules {
				exprs = append(exprs, corpus.Expr{Source: "lab/" + src + "/rules:" + g.Name + "/" + r.Name, Expr: r.Query})
			}
		}
	}
	return exprs
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// TestLogQLCorpus parses the committed LogQL corpus (testdata/logql-corpus,
// redacted design-partner queries) plus every Loki expression in the lab,
// prints the parse rate and fails if it drops below
// testdata/logql-corpus/baseline. See that directory's README.
func TestLogQLCorpus(t *testing.T) {
	lab := filepath.Join("..", "..", "testdata", "lab")
	exprs, err := corpus.LogQLFromPaths(filepath.Join(lab, "loki"), filepath.Join(lab, "grafana"))
	if err != nil {
		t.Fatal(err)
	}
	if len(exprs) < 2 {
		t.Fatalf("found %d lab Loki expressions, want the rule and the logs panel", len(exprs))
	}
	files, _ := filepath.Glob("testdata/logql-corpus/*.json")
	for _, f := range files {
		var es []corpus.Expr
		readJSON(t, f, &es)
		exprs = append(exprs, es...)
	}
	rate, failed := logqlParseRate(exprs)
	baseline := readBaseline(t, "testdata/logql-corpus/baseline")
	t.Logf("LogQL corpus parse rate %.4f (%d/%d, %d committed files), baseline %.4f", rate, len(exprs)-len(failed), len(exprs), len(files), baseline)
	logFailures(t, failed, 20)
	if rate < baseline {
		t.Errorf("parse rate %.4f is below the baseline %.4f", rate, baseline)
	}
}

// TestLogQLLocalCorpus parses LogQL that must not be committed, such as
// Loki's compiled mixins (AGPL), from the paths in TVT_LOGQL_CORPUS
// (separated like PATH), and prints the parse rate and failures. Skipped
// unless set. TVT_LOGQL_CORPUS_MIN (e.g. 0.95) makes a lower rate fail.
//
//	TVT_LOGQL_CORPUS=~/src/loki/production/loki-mixin-compiled go test ./pkg/parse -run TestLogQLLocalCorpus -v
func TestLogQLLocalCorpus(t *testing.T) {
	env := os.Getenv("TVT_LOGQL_CORPUS")
	if env == "" {
		t.Skip("TVT_LOGQL_CORPUS not set")
	}
	exprs, err := corpus.LogQLFromPaths(filepath.SplitList(env)...)
	if err != nil {
		t.Fatal(err)
	}
	if len(exprs) == 0 {
		t.Fatalf("no LogQL found under %s (Loki rule .yaml files and dashboard .json)", env)
	}
	rate, failed := logqlParseRate(exprs)
	t.Logf("local LogQL corpus parse rate %.4f (%d/%d)", rate, len(exprs)-len(failed), len(exprs))
	logFailures(t, failed, 100)
	if v := os.Getenv("TVT_LOGQL_CORPUS_MIN"); v != "" {
		min, err := strconv.ParseFloat(v, 64)
		if err != nil {
			t.Fatalf("TVT_LOGQL_CORPUS_MIN: %v", err)
		}
		if rate < min {
			t.Errorf("parse rate %.4f is below TVT_LOGQL_CORPUS_MIN %.4f", rate, min)
		}
	}
}

func logqlParseRate(exprs []corpus.Expr) (float64, []string) {
	var failed []string
	for _, e := range exprs {
		if _, err := LogQL(e.Expr); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v\n\t%s", e.Source, err, e.Expr))
		}
	}
	return float64(len(exprs)-len(failed)) / float64(len(exprs)), failed
}

func readBaseline(t *testing.T, path string) float64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

func logFailures(t *testing.T, failed []string, max int) {
	t.Helper()
	for i, f := range failed {
		if i == max {
			t.Logf("... and %d more", len(failed)-max)
			return
		}
		t.Logf("parse failure: %s", f)
	}
}

// TestRedactLogQL: redaction blanks values but keeps every reference.
func TestRedactLogQL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{app="acme-billing", env=~"prod-$region"} |= "card 4111" != ` + "`secret`",
			`{app="redacted", env=~"redacted$region"} |= "redacted" != "redacted"`},
		{`{app="a"} | json code="resp.code" | code >= 500 | line_format "{{.code}} for acme"`,
			`{app="redacted"} | json code="redacted" | code >= 500 | line_format "{{.code}} for acme"`},
		{"{app=\"a\"} | regexp `user=(?P<user>\\w+)` |~ \"x\" or \"y\" |= ip(\"10.1.0.0/16\")",
			"{app=\"redacted\"} | regexp `user=(?P<user>\\w+)` |~ \"redacted\" or \"redacted\" |= ip(\"192.0.2.0/24\")"},
		{`sum by (svc) (label_replace(rate({app="a"}[1m]), "svc", "$1", "service", "(.*)")) # "kept"`,
			`sum by (svc) (label_replace(rate({app="redacted"}[1m]), "svc", "$1", "service", "(.*)")) # "kept"`},
		{"{app= # team\n\"acme-private\"} |= # why\n\"card\" != ip( # net\n\"10.0.0.0/8\")",
			"{app= # team\n\"redacted\"} |= # why\n\"redacted\" != ip( # net\n\"192.0.2.0/24\")"},
	} {
		if got := corpus.RedactLogQL(tc.in); got != tc.want {
			t.Errorf("RedactLogQL(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
	// Over every table case: same refs apart from matcher values.
	blank := func(refs []Ref) []Ref {
		for i := range refs {
			ms := make([]Matcher, len(refs[i].Matchers))
			for j, m := range refs[i].Matchers {
				ms[j] = Matcher{Label: m.Label, Op: m.Op, Variable: m.Variable}
			}
			refs[i].Matchers = ms
		}
		return refs
	}
	for _, q := range logqlTableQueries() {
		want, werr := LogQL(q)
		got, gerr := LogQL(corpus.RedactLogQL(q))
		if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(blank(got), blank(want)) {
			t.Errorf("redacting %q changed its refs:\n got %+v (%v)\nwant %+v (%v)", q, got, gerr, want, werr)
		}
	}
}
