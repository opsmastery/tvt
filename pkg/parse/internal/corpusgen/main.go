// Command corpusgen builds the parser corpora in pkg/parse/testdata.
//
//	corpusgen kube-prometheus <manifests dir> > pkg/parse/testdata/promql-corpus/kube-prometheus.json
//	corpusgen logql -source partner-a <rule files, dashboards or dirs...> > pkg/parse/testdata/logql-corpus/partner-a.json
//
// kube-prometheus extracts PromQL from its manifests (see
// promql-corpus/README.md). logql extracts LogQL from Loki rule files
// (.yaml) and dashboards (.json, Loki panels) and redacts values (see
// logql-corpus/README.md); -no-redact keeps them, for a local check only.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/opsmastery/tvt/pkg/parse/internal/corpus"
)

const usage = `usage:
  corpusgen kube-prometheus <manifests dir>
  corpusgen logql -source <name> [-no-redact] <files or dirs...>`

func main() {
	if len(os.Args) < 2 {
		fail(2, usage)
	}
	var exprs []corpus.Expr
	var err error
	switch os.Args[1] {
	case "kube-prometheus":
		if len(os.Args) != 3 {
			fail(2, usage)
		}
		exprs, err = extract(os.Args[2])
	case "logql":
		fs := flag.NewFlagSet("logql", flag.ExitOnError)
		source := fs.String("source", "", "anonymous corpus name, e.g. partner-a (never a company name)")
		noRedact := fs.Bool("no-redact", false, "keep values (local use only; never commit the output)")
		_ = fs.Parse(os.Args[2:])
		if *source == "" || fs.NArg() == 0 {
			fail(2, usage)
		}
		exprs, err = corpus.LogQLFromPaths(fs.Args()...)
		for i := range exprs {
			exprs[i].Source = *source + "/" + exprs[i].Source
			if !*noRedact {
				exprs[i].Expr = corpus.RedactLogQL(exprs[i].Expr)
			}
		}
	default:
		fail(2, usage)
	}
	if err != nil {
		fail(1, "corpusgen: "+err.Error())
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(exprs); err != nil {
		fail(1, "corpusgen: "+err.Error())
	}
}

func fail(code int, msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(code)
}

func extract(dir string) ([]corpus.Expr, error) {
	var out []corpus.Expr
	rules, _ := filepath.Glob(filepath.Join(dir, "*-prometheusRule.yaml"))
	sort.Strings(rules)
	for _, f := range rules {
		var doc struct {
			Spec struct {
				Groups []struct {
					Name  string `yaml:"name"`
					Rules []struct {
						Alert  string `yaml:"alert"`
						Record string `yaml:"record"`
						Expr   string `yaml:"expr"`
					} `yaml:"rules"`
				} `yaml:"groups"`
			} `yaml:"spec"`
		}
		if err := readYAML(f, &doc); err != nil {
			return nil, err
		}
		for _, g := range doc.Spec.Groups {
			for i, r := range g.Rules {
				name := r.Alert + r.Record
				out = append(out, corpus.Expr{
					Source: fmt.Sprintf("rules/%s:%s/%d:%s", filepath.Base(f), g.Name, i, name),
					Expr:   strings.TrimSpace(r.Expr),
				})
			}
		}
	}
	var dash struct {
		Items []struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"items"`
	}
	if err := readYAML(filepath.Join(dir, "grafana-dashboardDefinitions.yaml"), &dash); err != nil {
		return nil, err
	}
	for _, it := range dash.Items {
		names := make([]string, 0, len(it.Data))
		for n := range it.Data {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			var v any
			if err := json.Unmarshal([]byte(it.Data[n]), &v); err != nil {
				return nil, fmt.Errorf("%s: %w", n, err)
			}
			es := corpus.DashboardExprs("dashboards/"+n, v)
			sort.Slice(es, func(i, j int) bool { return es[i].Source < es[j].Source })
			out = append(out, es...)
		}
	}
	return out, nil
}

func readYAML(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
