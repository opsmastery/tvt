package parse

import (
	"encoding/json"
	"os"
	"path/filepath"
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
