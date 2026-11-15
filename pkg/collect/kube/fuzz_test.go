package kube

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzResolve feeds arbitrary manifests to Load and Resolve: they must
// never panic, and a monitor is never both selected and not selected.
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
