package check

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/propose"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// TestLevelTier: check's guard for level tiers (S10 item 3). The tier
// tvt writes passes for an unread stream and for one whose readers keep
// to warn and above; a reader that may see lower levels refuses it, as
// does a record stream. A filter that can drop warn-and-above lines, or
// that drops with no cold copy when a cold tier is declared, is refused.
func TestLevelTier(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	const sel = `{service_name="coding-agent"}`
	const id = `loki:stream:` + sel
	base := "receivers:\n  otlp:\n    protocols:\n      http:\nprocessors:\n  batch: {}\nexporters:\n  otlp_http/loki:\n    endpoint: http://loki:3100/otlp\n  file/cold:\n    path: /tmp/cold.json\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      processors: [batch]\n      exporters: [otlp_http/loki]\n"
	tier := propose.Tier{ID: "tier:agent", Scan: 3, Services: []string{"coding-agent"}, Records: report.BuiltinRecordMatches.LogEvents, Cold: "file/cold", Collector: "0.165.0"}
	write := func(tr propose.Tier) string {
		out, err := propose.OtelcolTier([]byte(base), "logs", &tr)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	tiered := write(tier)
	noRecords := tier
	noRecords.Records = nil
	manifest := func(read []report.ReadSignal, sigs []report.ProtectedSignal) report.Manifest {
		return report.Manifest{Version: 2, Repo: "r", GeneratedAt: now, RecordMatches: report.BuiltinRecordMatches,
			PipelineDefs: []report.PipelineDef{{Name: "agent", Kind: "otelcol", Files: []string{"config.yaml"}, Cold: "file/cold"}},
			Read:         read, Signals: sigs}
	}
	files := func(src string) Files {
		return func(string) ([]byte, error) { return []byte(src), nil }
	}
	rules := func(m report.Manifest, head string) map[string]bool {
		got := map[string]bool{}
		for _, f := range Run(Input{Manifest: m, Base: files(base), Head: files(head), Now: now, SkipConfirmation: true}).Findings {
			got[f.Rule] = true
		}
		return got
	}
	read := func(warnPlus bool) []report.ReadSignal {
		return []report.ReadSignal{{ID: id, Kind: "stream", Name: sel, Pipeline: "agent", Readers: 1, WarnPlus: warnPlus}}
	}
	prot := func(warnPlus bool, record string) []report.ProtectedSignal {
		return []report.ProtectedSignal{{ID: id, Kind: "stream", Name: sel, Pipeline: "agent", Class: "keep_high", Record: record, WarnPlus: warnPlus}}
	}
	// A plain hot filter with no cold copy.
	hotOnly := tiered[:strings.Index(tiered, "    # tvt:begin tier:agent scan=3\n    logs/tvt-tier:")]
	// The tier's own condition with no level in it: a whole-service drop.
	unlevelled := strings.ReplaceAll(tiered, " and log.severity_number > 0 and log.severity_number < 13", "")
	for _, c := range []struct {
		name string
		m    report.Manifest
		head string
		want []string // rules; none means it passes
	}{
		{"unread", manifest(nil, nil), tiered, nil},
		{"read at warn and above", manifest(read(true), nil), tiered, nil},
		{"read below warn", manifest(read(false), nil), tiered, []string{RuleLeastLossy}},
		{"protected, warn and above", manifest(nil, prot(true, "")), tiered, nil},
		{"protected, below warn", manifest(nil, prot(false, "")), tiered, []string{RuleProtected}},
		{"record stream", manifest(nil, prot(true, "tool calls")), tiered, []string{RuleRecord}},
		{"records not excluded", manifest(nil, nil), write(noRecords), []string{RuleRecord}},
		{"no cold copy", manifest(nil, nil), hotOnly, []string{RuleLeastLossy}},
		{"warn and above dropped", manifest(nil, nil), unlevelled, []string{RuleWarnLogs}},
	} {
		got := rules(c.m, c.head)
		if len(c.want) == 0 && len(got) > 0 {
			t.Errorf("%s: want a pass, got %v", c.name, got)
		}
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%s: no %s finding: %v", c.name, w, got)
			}
		}
	}
	// Unconfirmed, the tier is a drop that needs an owner's confirmation.
	r := Run(Input{Manifest: manifest(nil, nil), Base: files(base), Head: files(tiered), Now: now})
	if len(r.Findings) != 1 || r.Findings[0].Rule != RuleUnconfirmed {
		t.Errorf("unconfirmed tier: %v", r.Findings)
	}
}

// TestLevelTierLines: an Alloy or Fluent Bit level tier (the reroute of
// the lines naming a level below warn) passes where its readers keep to
// warn and above, unlike a whole-stream reroute.
func TestLevelTierLines(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	const sel = `{app="shop", component="api"}`
	const id = "loki:stream:" + sel
	files := func(src string) Files { return func(string) ([]byte, error) { return []byte(src), nil } }
	rules := func(m report.Manifest, base, head string) map[string]bool {
		got := map[string]bool{}
		for _, f := range Run(Input{Manifest: m, Base: files(base), Head: files(head), Now: now, SkipConfirmation: true}).Findings {
			got[f.Rule] = true
		}
		return got
	}
	// Alloy.
	alloyBase, err := os.ReadFile("../propose/testdata/config.alloy")
	if err != nil {
		t.Fatal(err)
	}
	ab := strings.Replace(string(alloyBase), "loki.write \"loki\" {", "loki.write \"cold\" {\n  endpoint {\n    url = \"http://loki-cold:3100/loki/api/v1/push\"\n  }\n}\n\nloki.write \"loki\" {", 1)
	r := propose.Route{ID: "level_tier:" + id, Scan: 3, Stream: map[string]string{"app": "shop", "component": "api"}, Cold: "loki.write.cold", BelowWarn: true}
	tiered, err := propose.AlloyRoutes([]byte(ab), []propose.Route{r})
	if err != nil {
		t.Fatal(err)
	}
	r.BelowWarn = false
	whole, err := propose.AlloyRoutes([]byte(ab), []propose.Route{r})
	if err != nil {
		t.Fatal(err)
	}
	am := func(warnPlus bool) report.Manifest {
		return report.Manifest{Version: 2, Repo: "r", GeneratedAt: now,
			PipelineDefs: []report.PipelineDef{{Name: "apps", Kind: "alloy", Files: []string{"config.alloy"}, Cold: "loki.write.cold"}},
			Read:         []report.ReadSignal{{ID: id, Kind: "stream", Name: sel, Pipeline: "apps", Readers: 1, WarnPlus: warnPlus}}}
	}
	if got := rules(am(true), ab, string(tiered)); len(got) > 0 {
		t.Errorf("alloy tier, readers at warn and above: %v", got)
	}
	if got := rules(am(false), ab, string(tiered)); !got[RuleLeastLossy] {
		t.Errorf("alloy tier, a reader below warn: %v", got)
	}
	if got := rules(am(true), ab, string(whole)); !got[RuleLeastLossy] || !got[RuleWarnLogs] || got[RuleErrorGuard] {
		t.Errorf("alloy whole-stream reroute of a read stream: %v (the keep in front of loki.write.cold isn't a hot drop)", got)
	}
	if got := rules(am(true), ab, string(whole)); len(got) != 2 {
		t.Errorf("alloy whole-stream reroute: want least_lossy and warn_logs only, got %v", got)
	}
	// Fluent Bit.
	fb, err := os.ReadFile("../propose/testdata/fluent-bit-cold.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fr := propose.Route{ID: "level_tier:x", Scan: 3, Stream: map[string]string{"app": "shop", "component": "api", "level": "debug"}, Cold: "cold", BelowWarn: true}
	ftier, err := propose.FluentBit(fb, fr)
	if err != nil {
		t.Fatal(err)
	}
	fr.BelowWarn = false
	fwhole, err := propose.FluentBit(fb, fr)
	if err != nil {
		t.Fatal(err)
	}
	fm := func(warnPlus bool) report.Manifest {
		return report.Manifest{Version: 2, Repo: "r", GeneratedAt: now,
			PipelineDefs: []report.PipelineDef{{Name: "fb", Kind: "fluentbit", Files: []string{"fluent-bit.yaml"}, Cold: "cold"}},
			Signals:      []report.ProtectedSignal{{ID: id, Kind: "stream", Name: sel, Pipeline: "fb", Class: "keep_high", WarnPlus: warnPlus}}}
	}
	if got := rules(fm(true), string(fb), string(ftier)); len(got) > 0 {
		t.Errorf("fluent bit tier, readers at warn and above: %v", got)
	}
	if got := rules(fm(false), string(fb), string(ftier)); !got[RuleProtected] {
		t.Errorf("fluent bit tier, a reader below warn: %v", got)
	}
	if got := rules(fm(true), string(fb), string(fwhole)); !got[RuleProtected] {
		t.Errorf("fluent bit whole-stream reroute: %v", got)
	}
}

func TestLevelFilters(t *testing.T) {
	if !lineFilterLevel(`{app="a"} !~ "(?i)error" |~ ` + strconv.Quote(propose.LevelLinePattern)) {
		t.Error("tvt's level filter isn't one")
	}
	for _, f := range []string{`{app="a"} |~ "debug"`, `{app="a"} |~ ".*"`, `{app="a"} |= "level=debug"`, `{app="a"} !~ "level=(warn|error)"`} {
		if lineFilterLevel(f) {
			t.Errorf("%s: a level filter", f)
		}
	}
	if !lookaheadLevel(`$log ^(?!.*(?i:error))(?=.*(?i:` + strings.TrimPrefix(propose.LevelLinePattern, "(?i)") + `)).*$ tvt.cold.x false`) {
		t.Error("tvt's Fluent Bit level rule isn't one")
	}
	if lookaheadLevel(`$log ^(?!.*(?i:error)).*$ tvt.cold.x false`) || lookaheadLevel(`$log ^(?=.*debug).*$ t false`) {
		t.Error("a rule with no level lookahead counted")
	}
}
