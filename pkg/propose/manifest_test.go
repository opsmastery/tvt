package propose

import (
	"strings"
	"testing"
)

func TestEnsureProcessor(t *testing.T) {
	src := read(t, "builder-config.yaml")
	if !IsBuilderManifest(src) {
		t.Fatal("not recognised as a manifest")
	}
	d := Drop{ID: "otelcol-agent", Scan: 7}
	out, v, err := EnsureProcessor(src, FilterProcessorModule, d)
	if err != nil {
		t.Fatal(err)
	}
	if v != "v0.165.0" {
		t.Errorf("version %q, want the contrib components' v0.165.0", v)
	}
	added := onlyAdded(t, src, out)
	want := []string{
		"  # tvt:begin otelcol-agent scan=7",
		"  - gomod: " + FilterProcessorModule + " v0.165.0",
		"  # tvt:end",
	}
	if strings.Join(added, "\n") != strings.Join(want, "\n") {
		t.Errorf("added:\n%s\nwant:\n%s", strings.Join(added, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(string(out), "deltatocumulativeprocessor v0.165.0\n  # tvt:begin") {
		t.Errorf("not appended to processors:\n%s", out)
	}
	again, v2, err := EnsureProcessor(out, FilterProcessorModule, d)
	if err != nil || string(again) != string(out) || v2 != v {
		t.Errorf("rerun changed the manifest (err %v)", err)
	}
	// Listed by hand: nothing to add.
	hand := strings.Replace(string(src), "processors:\n", "processors:\n  - gomod: "+FilterProcessorModule+" v0.165.0\n", 1)
	if same, v, err := EnsureProcessor([]byte(hand), FilterProcessorModule, d); err != nil || v != "" || string(same) != hand {
		t.Errorf("already listed: v %q err %v", v, err)
	}
	// No processors section: one is added; core version when no contrib.
	bare := "dist:\n  name: x\nreceivers:\n  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.160.0\n"
	out, v, err = EnsureProcessor([]byte(bare), FilterProcessorModule, d)
	if err != nil || v != "v0.160.0" || !strings.HasSuffix(string(out), "# tvt:begin otelcol-agent scan=7\nprocessors:\n  - gomod: "+FilterProcessorModule+" v0.160.0\n# tvt:end\n") {
		t.Errorf("bare: v %q err %v\n%s", v, err, out)
	}
	if _, _, err := EnsureProcessor([]byte("dist:\n  name: x\nreceivers:\n  - gomod: example.com/r\n"), FilterProcessorModule, d); err == nil {
		t.Error("no version to match: want an error")
	}
	if IsBuilderManifest(read(t, "otelcol.yaml")) {
		t.Error("a Collector config isn't a manifest")
	}
}
