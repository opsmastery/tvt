package propose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/validate"
)

func TestPrometheusIntervals(t *testing.T) {
	src := read(t, "lab-scrape.yml")
	byJob := map[string]Interval{
		"loki":         {ID: "interval:loki", Scan: 9, Reason: "one instant reader: 2m keeps two samples in the 5m lookback", Every: 2 * time.Minute},
		"alertmanager": {ID: "interval:alertmanager", Scan: 9, Every: time.Minute},
	}
	out, err := PrometheusIntervals(src, byJob)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"    # tvt:begin interval:loki scan=9\n",
		"    # one instant reader: 2m keeps two samples in the 5m lookback\n    scrape_interval: 2m\n    # tvt:end\n",
		"    # tvt:original scrape_interval: 15s\n    scrape_interval: 1m\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "\n    scrape_interval: 15s\n") {
		t.Error("the job's own 15s line is still live")
	}
	// Rerun: identical. Strip: the original back, byte for byte.
	if again, err := PrometheusIntervals(out, byJob); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	if back, err := PrometheusIntervals(out, nil); err != nil || string(back) != string(src) {
		t.Errorf("stripping didn't restore the file (err %v):\n%s", err, back)
	}
	// With drops: the drop writer strips every block, so intervals go on
	// after it, and they survive a drop rerun in that order.
	drops, err := PrometheusDrops(src, map[string]Drop{"loki": {ID: "prometheus-scrape/loki", Scan: 9, Metrics: []string{"loki_build_info"}}})
	if err != nil {
		t.Fatal(err)
	}
	both, err := PrometheusIntervals(drops, byJob)
	if err != nil {
		t.Fatal(err)
	}
	if _, n, err := stripBlocks(both, "#"); err != nil || n != 3 {
		t.Errorf("blocks %d err %v (want the drop and two intervals, none nested):\n%s", n, err, both)
	}
	if again, err := PrometheusIntervals(both, byJob); err != nil || string(again) != string(both) {
		t.Errorf("interval rerun over drops changed the file (err %v)", err)
	}
	if _, err := PrometheusIntervals(src, map[string]Interval{"nope": {ID: "interval:nope", Every: time.Minute}}); err == nil {
		t.Error("a missing job was accepted")
	}
	if _, err := PrometheusIntervals(src, map[string]Interval{"loki": {ID: "loki", Every: time.Minute}}); err == nil {
		t.Error("an ID without the interval: prefix was accepted")
	}
}

func TestAlloyIntervals(t *testing.T) {
	src := read(t, "config.alloy")
	byJob := map[string]Interval{"shop": {ID: "interval:shop", Scan: 9, Reason: "every reader's range keeps 4 samples at 30s", Every: 30 * time.Second}}
	out, err := AlloyIntervals(src, byJob)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"// tvt:begin interval:shop scan=9",
		`// tvt:original scrape_interval = "10s"`,
		`scrape_interval = "30s"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "  scrape_interval = \"10s\"") != 1 {
		t.Errorf("node's 10s should be the only live 10s:\n%s", s)
	}
	if again, err := AlloyIntervals(out, byJob); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	if back, err := AlloyIntervals(out, nil); err != nil || string(back) != string(src) {
		t.Errorf("stripping didn't restore the file (err %v)", err)
	}
	// With the drop writer's block in a relabel component, first.
	drops, err := Alloy(src, []string{"shop"}, Drop{ID: "alloy-apps", Scan: 9, Metrics: []string{"shop_cart_operation_total"}})
	if err != nil {
		t.Fatal(err)
	}
	both, err := AlloyIntervals(drops, byJob)
	if err != nil {
		t.Fatal(err)
	}
	if _, n, err := stripBlocks(both, "//"); err != nil || n != 2 {
		t.Errorf("blocks %d err %v", n, err)
	}
}

// TestIntervalsValidate runs promtool and alloy on the edited files.
func TestIntervalsValidate(t *testing.T) {
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		t.Skip("set TVT_TEST_VALIDATORS=1 to run the pipelines' validators")
	}
	prom, err := PrometheusIntervals(read(t, "lab-scrape.yml"), map[string]Interval{
		"loki": {ID: "interval:loki", Every: 2 * time.Minute}, "alertmanager": {ID: "interval:alertmanager", Every: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	alloy, err := AlloyIntervals(read(t, "config.alloy"), map[string]Interval{"shop": {ID: "interval:shop", Every: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind, file string
		body       []byte
	}{{"prometheus", "prometheus.yml", prom}, {"alloy", "config.alloy", alloy}} {
		root, done, err := validate.Scratch()
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(root, c.file), c.body, 0o644)
		r := validate.Run(context.Background(), c.kind, root, c.file, validate.Options{NoNative: true})
		done()
		if !r.Ran || !r.OK {
			t.Errorf("%s: %+v\n%s", c.kind, r, c.body)
		}
	}
}
