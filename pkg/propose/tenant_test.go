package propose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/validate"
)

var tenantTier = TenantTier{ID: "tenant:prometheus-scrape", Scan: 7, Reason: "2 dormant metrics keep a week", Metrics: []string{"shop_cache_evictions_total", "node_disk_io_errs_total"},
	Tenant: "tvt-dormant", KeepDays: 7}

const promWithRW = `global:
  scrape_interval: 15s
scrape_configs:
  - job_name: shop
    static_configs:
      - targets: [shop:8080]
remote_write:
  - url: http://mimir:9009/api/v1/push
    queue_config:
      max_shards: 4
`

func TestPrometheusTenantTier(t *testing.T) {
	out, err := PrometheusTenantTier([]byte(promWithRW), &tenantTier)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	regex := `"node_disk_io_errs_total|shop_cache_evictions_total"`
	for _, want := range []string{
		"    # tvt:begin tenant:prometheus-scrape scan=7\n    write_relabel_configs:\n      - source_labels: [__name__]\n        regex: " + regex + "\n        action: drop\n    # tvt:end\n",
		"  - url: \"http://mimir:9009/api/v1/push\"\n    name: tvt-dormant\n    headers:\n      X-Scope-OrgID: \"tvt-dormant\"\n",
		"        regex: " + regex + "\n        action: keep\n  # tvt:end\n",
		"compactor_blocks_retention_period to 7d",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if again, err := PrometheusTenantTier(out, &tenantTier); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	if back, err := PrometheusTenantTier(out, nil); err != nil || string(back) != promWithRW {
		t.Errorf("stripping didn't restore the file (err %v):\n%s", err, back)
	}
	// With write_relabel_configs already there, the drop is appended to it.
	withRules := strings.Replace(promWithRW, "    queue_config:\n", "    write_relabel_configs:\n      - source_labels: [__name__]\n        regex: go_.*\n        action: drop\n    queue_config:\n", 1)
	out, err = PrometheusTenantTier([]byte(withRules), &tenantTier)
	if err != nil || !strings.Contains(string(out), "        action: drop\n      # tvt:begin tenant:prometheus-scrape scan=7\n      - source_labels: [__name__]\n        regex: "+regex+"\n        action: drop\n      # tvt:end\n    queue_config:") {
		t.Errorf("appended (err %v):\n%s", err, out)
	}
	if back, _ := PrometheusTenantTier(out, nil); string(back) != withRules {
		t.Errorf("stripping the appended form:\n%s", back)
	}
	for name, src := range map[string]string{
		"no remote_write": "scrape_configs: []\n",
		"two":             promWithRW + "  - url: http://other/api/v1/push\n",
		"credentials":     strings.Replace(promWithRW, "    queue_config:\n", "    basic_auth:\n      username: u\n    queue_config:\n", 1),
		"same tenant":     strings.Replace(promWithRW, "    queue_config:\n", "    headers:\n      X-Scope-OrgID: tvt-dormant\n    queue_config:\n", 1),
	} {
		if _, err := PrometheusTenantTier([]byte(src), &tenantTier); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	bad := tenantTier
	bad.Tenant = ""
	if _, err := PrometheusTenantTier([]byte(promWithRW), &bad); err == nil {
		t.Error("no tenant: accepted")
	}
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		return
	}
	root, done, err := validate.Scratch()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	out, _ = PrometheusTenantTier([]byte(promWithRW), &tenantTier)
	os.WriteFile(filepath.Join(root, "prometheus.yml"), out, 0o644)
	if r := validate.Run(context.Background(), "prometheus", root, "prometheus.yml", validate.Options{NoNative: true}); !r.Ran || !r.OK {
		t.Errorf("%+v\n%s", r, out)
	}
}
