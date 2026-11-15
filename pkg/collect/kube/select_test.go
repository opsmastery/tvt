package kube

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const kpsDir = "../../../testdata/kube-prometheus-stack"

// TestResolveKubePrometheusStack resolves the pinned chart's render plus
// the hand-written extra monitors: every chart monitor carries
// release: kps and is selected; shop (no release label) and legacy-jobs
// (release: other) are not, and never own a scrape pool.
func TestResolveKubePrometheusStack(t *testing.T) {
	objs, err := Load([]string{filepath.Join(kpsDir, "render.yaml"), filepath.Join(kpsDir, "extra-monitors.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	sel := Resolve(objs, "monitoring")
	const server = "Prometheus/monitoring/kps-kube-prometheus-stack-prometheus"
	if !slices.Equal(sel.Servers, []string{server}) {
		t.Fatalf("servers = %v", sel.Servers)
	}
	var selected, chart int
	for _, m := range sel.Monitors {
		if strings.HasSuffix(m.File, "render.yaml") {
			chart++
			if !slices.Equal(m.SelectedBy, []string{server}) {
				t.Errorf("chart monitor %s selected by %v", m.Ref(), m.SelectedBy)
			}
		}
		if m.Selected() {
			selected++
		}
	}
	if chart != 13 {
		t.Errorf("chart monitors = %d, want the render's 13", chart)
	}
	if selected != chart+2 {
		t.Errorf("selected = %d, want %d (chart plus payments and batch)", selected, chart+2)
	}
	var not []string
	for _, m := range sel.NotSelected() {
		not = append(not, m.Ref())
	}
	want := []string{"PodMonitor/batch/legacy-jobs", "ServiceMonitor/shop/shop"}
	if !slices.Equal(not, want) {
		t.Errorf("not selected = %v, want %v", not, want)
	}
	if u := sel.Uncertain(); len(u) != 0 {
		t.Errorf("uncertain = %v", u)
	}
	if _, ok := sel.ForPool("serviceMonitor/shop/shop/0"); ok {
		t.Error("an unselected monitor owns a scrape pool")
	}
	if _, ok := sel.ForPool("podMonitor/batch/legacy-jobs/0"); ok {
		t.Error("an unselected pod monitor owns a scrape pool")
	}
	if m, ok := sel.ForPool("serviceMonitor/payments/payments/0"); !ok || m.Name != "payments" {
		t.Errorf("payments pool = %v, %v", m, ok)
	}
	if m, ok := sel.ForPool("podMonitor/batch/batch/0"); !ok || m.Kind != KindPodMonitor {
		t.Errorf("batch pool = %v, %v", m, ok)
	}
	if _, ok := sel.ForPool("serviceMonitor/payments/payments"); ok {
		t.Error("a malformed pool resolved")
	}
}

// TestLoadDirectory walks a directory, reads List documents and skips
// documents without a kind; a missing path is an error.
func TestLoadDirectory(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.yaml", "# only a comment\n---\nkind: List\napiVersion: v1\nitems:\n- apiVersion: v1\n  kind: Namespace\n  metadata: {name: shop}\n")
	write("b.yml", "replicas: 3\n")
	write("notes.txt", "kind: Namespace\n")
	objs, err := Load([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Kind != "Namespace" || objs[0].Name != "shop" {
		t.Fatalf("objs = %+v", objs)
	}
	if _, err := Load([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("missing path loaded")
	}
	write("bad.yaml", "kind: [\n")
	if _, err := Load([]string{dir}); err == nil {
		t.Error("invalid YAML loaded")
	}
}

func TestResolveSemantics(t *testing.T) {
	prom := func(ns, spec string) string {
		return "apiVersion: monitoring.coreos.com/v1\nkind: Prometheus\nmetadata: {name: p, namespace: " + ns + "}\nspec:\n" + spec + "\n---\n"
	}
	sm := func(ns, name, labels string) string {
		return "apiVersion: monitoring.coreos.com/v1\nkind: ServiceMonitor\nmetadata: {name: " + name + ", namespace: " + ns + ", labels: " + labels + "}\nspec: {}\n---\n"
	}
	nsObj := func(name, labels string) string {
		return "apiVersion: v1\nkind: Namespace\nmetadata: {name: " + name + ", labels: " + labels + "}\n---\n"
	}
	cases := []struct {
		name              string
		yaml              string
		selected, unknown []string
	}{
		{"nil selector selects none", prom("mon", "  replicas: 1") + sm("mon", "a", "{}"), nil, nil},
		{"null selector selects none", prom("mon", "  serviceMonitorSelector: null") + sm("mon", "a", "{}"), nil, nil},
		{"empty selector, nil namespace selector: own namespace", prom("mon", "  serviceMonitorSelector: {}") + sm("mon", "a", "{}") + sm("other", "b", "{}"),
			[]string{"mon/a"}, nil},
		{"empty namespace selector: every namespace", prom("mon", "  serviceMonitorSelector: {}\n  serviceMonitorNamespaceSelector: {}") + sm("mon", "a", "{}") + sm("other", "b", "{}"),
			[]string{"mon/a", "other/b"}, nil},
		{"matchLabels", prom("mon", "  serviceMonitorSelector: {matchLabels: {release: kps}}\n  serviceMonitorNamespaceSelector: {}") + sm("x", "a", "{release: kps}") + sm("x", "b", "{release: other}") + sm("x", "c", "{}"),
			[]string{"x/a"}, nil},
		{"unquoted label values compare as text", prom("mon", "  serviceMonitorSelector: {matchLabels: {scrape: \"true\"}}\n  serviceMonitorNamespaceSelector: {}") + sm("x", "a", "{scrape: true}"),
			[]string{"x/a"}, nil},
		{"expressions", prom("mon", "  serviceMonitorSelector:\n    matchExpressions:\n    - {key: team, operator: In, values: [a, b]}\n    - {key: tier, operator: NotIn, values: [\"0\"]}\n    - {key: scrape, operator: Exists}\n    - {key: legacy, operator: DoesNotExist}\n  serviceMonitorNamespaceSelector: {}") +
			sm("x", "ok", "{team: a, tier: \"1\", scrape: \"\"}") + sm("x", "noscrape", "{team: a}") + sm("x", "tier0", "{team: b, tier: \"0\", scrape: y}") + sm("x", "legacy", "{team: a, scrape: y, legacy: y}") + sm("x", "team", "{team: c, scrape: y}"),
			[]string{"x/ok"}, nil},
		{"namespace labels from Namespace objects", prom("mon", "  serviceMonitorSelector: {}\n  serviceMonitorNamespaceSelector: {matchLabels: {monitored: \"yes\"}}") +
			nsObj("a", "{monitored: \"yes\"}") + nsObj("b", "{}") + sm("a", "m", "{}") + sm("b", "m", "{}") + sm("c", "m", "{}"),
			[]string{"a/m"}, []string{"c/m"}},
		{"metadata.name needs no Namespace object", prom("mon", "  serviceMonitorSelector: {}\n  serviceMonitorNamespaceSelector:\n    matchExpressions: [{key: kubernetes.io/metadata.name, operator: In, values: [shop]}]") + sm("shop", "a", "{}") + sm("pay", "b", "{}"),
			[]string{"shop/a"}, nil},
		{"invalid selector is unknown", prom("mon", "  serviceMonitorSelector: {matchExpressions: [{key: a, operator: Like, values: [x]}]}") + sm("mon", "a", "{a: x}"),
			nil, []string{"mon/a"}},
		{"selected by one server, unknown to another is selected", prom("mon", "  serviceMonitorSelector: {}") +
			"apiVersion: monitoring.coreos.com/v1\nkind: PrometheusAgent\nmetadata: {name: agent, namespace: mon}\nspec:\n  serviceMonitorSelector: {matchLabels: [bad]}\n---\n" + sm("mon", "a", "{}"),
			[]string{"mon/a"}, nil},
		{"other API groups are ignored", prom("mon", "  serviceMonitorSelector: {}") + "apiVersion: example.com/v1\nkind: ServiceMonitor\nmetadata: {name: a, namespace: mon}\n---\n", nil, nil},
		{"no namespace: the default", prom("", "  serviceMonitorSelector: {}") + "apiVersion: monitoring.coreos.com/v1\nkind: ServiceMonitor\nmetadata: {name: a}\n---\n", []string{"dflt/a"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "m.yaml")
			if err := os.WriteFile(path, []byte(c.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			objs, err := Load([]string{path})
			if err != nil {
				t.Fatal(err)
			}
			sel := Resolve(objs, "dflt")
			var got, unknown []string
			for _, m := range sel.Monitors {
				if m.Selected() {
					got = append(got, m.Namespace+"/"+m.Name)
				}
			}
			for _, m := range sel.Uncertain() {
				unknown = append(unknown, m.Namespace+"/"+m.Name)
			}
			if !slices.Equal(got, c.selected) {
				t.Errorf("selected = %v, want %v", got, c.selected)
			}
			if !slices.Equal(unknown, c.unknown) {
				t.Errorf("unknown = %v, want %v", unknown, c.unknown)
			}
		})
	}
}
