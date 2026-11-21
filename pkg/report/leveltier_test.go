package report

import (
	"math"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// The lab's coding-agent stream after tvt-lab #45: level in structured
// metadata, DEBUG model_stream chunks nobody reads, INFO events (two of
// them records) and a few WARN lines.
func levelTierInput() (Input, map[string]graph.Cost) {
	const agent = `{service_name="coding-agent"}`
	in := Input{
		StreamVolumes: []graph.StreamVolume{
			{Selector: agent, BytesPerDay: 1000, Levels: map[string]int64{"debug": 700, "info": 260, "warn": 40}},
			{Selector: `{app="shop", component="api", level="info"}`, BytesPerDay: 5000}, // level is a label: a reroute row's
			{Selector: `{service_name="quiet"}`, BytesPerDay: 100, Levels: map[string]int64{"warn": 60, "error": 40}},
		},
		Signals: []graph.Signal{
			{ID: "loki:stream:" + agent, Kind: graph.KindStream, Name: agent},
		},
		RecordLogEvents: BuiltinRecordMatches.LogEvents,
	}
	costs := map[string]graph.Cost{"loki:stream:" + agent: {BytesPerDay: 1000, MonthlyCost: 2}}
	return in, costs
}

func TestLevelTierRows(t *testing.T) {
	in, costs := levelTierInput()
	rows := levelTierRows(in, in.Signals, nil, nil, costs)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want the agent stream only (the shop stream's level is a label, the quiet one has nothing below warn): %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Held != "" || r.BelowWarn != 960 || math.Abs(r.Share-0.96) > 1e-9 || math.Abs(r.Monthly-1.92) > 1e-9 {
		t.Errorf("row %+v: want 960 bytes below warn, 0.96 of the stream, 1.92 a month", r)
	}
	if r.ErrorGuard != LevelErrorGuard || len(r.Records) != 2 || !strings.Contains(r.Records[0], "tool_result") {
		t.Errorf("guard %q, records %v", r.ErrorGuard, r.Records)
	}
}

func TestLevelTierReaders(t *testing.T) {
	in, costs := levelTierInput()
	sig := in.Signals[0].ID
	consumers := map[string]graph.Consumer{
		"panel:warn":  {ID: "panel:warn", Name: "Agent / warnings", Expr: `{service_name="coding-agent"} | detected_level=~"warn|error"`},
		"rule:count":  {ID: "rule:count", Name: "AgentLogVolume", Expr: `sum(count_over_time({service_name="coding-agent"}[5m]))`},
		"agent:sweep": {ID: "agent:sweep", Name: "what's here", Expr: `{service_name=~".+"}`},
	}
	refs := map[string][]graph.Reference{sig: {
		{SignalID: sig, ConsumerID: "panel:warn"},
		{SignalID: sig, ConsumerID: "agent:sweep", Sweep: true},
	}}
	rows := levelTierRows(in, in.Signals, refs, consumers, costs)
	if rows[0].Held != "" || len(rows[0].Readers) != 1 || rows[0].Readers[0].Sees != "warn and above" {
		t.Errorf("a warn-and-above reader holds nothing back (and a sweep isn't a reader): %+v", rows[0])
	}
	refs[sig] = append(refs[sig], graph.Reference{SignalID: sig, ConsumerID: "rule:count"})
	rows = levelTierRows(in, in.Signals, refs, consumers, costs)
	if rows[0].Held != "read below warn by AgentLogVolume" || len(rows[0].Readers) != 2 {
		t.Errorf("a count over every level holds the tier: %+v", rows[0])
	}
	// A reader of the whole stream space (signal "*") reads it too.
	in.Signals = append(in.Signals, graph.Signal{ID: "loki:stream:*", Kind: graph.KindStream, Name: "*"})
	refs = map[string][]graph.Reference{"loki:stream:*": {{SignalID: "loki:stream:*", ConsumerID: "rule:count"}}}
	if rows = levelTierRows(in, in.Signals, refs, consumers, costs); rows[0].Held == "" {
		t.Errorf("a reader of every stream holds it: %+v", rows[0])
	}
}

func TestLevelTierRecordStream(t *testing.T) {
	in, costs := levelTierInput()
	in.Signals[0].Record = "listed in records.streams"
	rows := levelTierRows(in, in.Signals, nil, nil, costs)
	if !strings.HasPrefix(rows[0].Held, "a record stream") {
		t.Errorf("held %q, want a record stream", rows[0].Held)
	}
}

func TestLevelTierHTML(t *testing.T) {
	in, costs := levelTierInput()
	r := Report{Version: Version, LevelTiers: levelTierRows(in, in.Signals, nil, nil, costs)}
	var buf strings.Builder
	if err := RenderHTML(&buf, r); err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	for _, want := range []string{"<h2>Level tiers</h2>", `{service_name=&#34;coding-agent&#34;}`, "debug 700 B", "96"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// TestManifestWarnPlus: a stream's readers keep to warn and above only
// when every one has a level matcher at warn or above.
func TestManifestWarnPlus(t *testing.T) {
	exprs := map[string]string{
		"warn": `sum(count_over_time({service_name="a"} | detected_level=~"warn|error" [5m]))`,
		"all":  `sum(count_over_time({service_name="a"} [5m]))`,
		"info": `{service_name="a"} | detected_level="info"`,
	}
	refs := func(ids ...string) []graph.Reference {
		var out []graph.Reference
		for _, id := range ids {
			out = append(out, graph.Reference{ConsumerID: id})
		}
		return out
	}
	for _, c := range []struct {
		ids  []string
		want bool
	}{{nil, false}, {[]string{"warn"}, true}, {[]string{"warn", "all"}, false}, {[]string{"info"}, false}} {
		if got := warnPlus(refs(c.ids...), exprs); got != c.want {
			t.Errorf("%v: %v, want %v", c.ids, got, c.want)
		}
	}
}
