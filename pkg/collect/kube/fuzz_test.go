package kube

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzResolve feeds arbitrary manifests to Load and Resolve: they must
// never panic, and a monitor is never both selected and not selected.
// Rules, Dashboards and Drops (S6 item 8) read the same objects and must
// not panic either.
func FuzzResolve(f *testing.F) {
	for _, name := range []string{"extra-monitors.yaml"} {
		b, err := os.ReadFile(filepath.Join(kpsDir, name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(b))
	}
	f.Add("apiVersion: monitoring.coreos.com/v1\nkind: Prometheus\nmetadata: {name: p}\nspec:\n  serviceMonitorSelector: {matchExpressions: [{key: a, operator: In, values: [1, true]}]}\n  serviceMonitorNamespaceSelector: {matchLabels: {x: y}}\n---\napiVersion: monitoring.coreos.com/v1\nkind: ServiceMonitor\nmetadata: {name: m, namespace: n, labels: {a: \"1\"}}\n")
	f.Add("kind: List\nitems: [{kind: List, items: []}]\n")
	f.Add("apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata: {name: r}\nspec: {groups: [{name: g, interval: 1m, rules: [{alert: A, expr: up, labels: {a: 1}}, {record: r, expr: [1]}]}]}\n")
	f.Add("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: d, labels: {grafana_dashboard: \"1\", grafana_datasource: \"1\"}}\ndata: {d.json: '{\"uid\":\"u\",\"panels\":[{\"type\":\"row\",\"panels\":[{\"id\":1}]}]}', ds.yaml: 'datasources: [{name: P}]'}\n")
	f.Fuzz(func(t *testing.T, doc string) {
		path := filepath.Join(t.TempDir(), "m.yaml")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		objs, err := Load([]string{path})
		if err != nil {
			return
		}
		sel := Resolve(objs, "")
		_, _ = Rules(objs)
		_, _ = Dashboards(objs)
		_, _ = Drops(objs, sel)
		not := map[string]bool{}
		for _, m := range sel.NotSelected() {
			not[m.Ref()] = true
		}
		for _, m := range sel.Monitors {
			if m.Selected() && not[m.Ref()] {
				t.Fatalf("%s both selected and not", m.Ref())
			}
		}
	})
}
