package check

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"go.yaml.in/yaml/v3"
)

// Outcome fields of a record span or log event (ADR 0011): never deleted.
// gen_ai.operation.name is what makes it a record.
var outcomeFields = []string{"gen_ai.tool.name", "gen_ai.tool.call.id", "success", "error.type", "gen_ai.operation.name"}

type otelPipeline struct {
	name, signal string
	receivers    []string
	processors   []string
	exporters    []string
	line         int
}

type otelConfig struct {
	processors map[string]*yaml.Node
	connectors map[string]*yaml.Node
	pipelines  []otelPipeline
}

func readOtel(src []byte) (*otelConfig, error) {
	root, err := yamlRoot(src)
	if err != nil {
		return nil, err
	}
	c := &otelConfig{processors: map[string]*yaml.Node{}, connectors: map[string]*yaml.Node{}}
	for key, dst := range map[string]map[string]*yaml.Node{"processors": c.processors, "connectors": c.connectors} {
		if n := field(root, key); n != nil && n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				dst[n.Content[i].Value] = n.Content[i+1]
			}
		}
	}
	if ps := field(field(root, "service"), "pipelines"); ps != nil && ps.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(ps.Content); i += 2 {
			name, n := ps.Content[i].Value, ps.Content[i+1]
			sig, _, _ := strings.Cut(name, "/")
			c.pipelines = append(c.pipelines, otelPipeline{name: name, signal: sig, receivers: strs(field(n, "receivers")),
				processors: strs(field(n, "processors")), exporters: strs(field(n, "exporters")), line: ps.Content[i].Line})
		}
	}
	return c, nil
}

func typeOf(id string) string { t, _, _ := strings.Cut(id, "/"); return t }

// ottlEnv is what tvt knows about one item: some paths' values. Paths are
// compared without a span., log. or datapoint. context prefix, so a match
// written span.attributes["x"] answers a statement's attributes["x"].
type ottlEnv map[string]any

func normPath(p string) string {
	for _, pre := range []string{"span.", "log.", "datapoint.", "spanevent."} {
		if strings.HasPrefix(p, pre) {
			return p[len(pre):]
		}
	}
	return p
}

func (e ottlEnv) Path(p string) (any, bool) {
	v, ok := e[normPath(p)]
	return v, ok
}

func envOf(eqs map[string]any) ottlEnv {
	e := ottlEnv{}
	for k, v := range eqs {
		e[normPath(k)] = v
	}
	return e
}

// canBeTrue: a where clause (and its group conditions) may hold for an
// item. Unknown counts: check fails closed.
func canBeTrue(o parse.OTTL, groups []parse.OTTL, env parse.OTTLEnv) parse.Tri {
	t := o.EvalWhere(env)
	for _, g := range groups {
		t = andTri(t, g.EvalWhere(env))
	}
	return t
}

func andTri(a, b parse.Tri) parse.Tri {
	switch {
	case a == parse.False || b == parse.False:
		return parse.False
	case a == parse.True && b == parse.True:
		return parse.True
	}
	return parse.Unknown
}

func orTri(a, b parse.Tri) parse.Tri {
	switch {
	case a == parse.True || b == parse.True:
		return parse.True
	case a == parse.False && b == parse.False:
		return parse.False
	}
	return parse.Unknown
}

// stmt is one OTTL statement or condition in the config, with where it
// came from.
type stmt struct {
	o      parse.OTTL
	groups []parse.OTTL // a statement group's own conditions
	text   string
	line   int
	err    error
}

// otelStatements reads a transform processor's statements of one signal
// (trace_statements, metric_statements, log_statements), in both forms:
// plain strings, and groups with context, statements and conditions.
func otelStatements(n *yaml.Node, condition bool) []stmt {
	if n == nil {
		return nil
	}
	parseOne := func(text string, line int, groups []parse.OTTL) stmt {
		var o parse.OTTL
		var err error
		if condition {
			o, err = parse.OTTLCondition(text)
		} else {
			o, err = parse.OTTLStatement(text)
		}
		return stmt{o: o, groups: groups, text: text, line: line, err: err}
	}
	var out []stmt
	for _, it := range n.Content {
		switch it.Kind {
		case yaml.ScalarNode:
			out = append(out, parseOne(it.Value, it.Line, nil))
		case yaml.MappingNode:
			var groups []parse.OTTL
			for _, c := range strs(field(it, "conditions")) {
				if g, err := parse.OTTLCondition(c); err == nil {
					groups = append(groups, g)
				} else {
					out = append(out, stmt{text: c, line: it.Line, err: err})
				}
			}
			key := "statements"
			if condition {
				key = "conditions"
				groups = nil
			}
			if l := field(it, key); l != nil {
				for _, s := range l.Content {
					out = append(out, parseOne(s.Value, s.Line, groups))
				}
			}
		}
	}
	return out
}

// filterConditions are a filter processor's conditions for a signal:
// traces span/spanevent, metrics metric/datapoint, logs log_record, and
// the context-inferred trace_conditions, metric_conditions,
// log_conditions. legacy says the filter uses include/exclude, which tvt
// doesn't evaluate.
func filterConditions(cfg *yaml.Node, signal string) (conds []stmt, legacy bool) {
	keys := map[string][]string{
		"traces":  {"span", "spanevent"},
		"metrics": {"metric", "datapoint"},
		"logs":    {"log_record"},
	}[signal]
	group := map[string]string{"traces": "traces", "metrics": "metrics", "logs": "logs"}[signal]
	g := field(cfg, group)
	for _, k := range keys {
		conds = append(conds, otelStatements(field(g, k), true)...)
	}
	flat := map[string]string{"traces": "trace_conditions", "metrics": "metric_conditions", "logs": "log_conditions"}[signal]
	conds = append(conds, otelStatements(field(cfg, flat), true)...)
	if field(g, "include") != nil || field(g, "exclude") != nil || field(cfg, "spans") != nil && signal == "traces" ||
		field(cfg, "logs") != nil && (field(field(cfg, "logs"), "include") != nil || field(field(cfg, "logs"), "exclude") != nil) {
		legacy = true
	}
	return conds, legacy
}

// otelcolEffects judges a Collector config.
func otelcolEffects(p report.PipelineDef, path string, src []byte, ix *index, files Files) ([]effect, error) {
	c, err := readOtel(src)
	if err != nil {
		return nil, err
	}
	blocks := tvtBlocks(src)
	var out []effect
	add := func(e effect) {
		e.pipeline, e.file = p.Name, path
		if e.block == "" && e.at != "" {
			var line int
			fmt.Sscanf(e.at, "line %d", &line)
			e.block = blockAt(blocks, line)
		}
		out = append(out, e)
	}
	for _, e := range ix.matchErrs {
		add(effect{rule: RuleFailClosed, reason: e})
	}
	produced := connectorOutputs(c)
	protectedNames := map[string]report.ProtectedSignal{}
	for _, s := range ix.byPipeline[p.Name] {
		if s.Kind == "metric" {
			protectedNames[s.Name] = s
		}
	}
	// readers: what each pipeline's later steps and its connector
	// exporters read (S8 items 9 and 10), by pipeline.
	readers := pipelineReaders(c, produced, protectedNames)
	for _, pl := range c.pipelines {
		set := map[string]string{}     // attribute path -> processor that sets it earlier
		deleted := map[string]string{} // attribute path -> processor that deleted it earlier
		setLater := laterSets(c, pl)
		var unmodeled []string // earlier steps tvt can't model
		for i, id := range pl.processors {
			cfg := c.processors[id]
			typ := typeOf(id)
			at := fmt.Sprintf("line %d", lineOf(cfg))
			comp := "processor " + id + " in " + pl.name
			if cfg == nil && typ != "batch" && typ != "memory_limiter" {
				continue
			}
			var conds, stmts []stmt
			drops := false
			switch typ {
			case "filter":
				var legacy bool
				conds, legacy = filterConditions(cfg, pl.signal)
				if legacy {
					add(effect{rule: RuleFailClosed, component: comp, reason: "include/exclude matching (not OTTL): check can't tell what it drops", at: at})
				}
				drops = len(conds) > 0 || legacy
			case "transform":
				key := map[string]string{"traces": "trace_statements", "metrics": "metric_statements", "logs": "log_statements"}[pl.signal]
				stmts = otelStatements(field(cfg, key), false)
			case "probabilistic_sampler":
				drops = true
				add(effect{rule: ruleDropMarker, component: comp, reason: "samples " + pl.signal, at: at})
				if pl.signal == "traces" && len(ix.spans) > 0 {
					add(effect{rule: RuleRecord, component: comp, reason: "probabilistic_sampler on a traces pipeline drops record spans (" + recordSpanText(ix) + ")", at: at})
				}
				if pl.signal == "logs" && len(ix.logEvents) > 0 {
					add(effect{rule: RuleRecord, component: comp, reason: "probabilistic_sampler on a logs pipeline drops record log events", at: at})
				}
			case "tail_sampling":
				drops = true
				add(effect{rule: ruleDropMarker, component: comp, reason: "tail sampling", at: at})
				if pl.signal == "traces" && len(ix.spans) > 0 && !keepsRecords(cfg, ix) {
					add(effect{rule: RuleRecord, component: comp, reason: "tail_sampling has no policy that always keeps record spans (" + recordSpanText(ix) + ")", at: at})
				}
			case "attributes":
				for _, a := range attributeActions(cfg) {
					if a.action == "delete" || a.action == "hash" {
						deletedAttr(add, ix, pl, comp, a.key, a.line, readers[pl.name], i, id)
						deleted["attributes[\""+a.key+"\"]"] = id
					} else {
						set["attributes[\""+a.key+"\"]"] = id
					}
				}
			case "logdedup":
				// A collapse changes every count over the lines (S8 item 6).
				// Records are never collapsed; read streams need their count
				// queries rewritten to sum the count attribute, which check
				// can't see offline, so it refuses until an owner confirms.
				drops = true
				add(effect{rule: ruleDropMarker, component: comp, reason: "collapses repeated log lines", at: at})
				readStreams := len(ix.of(p.Name, "stream")) > 0
				for _, s := range ix.read[p.Name] {
					if s.Kind == "stream" {
						readStreams = true
					}
				}
				if len(ix.logEvents) > 0 && pl.signal == "logs" {
					add(effect{rule: RuleRecord, component: comp, reason: "logdedup collapses log lines, and records (agent tool-call events) are never collapsed (ADR 0011)", at: at})
				}
				if readStreams {
					add(effect{rule: RuleCollapse, component: comp, reason: "collapses repeated lines of streams something reads: every count_over_time, rate or bytes_over_time over them changes unless rewritten to sum the count attribute (S8 item 6)", at: at})
				}
			}
			// Steps tvt can't model run before anything after them.
			for _, s := range append(append([]stmt{}, conds...), stmts...) {
				if s.err != nil {
					add(effect{rule: RuleFailClosed, component: comp, reason: fmt.Sprintf("an OTTL statement check can't parse: %q: %v", shorten(s.text, 80), s.err), at: fmt.Sprintf("line %d", s.line)})
					continue
				}
				if !s.o.Modeled() {
					why := strings.Join(s.o.Unmodeled, ", ")
					if s.o.Experimental {
						why = strings.TrimPrefix(why+", an experimental function", ", ")
					}
					if s.o.Lambda {
						why = strings.TrimPrefix(why+", a lambda", ", ")
					}
					unmodeled = append(unmodeled, fmt.Sprintf("%s (%s: %s)", id, why, shorten(s.text, 60)))
				}
			}
			if drops && len(unmodeled) > 0 && len(conds) > 0 {
				add(effect{rule: RuleFailClosed, component: comp,
					reason: fmt.Sprintf("this step drops %s after steps check can't model: %s", pl.signal, strings.Join(unmodeled, "; ")), at: at})
			}
			// Filter conditions: drop marker, protected signals, records,
			// attributes read before they're set (S8 item 7).
			if typ == "filter" {
				for _, s := range conds {
					if s.err != nil {
						continue
					}
					sat := fmt.Sprintf("line %d", s.line)
					add(effect{rule: ruleDropMarker, component: comp, reason: "drops " + pl.signal + " where " + shorten(s.text, 100), at: sat})
					if pl.signal == "logs" && !textGuard(s.text) {
						add(effect{rule: RuleErrorGuard, component: comp, reason: fmt.Sprintf("the log filter %q has no `and not IsMatch(log.body, %q)` keeping the error pattern (S8 item 2)", shorten(s.text, 80), "(?i)"+guardText), at: sat})
					}
					for _, r := range s.o.WhereReads {
						key := normPath(r)
						if by, ok := setLater[key]; ok && set[key] == "" {
							add(effect{rule: RuleAttrUnset, component: comp, signal: r,
								reason: fmt.Sprintf("the condition %q reads %s, which %s sets later in %s: it isn't there yet, so the condition doesn't do what it says", shorten(s.text, 80), r, by, pl.name), at: sat})
						}
						if by, ok := deleted[key]; ok {
							add(effect{rule: RuleAttrUnset, component: comp, signal: r,
								reason: fmt.Sprintf("the condition %q reads %s, which %s deleted earlier in %s", shorten(s.text, 80), r, by, pl.name), at: sat})
						}
					}
					filterEffects(add, ix, p, pl, comp, s, sat, readers[pl.name], i)
				}
			}
			for _, s := range stmts {
				if s.err != nil {
					continue
				}
				sat := fmt.Sprintf("line %d", s.line)
				transformEffects(add, ix, p, pl, comp, s, sat, readers[pl.name], i)
				if s.o.Target != "" {
					switch s.o.Editor {
					case "set":
						set[normPath(s.o.Target)] = id
					case "delete_key":
						if k, ok := s.o.ArgString(1); ok {
							deleted[normPath(s.o.Target)+"[\""+k+"\"]"] = id
						}
					}
				}
			}
		}
	}
	out = append(out, buildEffects(p, path, c, files)...)
	out = append(out, replicaEffects(p, path, c)...)
	return out, nil
}

// replicaEffects: a metrics pipeline that deletes datapoint attributes
// and then sums with delta_to_cumulative (the delta-counter label
// removal) is wrong behind several replicas with no load balancing: each
// replica sums only the sessions it sees (S8 item 12).
func replicaEffects(p report.PipelineDef, path string, c *otelConfig) []effect {
	if p.Replicas <= 1 || p.LoadBalanced {
		return nil
	}
	var out []effect
	for _, pl := range c.pipelines {
		if pl.signal != "metrics" {
			continue
		}
		deletes := ""
		for _, id := range pl.processors {
			switch typeOf(id) {
			case "transform":
				for _, s := range otelStatements(field(c.processors[id], "metric_statements"), false) {
					if s.err == nil && deletedKeys(s.o) != nil && normPath(s.o.Target) == "attributes" {
						deletes = id
					}
				}
			case "delta_to_cumulative":
				if deletes != "" {
					out = append(out, effect{rule: RuleDeltaReplicas, pipeline: p.Name, file: path, component: "processor " + deletes + " in " + pl.name,
						reason: fmt.Sprintf("removes labels from delta counters before delta_to_cumulative, but %d Collector replicas run this config with no load-balancing exporter in front: each sums only the sessions it sees", p.Replicas),
						at:     fmt.Sprintf("line %d", lineOf(c.processors[deletes]))})
					deletes = ""
				}
			}
		}
	}
	return out
}

func lineOf(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	return n.Line
}

// laterSets: for each processor position, the attributes a processor
// after it sets. Returned as the union over the pipeline, keyed by path,
// with the first processor that sets it; callers check it's not set yet.
func laterSets(c *otelConfig, pl otelPipeline) map[string]string {
	out := map[string]string{}
	for _, id := range pl.processors {
		cfg := c.processors[id]
		switch typeOf(id) {
		case "attributes":
			for _, a := range attributeActions(cfg) {
				if a.action == "insert" || a.action == "upsert" || a.action == "update" {
					k := "attributes[\"" + a.key + "\"]"
					if _, ok := out[k]; !ok {
						out[k] = id
					}
				}
			}
		case "transform":
			key := map[string]string{"traces": "trace_statements", "metrics": "metric_statements", "logs": "log_statements"}[pl.signal]
			for _, s := range otelStatements(field(cfg, key), false) {
				if s.err == nil && s.o.Editor == "set" && s.o.Target != "" {
					k := normPath(s.o.Target)
					if _, ok := out[k]; !ok {
						out[k] = id
					}
				}
			}
		}
	}
	return out
}

type attrAction struct {
	key, action string
	line        int
}

func attributeActions(cfg *yaml.Node) []attrAction {
	var out []attrAction
	if a := field(cfg, "actions"); a != nil {
		for _, it := range a.Content {
			out = append(out, attrAction{key: scalar(it, "key"), action: scalar(it, "action"), line: it.Line})
		}
	}
	return out
}

// keepsRecords: a tail_sampling policy always keeps record spans: an
// ottl_condition policy whose span condition holds for them, a
// string_attribute policy on gen_ai.operation.name with execute_tool, or
// always_sample.
func keepsRecords(cfg *yaml.Node, ix *index) bool {
	pols := field(cfg, "policies")
	if pols == nil {
		return false
	}
	for _, pol := range pols.Content {
		switch scalar(pol, "type") {
		case "always_sample":
			return true
		case "string_attribute":
			sa := field(pol, "string_attribute")
			for _, m := range ix.spans {
				for k, v := range m.Equalities() {
					if normPath(k) == "attributes[\""+scalar(sa, "key")+"\"]" {
						for _, val := range strs(field(sa, "values")) {
							if val == v {
								return true
							}
						}
					}
				}
			}
		case "ottl_condition":
			oc := field(pol, "ottl_condition")
			for _, c := range strs(field(oc, "span")) {
				o, err := parse.OTTLCondition(c)
				if err != nil {
					continue
				}
				all := len(ix.spans) > 0
				for _, m := range ix.spans {
					if o.EvalWhere(envOf(m.Equalities())) != parse.True {
						all = false
					}
				}
				if all {
					return true
				}
			}
		}
	}
	return false
}

// metricEnvs are what tvt knows about a protected metric inside the
// Collector: its name, in every spelling the condition might use. OTel
// names (coding_agent.token.usage) become Prometheus names
// (coding_agent_token_usage_total) on export, so a string literal in the
// condition that normalizes to the stored name is one more spelling.
var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9:]+`)

func metricEnvs(prom string, literals []string) []ottlEnv {
	cands := map[string]bool{prom: true, strings.TrimSuffix(prom, "_total"): true}
	base := strings.TrimSuffix(prom, "_total")
	for _, l := range literals {
		n := nonAlnum.ReplaceAllString(l, "_")
		if n == prom || n == base || strings.HasPrefix(prom, n+"_") {
			cands[l] = true
		}
	}
	var out []ottlEnv
	for n := range cands {
		out = append(out, ottlEnv{"metric.name": n, "name": n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out
}

func anyEnv(o parse.OTTL, groups []parse.OTTL, envs []ottlEnv) parse.Tri {
	t := parse.False
	for _, e := range envs {
		t = orTri(t, canBeTrue(o, groups, e))
	}
	return t
}

func recordEnvs(ms []parse.OTTL) []ottlEnv {
	var out []ottlEnv
	for _, m := range ms {
		out = append(out, envOf(m.Equalities()))
	}
	return out
}

// filterEffects judges one filter condition: which protected metrics it
// can drop (metrics pipelines), whether it can drop a record span or log
// event, and whether it empties what a connector downstream reads.
func filterEffects(add func(effect), ix *index, p report.PipelineDef, pl otelPipeline, comp string, s stmt, at string, rd []reader, pos int) {
	switch pl.signal {
	case "metrics":
		for _, sig := range ix.of(p.Name, "metric") {
			if t := anyEnv(s.o, s.groups, metricEnvs(sig.Name, s.o.Strings())); t != parse.False {
				add(effect{rule: ruleFor(sig), component: comp, signal: sig.ID,
					reason: fmt.Sprintf("%s %s, %s (condition %q)", mayWord(t, "drops"), sig.Name, why(sig), shorten(s.text, 80)), at: at})
			}
		}
	case "traces":
		if t := anyEnv(s.o, s.groups, recordEnvs(ix.spans)); t != parse.False && len(ix.spans) > 0 {
			add(effect{rule: RuleRecord, component: comp,
				reason: fmt.Sprintf("the condition %q %s true for record spans (%s), so the filter drops them", shorten(s.text, 80), canWord(t), recordSpanText(ix)), at: at})
		}
	case "logs":
		if t := anyEnv(s.o, s.groups, recordEnvs(ix.logEvents)); t != parse.False && len(ix.logEvents) > 0 {
			add(effect{rule: RuleRecord, component: comp,
				reason: fmt.Sprintf("the condition %q %s true for record log events, so the filter drops them", shorten(s.text, 80), canWord(t)), at: at})
		}
	}
	for _, r := range rd {
		if r.pos <= pos || !r.counts {
			continue
		}
		if t := anyEnv(s.o, s.groups, []ottlEnv{r.env}); t != parse.False {
			add(effect{rule: RulePipelineRead, component: comp, signal: r.produces,
				reason: fmt.Sprintf("the condition %q %s true for what %s counts, which makes %s (%s): the filter empties it", shorten(s.text, 80), canWord(t), r.name, r.produces, r.why), at: at})
		}
	}
}

func mayWord(t parse.Tri, verb string) string {
	if t == parse.True {
		return verb
	}
	return "may " + strings.TrimSuffix(verb, "s")
}

func canWord(t parse.Tri) string {
	if t == parse.True {
		return "is"
	}
	return "can be"
}

// transformEffects judges one transform statement: deletes of protected
// labels (metrics), of record outcome fields (traces, logs), and of
// attributes a later reader reads.
func transformEffects(add func(effect), ix *index, p report.PipelineDef, pl otelPipeline, comp string, s stmt, at string, rd []reader, pos int) {
	keys := deletedKeys(s.o)
	if keys == nil {
		return
	}
	target := normPath(s.o.Target)
	switch pl.signal {
	case "metrics":
		if target != "attributes" {
			return
		}
		for _, sig := range ix.of(p.Name, "label") {
			if !keys.matches(storedLabelVariants(sig.Name)) {
				continue
			}
			if t := anyEnv(s.o, s.groups, metricEnvs(sig.Parent, s.o.Strings())); t != parse.False {
				add(effect{rule: ruleFor(sig), component: comp, signal: sig.ID,
					reason: fmt.Sprintf("%s %s, %s (%q)", mayWord(t, "removes"), describe(sig), why(sig), shorten(s.text, 80)), at: at})
			}
		}
	case "traces", "logs":
		if target != "attributes" {
			break
		}
		ms := ix.spans
		what := "record spans"
		if pl.signal == "logs" {
			ms, what = ix.logEvents, "record log events"
		}
		if len(ms) == 0 {
			break
		}
		for _, f := range outcomeFields {
			if !keys.matches([]string{f}) {
				continue
			}
			if t := anyEnv(s.o, s.groups, recordEnvs(ms)); t != parse.False {
				add(effect{rule: RuleRecord, component: comp, signal: f,
					reason: fmt.Sprintf("deletes the outcome field %s from %s (%q)", f, what, shorten(s.text, 80)), at: at})
			}
		}
	}
	for _, r := range rd {
		if r.pos <= pos {
			continue
		}
		for _, a := range r.attrs {
			if normPath(s.o.Target) == attrMap(a) && keys.matches([]string{attrKey(a)}) {
				add(effect{rule: RulePipelineRead, component: comp, signal: a,
					reason: fmt.Sprintf("deletes %s, which %s reads (%s)", a, r.name, r.why), at: at})
			}
		}
	}
}

// keySet is what a statement deletes from a map: exact keys, a pattern,
// or every key but some.
type keySet struct {
	exact   []string
	pattern *regexp.Regexp
	keep    []string
	all     bool
}

func (k *keySet) matches(names []string) bool {
	for _, n := range names {
		switch {
		case k.all:
			return true
		case k.pattern != nil && k.pattern.MatchString(n):
			return true
		case k.keep != nil:
			found := false
			for _, x := range k.keep {
				if x == n {
					found = true
				}
			}
			if !found {
				return true
			}
		}
		for _, x := range k.exact {
			if x == n {
				return true
			}
		}
	}
	return false
}

// deletedKeys: what an editor removes from its target map, or nil when it
// removes nothing. A cap (truncate_all, Substring) isn't a delete.
func deletedKeys(o parse.OTTL) *keySet {
	switch o.Editor {
	case "delete_key":
		if k, ok := o.ArgString(1); ok {
			return &keySet{exact: []string{k}}
		}
		return &keySet{all: true}
	case "delete_matching_keys":
		if pat, ok := o.ArgString(1); ok {
			if re, err := regexp.Compile(pat); err == nil {
				return &keySet{pattern: re}
			}
		}
		return &keySet{all: true}
	case "keep_keys":
		if ks, ok := o.ArgStrings(1); ok {
			return &keySet{keep: ks}
		}
		return &keySet{all: true}
	case "keep_matching_keys", "limit":
		return &keySet{all: true}
	case "set":
		// set(attributes["k"], nil) deletes the value.
		if strings.HasSuffix(o.Target, "]") && len(o.Args) > 0 && strings.TrimSpace(o.Args[0].Text) == "nil" {
			return &keySet{exact: []string{attrKey(o.Target)}}
		}
	}
	return nil
}

func attrKey(path string) string {
	i := strings.Index(path, "[\"")
	if i < 0 {
		return ""
	}
	return strings.TrimSuffix(path[i+2:], "\"]")
}

func attrMap(path string) string {
	p := normPath(path)
	if i := strings.Index(p, "["); i >= 0 {
		return p[:i]
	}
	return p
}

// storedLabelVariants: OTel attribute names that Prometheus stores as the
// label (session.id and session_id both become session_id).
func storedLabelVariants(label string) []string {
	return []string{label, strings.ReplaceAll(label, "_", ".")}
}

// deletedAttr judges an attributes processor delete or hash.
func deletedAttr(add func(effect), ix *index, pl otelPipeline, comp, key string, line int, rd []reader, pos int, id string) {
	at := fmt.Sprintf("line %d", line)
	switch pl.signal {
	case "traces", "logs":
		for _, f := range outcomeFields {
			if f == key {
				add(effect{rule: RuleRecord, component: comp, signal: f,
					reason: fmt.Sprintf("deletes the outcome field %s of record %s", f, map[string]string{"traces": "spans", "logs": "log events"}[pl.signal]), at: at})
			}
		}
	}
	for _, r := range rd {
		if r.pos <= pos {
			continue
		}
		for _, a := range r.attrs {
			if attrKey(a) == key {
				add(effect{rule: RulePipelineRead, component: comp, signal: a, reason: fmt.Sprintf("deletes %s, which %s reads (%s)", a, r.name, r.why), at: at})
			}
		}
	}
}

// reader is a step that reads attributes: a routing table, a tail
// sampling policy, a later filter, or a connector's conditions and
// dimensions. pos is its place in the pipeline (connectors after every
// processor); counts marks a counting connector whose output something
// protected reads.
type reader struct {
	name     string
	pos      int
	attrs    []string
	why      string
	counts   bool
	env      ottlEnv
	produces string
}

// connectorOutputs: metric names (as Prometheus stores them) each
// count, sum or signal_to_metrics connector makes.
func connectorOutputs(c *otelConfig) map[string][]string {
	out := map[string][]string{}
	for id, cfg := range c.connectors {
		switch typeOf(id) {
		case "count", "sum", "signaltometrics", "signal_to_metrics":
			for _, sig := range []string{"spans", "spanevents", "metrics", "datapoints", "logs"} {
				n := field(cfg, sig)
				if n == nil || n.Kind != yaml.MappingNode {
					continue
				}
				for i := 0; i+1 < len(n.Content); i += 2 {
					name := nonAlnum.ReplaceAllString(n.Content[i].Value, "_")
					if typeOf(id) == "count" || typeOf(id) == "sum" {
						name += "_total"
					}
					out[id] = append(out[id], name)
				}
			}
		}
	}
	return out
}

// pipelineReaders: for each pipeline, the readers downstream of its
// processors: routing connectors it exports to, counting connectors whose
// output is protected, and its own filter and tail sampling conditions.
func pipelineReaders(c *otelConfig, produced map[string][]string, protected map[string]report.ProtectedSignal) map[string][]reader {
	out := map[string][]reader{}
	for _, pl := range c.pipelines {
		end := len(pl.processors)
		for _, x := range pl.exporters {
			cfg, ok := c.connectors[x]
			if !ok {
				continue
			}
			switch typeOf(x) {
			case "routing":
				var attrs []string
				if t := field(cfg, "table"); t != nil {
					for _, row := range t.Content {
						for _, k := range []string{"condition", "statement"} {
							if v := scalar(row, k); v != "" {
								if o, err := parse.OTTLCondition(v); err == nil {
									attrs = append(attrs, o.Reads...)
								} else if o, err := parse.OTTLStatement(v); err == nil {
									attrs = append(attrs, o.Reads...)
								}
							}
						}
					}
				}
				out[pl.name] = append(out[pl.name], reader{name: "connector " + x, pos: end, attrs: attrs, why: "its routing table reads it"})
			case "count", "sum", "signaltometrics", "signal_to_metrics":
				for _, m := range produced[x] {
					sig, ok := protected[m]
					if !ok {
						continue
					}
					r := reader{name: "connector " + x, pos: end, counts: true, produces: m, why: "read by " + why(sig), env: ottlEnv{}}
					for _, sigKey := range []string{"spans", "spanevents", "metrics", "datapoints", "logs"} {
						n := field(cfg, sigKey)
						if n == nil || n.Kind != yaml.MappingNode {
							continue
						}
						for i := 0; i+1 < len(n.Content); i += 2 {
							def := n.Content[i+1]
							for _, cs := range strs(field(def, "conditions")) {
								if o, err := parse.OTTLCondition(cs); err == nil {
									r.attrs = append(r.attrs, o.Reads...)
									for k, v := range o.Equalities() {
										r.env[normPath(k)] = v
									}
								}
							}
							if as := field(def, "attributes"); as != nil {
								for _, a := range as.Content {
									r.attrs = append(r.attrs, "attributes[\""+scalar(a, "key")+"\"]")
								}
							}
						}
					}
					out[pl.name] = append(out[pl.name], r)
				}
			}
		}
	}
	return out
}

// buildEffects: with a builder manifest (pipelines[].build), every
// processor and connector the config uses must be in it.
func buildEffects(p report.PipelineDef, path string, c *otelConfig, files Files) []effect {
	if p.Build == "" {
		return nil
	}
	src, err := files(p.Build)
	if err != nil {
		return []effect{{rule: RuleBuild, pipeline: p.Name, file: p.Build, reason: "the builder manifest can't be read: " + err.Error()}}
	}
	root, err := yamlRoot(src)
	if err != nil {
		return []effect{{rule: RuleBuild, pipeline: p.Name, file: p.Build, reason: "the builder manifest doesn't parse: " + err.Error()}}
	}
	have := map[string]bool{}
	for _, kind := range []string{"processors", "connectors"} {
		if l := field(root, kind); l != nil {
			for _, it := range l.Content {
				mod := strings.Fields(scalar(it, "gomod"))
				if len(mod) == 0 {
					continue
				}
				parts := strings.Split(mod[0], "/")
				last := parts[len(parts)-1]
				have[kind+":"+strings.TrimSuffix(strings.TrimSuffix(last, "processor"), "connector")] = true
			}
		}
	}
	var out []effect
	used := map[string]bool{}
	for _, pl := range c.pipelines {
		for _, id := range pl.processors {
			used["processors:"+typeOf(id)] = true
		}
		for _, x := range pl.exporters {
			if _, ok := c.connectors[x]; ok {
				used["connectors:"+typeOf(x)] = true
			}
		}
	}
	keys := make([]string, 0, len(used))
	for k := range used {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !have[k] {
			kind, name, _ := strings.Cut(k, ":")
			out = append(out, effect{rule: RuleBuild, pipeline: p.Name, file: path, component: name,
				reason: fmt.Sprintf("the %s %s isn't in the Collector build (%s)", strings.TrimSuffix(kind, "s"), name, p.Build)})
		}
	}
	return out
}
