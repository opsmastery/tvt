package scan

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOwners is tvt-lab ground truth owners: (S9 item 1), with the lab's
// catalog-info.yaml and CODEOWNERS, plus a team label and a Kubernetes
// fallback the lab doesn't have.
func TestOwners(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "lab/shop/catalog-info.yaml", `apiVersion: backstage.io/v1alpha1
kind: Component
metadata:
  name: shop
  annotations:
    backstage.io/kubernetes-label-selector: app=shop
spec:
  type: service
  owner: group:default/shop-team
---
apiVersion: backstage.io/v1alpha1
kind: Component
metadata:
  name: alertmanager
spec:
  owner: group:default/sre
`)
	codeowners := write(t, dir, "lab/.github/CODEOWNERS", "# owners\n/alertmanager/ @jasonvance\n*.md @docs\n/billing/ @billing # trailing comment\n")
	k8s := write(t, dir, "k8s.yaml", `apiVersion: v1
kind: Namespace
metadata:
  name: payments
  labels: {team: payments-team}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ledger
  namespace: payments
  labels: {app.kubernetes.io/name: ledger}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: search
  namespace: default
  labels: {app.kubernetes.io/part-of: discovery}
`)
	cfg := config.Ownership{
		Labels:     []string{"team"},
		Owners:     []config.Owner{{Name: "reconciler", Streams: []string{`{app="reconciler"}`}}},
		CODEOWNERS: []string{codeowners},
		Backstage:  []string{filepath.Join(dir, "lab")},
		Services:   map[string]string{"coding-agent": "ai-platform", "discovery": "search-team"},
		Kubernetes: []string{k8s},
	}
	src, err := LoadOwnerSources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if src.CODEOWNERS["billing"] != "@billing" || src.CODEOWNERS["*.md"] != "" {
		t.Errorf("CODEOWNERS: %v", src.CODEOWNERS)
	}
	src.Labels = LabelOwners(collect.LabelMetrics{"team": {"checkout": {"checkout_orders_total", "mixed_total"}, "other": {"mixed_total"}}}, cfg.Labels)
	om, err := newOwnerMatcher(cfg.Owners)
	if err != nil {
		t.Fatal(err)
	}
	r := &ownerResolver{cfg: om, src: src, jobs: map[string][]string{
		"shop_search_requests_total":     {"shop"},
		"alertmanager_build_info":        {"alertmanager"},
		"coding_agent_token_usage_total": {"coding-agent"},
		"node_vmstat_oom_kill":           {"node"},
		"checkout_orders_total":          {"checkout"},
		"ledger_entries_total":           {"payments/ledger"},
		"search_queries_total":           {"search"},
		"mixed_total":                    {"shop"},
	}}
	metric := func(n string) graph.Signal { return graph.Signal{Kind: graph.KindMetric, Name: n} }
	stream := func(n string) graph.Signal { return graph.Signal{Kind: graph.KindStream, Name: n} }
	for _, c := range []struct {
		s             graph.Signal
		owner, source string
		inferred      bool
	}{
		{stream(`{app="reconciler", component="worker", level="error"}`), "reconciler", OwnerFromConfig, false},
		{metric("shop_search_requests_total"), "group:default/shop-team", OwnerFromBackstage, false},
		{graph.Signal{Kind: graph.KindLabel, Name: "user_id", Parent: "shop_search_requests_total"}, "group:default/shop-team", OwnerFromBackstage, false},
		{stream(`{app="shop", component="worker", level="debug"}`), "group:default/shop-team", OwnerFromBackstage, false},
		// CODEOWNERS comes before Backstage, which also names alertmanager.
		{metric("alertmanager_build_info"), "@jasonvance", OwnerFromCODEOWNERS, false},
		{metric("coding_agent_token_usage_total"), "ai-platform", OwnerFromService, true},
		{metric("checkout_orders_total"), "checkout", OwnerFromLabel, false},
		// Series that disagree on the team label name no owner from it.
		{metric("mixed_total"), "group:default/shop-team", OwnerFromBackstage, false},
		{stream(`{team="growth", app="x"}`), "growth", OwnerFromLabel, false},
		{metric("ledger_entries_total"), "payments-team", OwnerFromKubernetes, true},
		{metric("search_queries_total"), "search-team", OwnerFromKubernetes, true},
		// A metric with no job still matches a component by name prefix.
		{metric("billing_invoices_total"), "@billing", OwnerFromCODEOWNERS, false},
		{metric("node_vmstat_oom_kill"), "", "", false},
		{stream(`{app="unknown"}`), "", "", false},
	} {
		got := r.resolve(c.s)
		if got.name != c.owner || got.source != c.source || got.inferred != c.inferred {
			t.Errorf("%s: got %q from %q (inferred %v), want %q from %q (inferred %v)", c.s.Name, got.name, got.source, got.inferred, c.owner, c.source, c.inferred)
		}
	}
}

// TestClassifyOwned: classify records the owner's source, and a tvt.yaml
// owner still gives the tier.
func TestClassifyOwned(t *testing.T) {
	tier := 1
	signals := []graph.Signal{
		{ID: "prom:metric:coding_agent_token_usage_total", Kind: graph.KindMetric, Name: "coding_agent_token_usage_total", Source: "prom"},
		{ID: "prom:metric:pay_total", Kind: graph.KindMetric, Name: "pay_total", Source: "prom"},
	}
	src := OwnerSources{Services: map[string]string{"coding-agent": "ai-platform"}}
	owners := []config.Owner{{Name: "payments", Tier: &tier, Metrics: []string{"pay_.*"}}}
	if err := classifyOwned(signals, nil, nil, nil, owners, src, map[string][]string{"coding_agent_token_usage_total": {"coding-agent"}}, nil, nil, time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s := signals[0]; s.Owner != "ai-platform" || s.OwnerSource != OwnerFromService || !s.OwnerInferred || s.Tier != nil {
		t.Errorf("agent: %+v", s)
	}
	if s := signals[1]; s.Owner != "payments" || s.OwnerSource != OwnerFromConfig || s.OwnerInferred || s.Tier == nil || *s.Tier != 1 {
		t.Errorf("payments: %+v", s)
	}
}
