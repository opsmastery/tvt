package check

import (
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/propose"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// TestTenantTier: check judges remote_write drops (it read only scrape
// configs before), and lets a dormant metric leave the main remote_write
// when the retention tier's tenant keeps it (S10 item 2). A metric that
// isn't dormant, or a tier with no tenant in the manifest, is refused.
func TestTenantTier(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	base := "scrape_configs:\n  - job_name: shop\n    static_configs:\n      - targets: [shop:8080]\nremote_write:\n  - url: http://mimir:9009/api/v1/push\n"
	tier := propose.TenantTier{ID: "tenant:prom", Scan: 2, Metrics: []string{"shop_cache_evictions_total"}, Tenant: "tvt-dormant", KeepDays: 7}
	out, err := propose.PrometheusTenantTier([]byte(base), &tier)
	if err != nil {
		t.Fatal(err)
	}
	files := func(src string) Files { return func(string) ([]byte, error) { return []byte(src), nil } }
	sig := func(class string) report.ProtectedSignal {
		return report.ProtectedSignal{ID: "prom:metric:shop_cache_evictions_total", Kind: "metric", Name: "shop_cache_evictions_total", Pipeline: "prom", Class: class}
	}
	rules := func(tenant string, s report.ProtectedSignal, head string) map[string]bool {
		m := report.Manifest{Version: 2, Repo: "r", GeneratedAt: now, DormantTenant: tenant,
			PipelineDefs: []report.PipelineDef{{Name: "prom", Kind: "prometheus", Files: []string{"prometheus.yml"}}},
			Signals:      []report.ProtectedSignal{s}}
		got := map[string]bool{}
		for _, f := range Run(Input{Manifest: m, Base: files(base), Head: files(head), Now: now, SkipConfirmation: true}).Findings {
			got[f.Rule] = true
		}
		return got
	}
	if got := rules("tvt-dormant", sig("dormant"), string(out)); len(got) > 0 {
		t.Errorf("a dormant metric to the tier tenant: %v", got)
	}
	if got := rules("tvt-dormant", sig("hard_keep"), string(out)); !got[RuleProtected] {
		t.Errorf("a hard keep moved to the tier tenant: %v", got)
	}
	if got := rules("", sig("dormant"), string(out)); !got[RuleProtected] {
		t.Errorf("a tier with no tenant in the manifest: %v", got)
	}
	// A plain remote_write drop: refused, which check didn't see before.
	plain := base + "    write_relabel_configs:\n      - source_labels: [__name__]\n        regex: shop_cache_evictions_total\n        action: drop\n"
	if got := rules("tvt-dormant", sig("dormant"), plain); !got[RuleProtected] {
		t.Errorf("a remote_write drop of a dormant metric with no tier: %v", got)
	}
	// Unconfirmed, the tier is a drop that needs the owner's confirmation.
	m := report.Manifest{Version: 2, Repo: "r", GeneratedAt: now, DormantTenant: "tvt-dormant",
		PipelineDefs: []report.PipelineDef{{Name: "prom", Kind: "prometheus", Files: []string{"prometheus.yml"}}},
		Signals:      []report.ProtectedSignal{sig("dormant")}}
	r := Run(Input{Manifest: m, Base: files(base), Head: files(string(out)), Now: now})
	if len(r.Findings) != 1 || r.Findings[0].Rule != RuleUnconfirmed {
		t.Errorf("unconfirmed tier: %v", r.Findings)
	}
}
