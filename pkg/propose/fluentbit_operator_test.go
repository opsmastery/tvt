package propose

import (
	"strings"
	"testing"
)

func TestFluentOperator(t *testing.T) {
	src := read(t, "fluent-operator.yaml")
	if !IsFluentOperator(src) {
		t.Fatal("not recognised")
	}
	out, err := FluentOperatorRoutes(src, []Route{route})
	if err != nil {
		t.Fatal(err)
	}
	want := `---
apiVersion: fluentbit.fluent.io/v1alpha2
kind: ClusterFilter
metadata:
  name: tvt-cold-shop-debug
  labels:
    fluentbit.fluent.io/component: "logging"
    fluentbit.fluent.io/enabled: "true"
spec:
  match: shop.debug
  filters:
    - rewriteTag:
        rules:
          - "$log ^(?!.*(?i:error|exception|traceback|critical|fatal)).*$ tvt.cold.shop.debug false"
        emitterName: tvt_cold_shop_debug
# tvt:end
`
	if !strings.HasSuffix(string(out), want) {
		t.Errorf("output:\n%s", out)
	}
	if again, err := FluentOperatorRoutes(out, []Route{route}); err != nil || string(again) != string(out) {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	if stripped, _, _ := stripBlocks(out, "#"); string(stripped) != string(src) {
		t.Errorf("stripping gives back the original?\n%s", stripped)
	}
	for _, c := range []struct{ name, src, want string }{
		{"no cold", strings.Replace(string(src), "name: cold", "name: archive", 1), `no ClusterOutput named "cold"`},
		{"cold mismatch", strings.Replace(string(src), "match: tvt.cold.*", "match: archive.*", 1), "doesn't match tvt.cold.shop.debug"},
		{"dynamic labels", strings.Replace(string(src), "      - level=debug\n", "      - level=debug\n      - host=$hostname\n", 1), "record fields"},
		{"parser", strings.Replace(string(src), "    path: /var/log/shop/debug.log\n", "    path: /var/log/shop/debug.log\n    parser: json\n", 1), "parses its records"},
	} {
		if _, err := FluentOperatorRoutes([]byte(c.src), []Route{route}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}
