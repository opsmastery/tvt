package parse

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestOTTLStatement(t *testing.T) {
	for _, c := range []struct {
		in           string
		editor       string
		target       string
		reads, where []string
		funcs        []string
	}{
		{
			in:     `set(log.attributes["namespace"], resource.attributes["k8s.namespace.name"])`,
			editor: "set", target: `log.attributes["namespace"]`,
			reads: []string{`resource.attributes["k8s.namespace.name"]`}, funcs: []string{"set"},
		},
		{
			in:     `delete_key(log.attributes, "http.request.header.authorization") where log.attributes["http.request.header.authorization"] != nil`,
			editor: "delete_key", target: "log.attributes",
			reads: []string{`log.attributes["http.request.header.authorization"]`},
			where: []string{`log.attributes["http.request.header.authorization"]`}, funcs: []string{"delete_key"},
		},
		{
			// tvt's own pseudonymise statement (S7 item 6).
			in:     `set(span.attributes["session.id"], Substring(SHA256(String(span.attributes["session.id"])), 0, 16)) where span.attributes["session.id"] != nil and not IsMatch(String(span.attributes["session.id"]), "^[0-9a-f]{16}$")`,
			editor: "set", target: `span.attributes["session.id"]`,
			reads: []string{`span.attributes["session.id"]`}, where: []string{`span.attributes["session.id"]`},
			funcs: []string{"set", "Substring", "SHA256", "String", "IsMatch"},
		},
		{
			in:     `convert_sum_to_gauge() where metric.name == "system.processes.count"`,
			editor: "convert_sum_to_gauge", reads: []string{"metric.name"}, where: []string{"metric.name"},
			funcs: []string{"convert_sum_to_gauge"},
		},
		{
			// A computed key, a converter's key and math.
			in:     `set(attributes[Concat(["a", resource.attributes["b"]], ".")], Split(body, ",")[1]) where end_time_unix_nano - start_time_unix_nano > 1000`,
			editor: "set", target: `attributes[*]`,
			reads: []string{`resource.attributes["b"]`, "body", "end_time_unix_nano", "start_time_unix_nano"},
			where: []string{"end_time_unix_nano", "start_time_unix_nano"}, funcs: []string{"set", "Concat", "Split"},
		},
		{
			// Named arguments, a map literal and an enum.
			in:     `set(span.status.code, STATUS_CODE_ERROR) where span.attributes["http.status_code"] >= 500 or (span.kind == SPAN_KIND_SERVER and IsMatch(span.name, ".*"))`,
			editor: "set", target: "span.status.code",
			reads: []string{`span.attributes["http.status_code"]`, "span.kind", "span.name"},
			where: []string{`span.attributes["http.status_code"]`, "span.kind", "span.name"}, funcs: []string{"set", "IsMatch"},
		},
		{
			in:     `merge_maps(log.attributes, ParseJSON(log.body), "upsert", ignore_errors = true)`,
			editor: "merge_maps", target: "log.attributes", reads: []string{"log.body"}, funcs: []string{"merge_maps", "ParseJSON"},
		},
	} {
		o, err := OTTLStatement(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if o.Editor != c.editor || o.Target != c.target || !reflect.DeepEqual(o.Reads, c.reads) ||
			!reflect.DeepEqual(o.WhereReads, c.where) || !reflect.DeepEqual(o.Funcs, c.funcs) {
			t.Errorf("%s:\n got editor %q target %q reads %q where %q funcs %q", c.in, o.Editor, o.Target, o.Reads, o.WhereReads, o.Funcs)
		}
		if !o.Modeled() {
			t.Errorf("%s: unmodeled %v", c.in, o.Unmodeled)
		}
	}
}

func TestOTTLFailClosed(t *testing.T) {
	o, err := OTTLStatement(`merge_maps(span.attributes, MapEach(Filter(span.attributes, (k, v) => IsMatch(k, "(?i)email")), (k, v) => "REDACTED"), "upsert")`)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Lambda || !o.Experimental || o.Modeled() {
		t.Errorf("lambda %v experimental %v modeled %v", o.Lambda, o.Experimental, o.Modeled())
	}
	if !reflect.DeepEqual(o.Reads, []string{"span.attributes"}) {
		t.Errorf("lambda parameters read as paths: %q", o.Reads)
	}
	o, err = OTTLStatement(`redact_everything(attributes) where IsCompanySecret(name)`)
	if err != nil || o.Modeled() || !reflect.DeepEqual(o.Unmodeled, []string{"redact_everything", "IsCompanySecret"}) {
		t.Errorf("unknown functions: %v %v", err, o.Unmodeled)
	}
	for _, bad := range []string{
		`span.name == "x"`,           // a condition, not a statement
		`set(attributes["a"], "b"`,   // unclosed
		`set(attributes["a"]) where`, // empty where
		`IsMatch(name, "a")`,         // a converter is no editor
	} {
		if _, err := OTTLStatement(bad); err == nil {
			t.Errorf("%s: parsed", bad)
		}
	}
}

func TestOTTLCondition(t *testing.T) {
	o, err := OTTLCondition(`resource.attributes["service.name"] == "noisy" and log.severity_number < SEVERITY_NUMBER_WARN and not IsMatch(log.body, "error|fatal")`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`resource.attributes["service.name"]`, "log.severity_number", "log.body"}
	if o.Editor != "" || !reflect.DeepEqual(o.Reads, want) || !reflect.DeepEqual(o.WhereReads, want) || !o.Modeled() {
		t.Errorf("got %+v", o)
	}
	if _, err := OTTLCondition(`otelcol.client.metadata["X-Tenant"][0] == "ecorp"`); err != nil {
		t.Error(err)
	}
}

// ottlCorpusEntry is one line of testdata/ottl-corpus/*.jsonl.
type ottlCorpusEntry struct {
	Source string `json:"source"`
	Kind   string `json:"kind"` // statement or condition
	Text   string `json:"text"`
	// Invalid: a docs example no Collector accepts (a typo, PromQL), with
	// why. Left out of the rate; it must still fail to parse.
	Invalid string `json:"invalid,omitempty"`
}

// TestOTTLCorpus is ADR 0012's gate. Every OTTL statement and condition
// in testdata/ottl-corpus, plus those in the lab's Collector configs, is
// parsed. Recognized means it parsed and every function is one tvt knows
// (standard, a processor's own, or experimental); the share recognized
// must stay at or above testdata/ottl-corpus/baseline, which starts at the
// ADR's 0.95, or the ADR reopens for contrib's parser. Statements that use
// a lambda or an experimental function are recognized but not modeled:
// check fails closed on them, and the test reports how many. Raise the
// baseline when the rate improves; never lower it to make a change pass.
func TestOTTLCorpus(t *testing.T) {
	entries := loadOTTLCorpus(t)
	if len(entries) < 400 {
		t.Fatalf("corpus has %d entries; expected the contrib docs at least", len(entries))
	}
	type tally struct{ total, recognized, modeled int }
	var all tally
	bySource := map[string]*tally{}
	var failed, unknown, closed []string
	invalid := 0
	for _, e := range entries {
		var o OTTL
		var err error
		if e.Kind == "statement" {
			o, err = OTTLStatement(e.Text)
		} else {
			o, err = OTTLCondition(e.Text)
		}
		if e.Invalid != "" {
			invalid++
			if err == nil {
				t.Errorf("%s: %q is marked invalid (%s) but parsed", e.Source, e.Text, e.Invalid)
			}
			continue
		}
		group := strings.Fields(e.Source)[0]
		g := bySource[group]
		if g == nil {
			g = &tally{}
			bySource[group] = g
		}
		g.total++
		all.total++
		switch {
		case err != nil:
			failed = append(failed, e.Source+": "+e.Text+": "+err.Error())
		case len(o.Unmodeled) > 0:
			unknown = append(unknown, e.Source+": "+e.Text+": "+strings.Join(o.Unmodeled, ", "))
		default:
			g.recognized++
			all.recognized++
			if o.Modeled() {
				g.modeled++
				all.modeled++
			} else {
				closed = append(closed, e.Source+": "+e.Text)
			}
		}
	}
	rate := float64(all.recognized) / float64(all.total)
	b, err := os.ReadFile("testdata/ottl-corpus/baseline")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	t.Logf("OTTL corpus: %d entries (%d invalid docs examples left out); recognized %.4f (%d), modeled %.4f (%d; %d fail closed: lambda or experimental); baseline %.4f",
		all.total, invalid, rate, all.recognized, float64(all.modeled)/float64(all.total), all.modeled, len(closed), baseline)
	for _, k := range sortedKeys(bySource) {
		g := bySource[k]
		t.Logf("  %s: %d recognized, %d modeled, of %d", k, g.recognized, g.modeled, g.total)
	}
	for _, f := range failed {
		t.Logf("parse failure: %s", f)
	}
	for _, f := range unknown {
		t.Logf("unknown function: %s", f)
	}
	if rate < baseline {
		t.Errorf("recognized share %.4f is below the baseline %.4f (ADR 0012)", rate, baseline)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func loadOTTLCorpus(t *testing.T) []ottlCorpusEntry {
	t.Helper()
	files, err := filepath.Glob("testdata/ottl-corpus/*.jsonl")
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus files: %v", err)
	}
	var out []ottlCorpusEntry
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var e ottlCorpusEntry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			out = append(out, e)
		}
		fh.Close()
	}
	// The lab's Collector configs, as captured in the benchmark states.
	labs, _ := filepath.Glob("../../testdata/bench/*/checkout/otelcol/*.yaml")
	for _, f := range labs {
		out = append(out, collectorOTTL(t, f)...)
	}
	if len(labs) == 0 {
		t.Error("no lab Collector configs found under testdata/bench/*/checkout/otelcol")
	}
	return out
}

// collectorOTTL pulls the OTTL out of a Collector config: transform
// statements, filter conditions, routing conditions and statements, tail
// sampling OTTL policies and connectors' conditions.
func collectorOTTL(t *testing.T, path string) []ottlCorpusEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	src := "lab " + filepath.ToSlash(path[strings.Index(path, "testdata/"):])
	var out []ottlCorpusEntry
	add := func(kind, text string) {
		out = append(out, ottlCorpusEntry{Source: src, Kind: kind, Text: text})
	}
	var walk func(n *yaml.Node, key string)
	walk = func(n *yaml.Node, key string) {
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, key)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i].Value, n.Content[i+1]
				if (k == "condition" || k == "statement") && v.Kind == yaml.ScalarNode {
					add(map[string]string{"condition": "condition", "statement": "statement"}[k], v.Value)
					continue
				}
				walk(v, k)
			}
		case yaml.SequenceNode:
			kind := ""
			switch key {
			case "statements", "trace_statements", "metric_statements", "log_statements":
				kind = "statement"
			case "conditions", "metric", "datapoint", "span", "spanevent", "log", "logrecord", "resource", "scope":
				kind = "condition"
			}
			for _, c := range n.Content {
				if c.Kind == yaml.ScalarNode && kind != "" {
					add(kind, c.Value)
					continue
				}
				walk(c, key)
			}
		}
	}
	walk(&doc, "")
	return out
}

// FuzzOTTL: neither parser panics, and a statement that parses names its
// editor first among its functions.
func FuzzOTTL(f *testing.F) {
	for _, s := range []string{
		`set(log.attributes["a"], resource.attributes["b"]) where log.body != nil`,
		`delete_matching_keys(datapoint.attributes, "^session[^A-Za-z0-9:]+id$") where metric.name == "x" or (metric.is_monotonic == true and IsMatch(metric.name, "^x$"))`,
		`merge_maps(span.attributes, MapEach(Filter(span.attributes, (k, v) => IsMatch(k, "a")), (_, v) => "R"), "upsert")`,
		`set(attributes[Concat(["a", b], ".")], Split(body, ",")[1] + -1.5e3 * (2 - 0x0F))`,
		`not (IsMatch(span.name, "http_.*") and span.kind > SPAN_KIND_SERVER)`,
		`{"a": {"b": [1, nil, true]}}`, `(`, `(a) =>`, `x[`, `"`, `set(`, `a.`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if o, err := OTTLStatement(s); err == nil && (len(o.Funcs) == 0 || o.Funcs[0] != o.Editor) {
			t.Fatalf("%q: editor %q, funcs %q", s, o.Editor, o.Funcs)
		}
		_, _ = OTTLCondition(s)
	})
}

type mapEnv map[string]any

func (m mapEnv) Path(p string) (any, bool) { v, ok := m[p]; return v, ok }

func TestOTTLEvalWhere(t *testing.T) {
	record := mapEnv{`span.attributes["gen_ai.operation.name"]`: "execute_tool"}
	for _, c := range []struct {
		cond string
		env  mapEnv
		want Tri
	}{
		{`span.attributes["success"] == true`, record, Unknown}, // success unknown: can be true
		{`span.attributes["gen_ai.operation.name"] == "chat"`, record, False},
		{`span.attributes["gen_ai.operation.name"] == "execute_tool" and span.attributes["x"] == 1`, record, Unknown},
		{`span.attributes["gen_ai.operation.name"] == "chat" and span.attributes["x"] == 1`, record, False},
		{`not IsMatch(span.attributes["gen_ai.operation.name"], "^exec")`, record, False},
		{`span.attributes["gen_ai.operation.name"] != nil or IsRootSpan()`, record, True},
		{`metric.name == "coding_agent_cost_usage_USD_total" or (metric.is_monotonic == true and IsMatch(metric.name, "^coding[^A-Za-z0-9:]+agent[^A-Za-z0-9:]+cost"))`,
			mapEnv{"metric.name": "coding_agent_token_usage_total"}, False}, // is_monotonic unknown, but IsMatch false: and is False
		{`span.attributes["http.response.status_code"] == "500"`, mapEnv{`span.attributes["http.response.status_code"]`: int64(500)}, False},
		{`Len(span.attributes["p"]) > 3`, mapEnv{`span.attributes["p"]`: "abcd"}, True},
	} {
		o, err := OTTLCondition(c.cond)
		if err != nil {
			t.Fatal(err)
		}
		if got := o.EvalWhere(c.env); got != c.want {
			t.Errorf("%s: %v, want %v", c.cond, got, c.want)
		}
	}
}
