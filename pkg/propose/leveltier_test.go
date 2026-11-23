package propose

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/validate"
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

func TestOtelcolLogsShipper(t *testing.T) {
	lab := `connectors:
  count/x:
exporters:
  otlp_http/loki:
  file/cold:
service:
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [otlp_http/loki]
    logs/shop-errors:
      receivers: [otlp/shop]
      exporters: [count/x]
    logs/tvt-tier:
      receivers: [otlp]
      exporters: [file/cold]
    logs/archive:
      receivers: [otlp]
      exporters: [file/cold]
    metrics:
      receivers: [otlp]
      exporters: [otlp_http/loki]
`
	if got, err := OtelcolLogsShipper([]byte(lab), "file/cold"); err != nil || got != "logs" {
		t.Errorf("lab: %q, %v", got, err)
	}
	two := strings.Replace(lab, "exporters: [count/x]", "exporters: [count/x, otlp_http/loki]", 1)
	if _, err := OtelcolLogsShipper([]byte(two), "file/cold"); err == nil {
		t.Error("two shippers: no error")
	}
	if got, err := OtelcolLogsShipper([]byte("receivers:\n  otlp:\n"), "file/cold"); err != nil || got != "" {
		t.Errorf("no pipelines: %q, %v", got, err)
	}
}

// TestLevelLinePattern: a line names its level below warn as a key, in
// logfmt or JSON, any case; a bare word or a warn level doesn't count.
func TestLevelLinePattern(t *testing.T) {
	re := regexp.MustCompile(LevelLinePattern)
	for line, want := range map[string]bool{
		`ts=1 level=debug msg="chunk"`: true,
		`level=INFO msg=ok`:            true,
		`{"level":"info","msg":"ok"}`:  true,
		`{"ts":1, "lvl": "trace"}`:     true,
		`severity=Debug x=1`:           true,
		`level=warn msg="slow"`:        false,
		`level=information x`:          false,
		`DEBUG starting worker`:        false,
		// The pattern doesn't parse quotes: a level key inside a message counts.
		`msg="set level=debug for the next hour"`: true,
		`loglevel=debug`:                      false,
		`level=debugging`:                     false,
		`{"level":"error","msg":"info only"}`: false,
	} {
		if got := re.MatchString(line); got != want {
			t.Errorf("%q: %v, want %v", line, got, want)
		}
	}
}

// tierRoute is the shop's api stream as a level tier (its level isn't a
// label there).
var tierRoute = Route{ID: "level_tier:loki:stream:shop-api", Scan: 9, Evidence: "feedfacecafe", Reason: "lines below warn nobody reads",
	Stream: map[string]string{"app": "shop", "component": "api", "level": "debug"}, Cold: "cold", BelowWarn: true}

func TestFluentBitLevelTier(t *testing.T) {
	src := read(t, "fluent-bit-cold.yaml")
	out, err := FluentBit(src, tierRoute)
	if err != nil {
		t.Fatal(err)
	}
	want := "      rule: $log ^(?!.*(?i:error|exception|traceback|critical|fatal))(?=.*(?i:" + strings.TrimPrefix(LevelLinePattern, "(?i)") + ")).*$ tvt.cold.shop.debug false\n"
	if !strings.Contains(string(out), want) {
		t.Errorf("lacks %q:\n%s", want, out)
	}
	if again, err := FluentBit(out, tierRoute); err != nil || string(again) != string(out) {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	classic, err := FluentBitClassicRoutes(read(t, "fluent-bit-cold.conf"), []Route{tierRoute})
	if err != nil || !strings.Contains(string(classic), "Rule         $log "+tierRoute.fluentRegex()+" tvt.cold.shop.debug false") {
		t.Errorf("classic (err %v):\n%s", err, classic)
	}
	if strings.Contains(tierRoute.fluentRegex(), " ") {
		t.Error("the rule's regex has a space: rewrite_tag splits the rule on spaces")
	}
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		return
	}
	root, done, err := validate.Scratch()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	os.WriteFile(filepath.Join(root, "fluent-bit.yaml"), out, 0o644)
	if r := validate.Run(context.Background(), "fluentbit", root, "fluent-bit.yaml", validate.Options{NoNative: true}); !r.Ran || !r.OK {
		t.Errorf("%+v\n%s", r, out)
	}
}

func TestAlloyLevelTier(t *testing.T) {
	src := strings.Replace(string(read(t, "config.alloy")), "loki.write \"loki\" {", "loki.write \"cold\" {\n  endpoint {\n    url = \"http://loki-cold:3100/loki/api/v1/push\"\n  }\n}\n\nloki.write \"loki\" {", 1)
	r := tierRoute
	r.Stream = map[string]string{"app": "shop", "component": "api"}
	r.Cold = "loki.write.cold"
	out, err := AlloyRoutes([]byte(src), []Route{r})
	if err != nil {
		t.Fatal(err)
	}
	sel := `{app="shop", component="api"} !~ ` + strconv.Quote(ErrorPattern) + ` |~ ` + strconv.Quote(LevelLinePattern)
	want := "selector = " + strconv.Quote(sel)
	if !strings.Contains(string(out), want) {
		t.Errorf("lacks %s:\n%s", want, out)
	}
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		return
	}
	root, done, err := validate.Scratch()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	os.WriteFile(filepath.Join(root, "config.alloy"), out, 0o644)
	if r := validate.Run(context.Background(), "alloy", root, "config.alloy", validate.Options{NoNative: true}); !r.Ran || !r.OK {
		t.Errorf("%+v\n%s", r, out)
	}
}
