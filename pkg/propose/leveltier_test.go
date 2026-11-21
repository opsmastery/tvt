package propose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/validate"
)

var tier = Tier{ID: "tier:otelcol-agent", Scan: 11, Reason: "99.9% of the coding agent's stream is below warn and nothing reads it",
	Services: []string{"coding-agent"}, Records: []string{`attributes["event.name"] == "tool_result"`}, Cold: "file/cold", Collector: "0.165.0"}

// tierSource is the lab's Collector config with a cold exporter.
func tierSource(t *testing.T) []byte {
	return []byte(strings.Replace(string(read(t, "otelcol.yaml")), "  nop: {}\n", "  nop: {}\n  file/cold:\n    path: /tmp/cold.json\n", 1))
}

func TestOtelcolTier(t *testing.T) {
	src := tierSource(t)
	out, err := OtelcolTier(src, "logs", &tier)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	cond := `resource.attributes["service.name"] == "coding-agent" and log.severity_number > 0 and log.severity_number < 13 and IsString(log.body) and not IsMatch(log.body, "(?i)error|exception|traceback|critical|fatal") and not (log.attributes["event.name"] == "tool_result")`
	for _, want := range []string{
		"  # tvt:begin tier:otelcol-agent scan=11\n",
		"  filter/tvt-tier-hot:\n    error_mode: ignore\n    logs:\n      log_record:\n        - '" + cond + "'\n",
		"  filter/tvt-tier-cold:\n    error_mode: ignore\n    logs:\n      log_record:\n        - 'not (" + cond + ")'\n",
		"processors: [filter/tvt-tier-hot, batch]",
		"    logs/tvt-tier:\n      receivers: [otlp]\n      processors: [filter/tvt-tier-cold, batch]\n      exporters: [file/cold]\n    # tvt:end\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if again, err := OtelcolTier(out, "logs", &tier); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	if back, err := OtelcolTier(out, "", nil); err != nil || string(back) != string(src) {
		t.Errorf("stripping didn't restore the file (err %v):\n%s", err, back)
	}
	// After a drop and an aggregation, and with a dedup: each writer keeps
	// the others' blocks.
	both, err := OtelcolChanges(src, []string{"metrics"}, nil, &agg)
	if err != nil {
		t.Fatal(err)
	}
	if both, err = OtelcolDedup(both, []string{"logs"}, &dedup); err != nil {
		t.Fatal(err)
	}
	if both, err = OtelcolTier(both, "logs", &tier); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{OtelcolAggregateProcessor, OtelcolDedupProcessor, OtelcolTierHot, OtelcolTierPipeline} {
		if !strings.Contains(string(both), id) {
			t.Errorf("composed config lacks %s:\n%s", id, both)
		}
	}
	if again, err := OtelcolTier(both, "logs", &tier); err != nil || string(again) != string(both) {
		t.Errorf("tier rerun over the others changed the file (err %v)", err)
	}
	// The whole sequence again (dedup, then tier) gives the same file.
	again, err := OtelcolDedup(both, []string{"logs"}, &dedup)
	if err == nil {
		again, err = OtelcolTier(again, "logs", &tier)
	}
	if err != nil || string(again) != string(both) {
		t.Errorf("dedup and tier rerun changed the file (err %v)", err)
	}
	// Bare paths for a Collector older than the context form.
	old := tier
	old.Collector = "0.110.0"
	if out, err := OtelcolTier(src, "logs", &old); err != nil || strings.Contains(string(out), "log.severity_number") || !strings.Contains(string(out), " severity_number < 13") {
		t.Errorf("old Collector (err %v):\n%s", err, out)
	}
	if got := bareLogPaths(`log.body == "log.x" and resource.attributes["a"] == "b" and catalog.x`); got != `body == "log.x" and resource.attributes["a"] == "b" and catalog.x` {
		t.Errorf("bareLogPaths: %s", got)
	}
	for name, bad := range map[string]Tier{
		"unscoped":  {ID: "tier:x", Cold: "file/cold"},
		"bad id":    {ID: "x", Services: []string{"a"}, Cold: "file/cold"},
		"no cold":   {ID: "tier:x", Services: []string{"a"}},
		"quote":     {ID: "tier:x", Services: []string{"a"}, Records: []string{`body == 'x'`}, Cold: "file/cold"},
		"undefined": {ID: "tier:x", Services: []string{"a"}, Cold: "file/archive"},
		"hot cold":  {ID: "tier:x", Services: []string{"a"}, Cold: "otlp_http/loki"},
	} {
		if _, err := OtelcolTier(src, "logs", &bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, p := range []string{"metrics", "logs/missing", OtelcolTierPipeline} {
		if _, err := OtelcolTier(src, p, &tier); err == nil {
			t.Errorf("pipeline %s was accepted", p)
		}
	}
	taken := strings.Replace(string(src), "    logs:\n", "    logs/tvt-tier:\n      receivers: [otlp]\n      exporters: [nop]\n    logs:\n", 1)
	if _, err := OtelcolTier([]byte(taken), "logs", &tier); err == nil {
		t.Error("a user's logs/tvt-tier was overwritten")
	}
}

// TestOtelcolTierValidate runs otelcol-contrib validate on the result.
func TestOtelcolTierValidate(t *testing.T) {
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		t.Skip("set TVT_TEST_VALIDATORS=1 to run the pipelines' validators")
	}
	out, err := OtelcolTier(tierSource(t), "logs", &tier)
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
