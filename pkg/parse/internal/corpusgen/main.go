// Command corpusgen extracts PromQL expressions from kube-prometheus
// manifests into pkg/parse/testdata/promql-corpus/kube-prometheus.json.
// See that directory's README for the source commit and how to rerun it.
//
//	go run ./pkg/parse/internal/corpusgen <manifests dir> > pkg/parse/testdata/promql-corpus/kube-prometheus.json
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Opsmastery-Summit/tvt/pkg/parse/internal/corpus"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: corpusgen <kube-prometheus manifests dir>")
		os.Exit(2)
	}
	exprs, err := extract(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpusgen:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(exprs); err != nil {
		fmt.Fprintln(os.Stderr, "corpusgen:", err)
		os.Exit(1)
	}
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
