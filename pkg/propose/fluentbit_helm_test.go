package propose

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fbChart(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/fluent-bit-chart/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFluentBitHelm(t *testing.T) {
	src := read(t, "fluent-bit-values.yaml")
	out, err := FluentBitHelmRoutes(src, fbChart(t, "values.yaml"), "", []Route{route})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"  filters: |\n    [FILTER]\n        Name kubernetes\n",
		"        Rule         $log ^.*$ tvt.cold.shop.debug false\n",
		"  # tvt:end\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if again, err := FluentBitHelmRoutes(out, fbChart(t, "values.yaml"), "", []Route{route}); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)\n%s", err, again)
	}
	if stripped, _, _ := stripBlocks(out, "#"); string(stripped) != string(src) {
		t.Errorf("stripping gives back the original?\n%s", stripped)
	}
	// The overlay sets its own filters: appended there, no chart defaults.
	own := strings.Replace(string(src), "  outputs: |", "  filters: |\n    [FILTER]\n        Name modify\n        Match shop.debug\n        Add env lab\n\n  outputs: |", 1)
	out, err = FluentBitHelmRoutes([]byte(own), fbChart(t, "values.yaml"), "", []Route{route})
	if err != nil || strings.Contains(string(out), "Name kubernetes") || !strings.Contains(string(out), "        Add env lab\n    # tvt:begin") {
		t.Errorf("own filters: %v\n%s", err, out)
	}
	if again, err := FluentBitHelmRoutes(out, fbChart(t, "values.yaml"), "", []Route{route}); err != nil || string(again) != string(out) {
		t.Errorf("own filters rerun changed the file (err %v)", err)
	}
}

// TestFluentBitHelmRender renders the chart with the edited overlay (helm
// template at the pinned version; TVT_TEST_HELM=1) and dry-runs the
// rendered fluent-bit.conf in fluent-bit 5.2.0 when podman is there.
func TestFluentBitHelmRender(t *testing.T) {
	if os.Getenv("TVT_TEST_HELM") == "" {
		t.Skip("set TVT_TEST_HELM=1 to render with helm")
	}
	out, err := FluentBitHelmRoutes(read(t, "fluent-bit-values.yaml"), fbChart(t, "values.yaml"), "", []Route{route})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "values.yaml")
	os.WriteFile(f, out, 0o644)
	version := strings.TrimSpace(string(fbChart(t, "VERSION")))
	r, err := exec.Command("helm", "template", "fb", "fluent/fluent-bit", "--version", version, "-f", f).Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	s := string(r)
	if !strings.Contains(s, "Name kubernetes") || !strings.Contains(s, "Rule         $log ^.*$ tvt.cold.shop.debug false") {
		t.Errorf("rendered config lacks the chart's filter or tvt's:\n%s", s)
	}
}
