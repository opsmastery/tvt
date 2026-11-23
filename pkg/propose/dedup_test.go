package propose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/validate"
)

var dedup = Dedup{ID: "dedup:otelcol-agent", Scan: 9, Reason: "the reconciler logs one line every 30s; nothing reads its stream",
	Services: []string{"reconciler"}, Records: []string{`attributes["event.name"] == "tool_result"`}, Collector: "0.165.0"}

func TestOtelcolDedup(t *testing.T) {
	src := read(t, "otelcol.yaml")
	out, err := OtelcolDedup(src, []string{"logs"}, &dedup)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"  # tvt:begin dedup:otelcol-agent scan=9\n",
		"  log_dedup/tvt:\n    interval: 1m\n    log_count_attribute: log_count\n    conditions:\n",
		`      - 'resource.attributes["service.name"] == "reconciler" and not (log.attributes["event.name"] == "tool_result")'`,
		"processors: [log_dedup/tvt, batch]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if again, err := OtelcolDedup(out, []string{"logs"}, &dedup); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	if back, err := OtelcolDedup(out, nil, nil); err != nil || string(back) != string(src) {
		t.Errorf("stripping didn't restore the file (err %v):\n%s", err, back)
	}
	// After a drop and an aggregation in the same file: those writers
	// strip every block, so the dedup goes on after them, and a dedup
	// rerun keeps theirs.
	both, err := OtelcolChanges(src, []string{"metrics"}, nil, &agg)
	if err != nil {
		t.Fatal(err)
	}
	both, err = OtelcolDedup(both, []string{"logs"}, &dedup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(both), OtelcolAggregateProcessor) || !strings.Contains(string(both), OtelcolDedupProcessor) {
		t.Errorf("composed:\n%s", both)
	}
	if again, err := OtelcolDedup(both, []string{"logs"}, &dedup); err != nil || string(again) != string(both) {
		t.Errorf("dedup rerun over an aggregate changed the file (err %v)", err)
	}
	for name, bad := range map[string]Dedup{
		"unscoped":   {ID: "dedup:x", Collector: "0.165.0"},
		"old":        {ID: "dedup:x", Services: []string{"a"}, Collector: "0.110.0"},
		"no version": {ID: "dedup:x", Services: []string{"a"}},
		"bad id":     {ID: "x", Services: []string{"a"}, Collector: "0.165.0"},
		"quote":      {ID: "dedup:x", Services: []string{"a"}, Records: []string{`body == 'x'`}, Collector: "0.165.0"},
		"subsecond":  {ID: "dedup:x", Services: []string{"a"}, Interval: 1500 * time.Millisecond, Collector: "0.165.0"},
	} {
		if _, err := OtelcolDedup(src, []string{"logs"}, &bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := OtelcolDedup(src, []string{"metrics"}, &dedup); err == nil {
		t.Error("a metrics pipeline was accepted")
	}
}

func TestLogContext(t *testing.T) {
	for in, want := range map[string]string{
		`attributes["event.name"] == "tool_result"`:            `log.attributes["event.name"] == "tool_result"`,
		`log.attributes["x"] == "y"`:                           `log.attributes["x"] == "y"`,
		`IsMatch(body, "attributes[") and severity_number > 8`: `IsMatch(log.body, "attributes[") and log.severity_number > 8`,
		`resource.attributes["service.name"] == "a"`:           `resource.attributes["service.name"] == "a"`,
		`attributes["somebody"] != nil`:                        `log.attributes["somebody"] != nil`,
	} {
		if got := logContext(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// TestOtelcolDedupValidate runs otelcol-contrib validate on the result.
func TestOtelcolDedupValidate(t *testing.T) {
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		t.Skip("set TVT_TEST_VALIDATORS=1 to run the pipelines' validators")
	}
	out, err := OtelcolDedup(read(t, "otelcol.yaml"), []string{"logs"}, &dedup)
	if err != nil {
		t.Fatal(err)
	}
	root, done, err := validate.Scratch()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	os.WriteFile(filepath.Join(root, "config.yaml"), out, 0o644)
	r := validate.Run(context.Background(), "otelcol", root, "config.yaml", validate.Options{NoNative: true})
	if !r.Ran || !r.OK {
		t.Errorf("%+v\n%s", r, out)
	}
}

// TestOtelcolNoService: a config with no service section is refused, not
// a crash (value used to dereference the missing section).
func TestOtelcolNoService(t *testing.T) {
	src := []byte("processors:\n  batch: {}\nexporters:\n  file/cold:\n")
	if _, err := OtelcolDedup(src, []string{"logs"}, &dedup); err == nil {
		t.Error("dedup: accepted")
	}
	tr := tier
	if _, err := OtelcolTier(src, "logs", &tr); err == nil {
		t.Error("tier: accepted")
	}
}
