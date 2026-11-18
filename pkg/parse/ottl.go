package parse

import (
	"fmt"
	"strings"
)

// OTTL is what tvt needs from one OTTL statement or condition (ADR 0012):
// which paths it reads, which it changes, and which functions it calls.
// It is a narrow extractor written from the v1 language reference
// (opentelemetry-collector-contrib v0.164.0), not an evaluator, and it is
// permissive: it accepts some text the Collector would reject, since the
// Collector validates its own config. Callers stay fail-closed on what it
// can't tell them: an unknown function (Unmodeled), a lambda (alpha) or an
// experimental function.
type OTTL struct {
	// Editor is the statement's editor (set, delete_key, ...); empty for a
	// condition.
	Editor string
	// Target is the path the editor changes: its first argument when that
	// is a path (set's target, delete_key's map). Empty when the editor
	// takes none (convert_sum_to_gauge()) or its first argument isn't a
	// path.
	Target string
	// Args are the editor's arguments as written, in order.
	Args []OTTLArg
	// Where is the statement's where clause as written; the whole text for
	// a condition.
	Where string
	// Reads are the paths read anywhere outside Target, in order, once
	// each: the editor's other arguments, converters' arguments and the
	// where clause. Map keys are kept (attributes["k"]); a computed key is
	// written [*].
	Reads []string
	// WhereReads are the paths the where clause (or the condition) reads.
	WhereReads []string
	// Funcs are the functions called, the editor first, once each.
	Funcs []string
	// Unmodeled are the functions tvt doesn't know: not in OTTL's standard
	// set, the transform and filter processors' own, or the experimental
	// set.
	Unmodeled []string
	// Experimental: calls a function from the xottl module, outside OTTL's
	// stability guarantee.
	Experimental bool
	// Lambda: uses a lambda expression (alpha, behind a feature gate).
	Lambda bool

	where ottlNode   // the where clause, or the whole condition; nil when none
	args  []ottlNode // the editor's argument values, in order
}

// OTTLArg is one editor argument.
type OTTLArg struct {
	// Name is set for a named argument (name = value).
	Name string
	// Path is set when the argument is a path.
	Path string
	// String is the value of a string literal argument.
	String string
	// Text is the argument as written.
	Text string
}

// Modeled reports whether tvt knows every function the statement calls
// and it uses no lambda or experimental function: only then can a caller
// rely on Target and Reads.
func (o OTTL) Modeled() bool {
	return len(o.Unmodeled) == 0 && !o.Lambda && !o.Experimental
}

// OTTLStatement parses an editor invocation with an optional where
// clause, as in the transform processor's statements.
func OTTLStatement(s string) (OTTL, error) {
	p, err := newOTTLParser(s)
	if err != nil {
		return OTTL{}, err
	}
	call, err := p.value()
	if err != nil {
		return OTTL{}, err
	}
	c, ok := call.(*ottlCall)
	if !ok || !isLowerStart(c.name) || len(c.keys) > 0 {
		return OTTL{}, fmt.Errorf("OTTL statement: want an editor call, got %q", strings.TrimSpace(s))
	}
	var where ottlNode
	whereText := ""
	if p.peekWord("where") {
		start := p.toks[p.i+1].pos
		p.i++
		if where, err = p.boolExpr(); err != nil {
			return OTTL{}, err
		}
		whereText = strings.TrimSpace(s[start:])
	}
	if err := p.end(); err != nil {
		return OTTL{}, err
	}
	o := OTTL{Editor: c.name, Where: whereText, where: where}
	for _, a := range c.args {
		o.args = append(o.args, a.value)
	}
	w := &ottlWalk{}
	w.fn(c.name)
	for i, a := range c.args {
		arg := OTTLArg{Text: strings.TrimSpace(s[a.start:a.end])}
		v := a.value
		if a.name != "" {
			arg.Name = a.name
		}
		if pt, ok := v.(*ottlPath); ok {
			arg.Path = pt.text()
			if i == 0 && a.name == "" {
				o.Target = arg.Path
				w.keys(pt)
				continue
			}
		}
		if l, ok := v.(*ottlLit); ok && l.str {
			arg.String = l.s
		}
		o.Args = append(o.Args, arg)
		w.node(v)
	}
	if where != nil {
		ww := &ottlWalk{}
		ww.node(where)
		o.WhereReads = ww.reads
		w.node(where)
	}
	o.finish(w)
	return o, nil
}

// OTTLCondition parses a boolean expression on its own, as in the filter
// processor's conditions, a routing connector's condition or a tail
// sampling OTTL policy.
func OTTLCondition(s string) (OTTL, error) {
	p, err := newOTTLParser(s)
	if err != nil {
		return OTTL{}, err
	}
	n, err := p.boolExpr()
	if err != nil {
		return OTTL{}, err
	}
	if err := p.end(); err != nil {
		return OTTL{}, err
	}
	w := &ottlWalk{}
	w.node(n)
	o := OTTL{Where: strings.TrimSpace(s), WhereReads: w.reads, where: n}
	o.finish(w)
	return o, nil
}

func (o *OTTL) finish(w *ottlWalk) {
	o.Reads, o.Funcs, o.Lambda = w.reads, w.funcs, w.lambda
	for _, f := range w.funcs {
		switch {
		case ottlExperimental[f]:
			o.Experimental = true
		case !ottlKnown[f]:
			o.Unmodeled = append(o.Unmodeled, f)
		}
	}
}

// The functions tvt knows, from the v0.165.0 READMEs: OTTL's standard
// editors and converters (pkg/ottl/ottlfuncs), the transform and filter
// processors' own, and the experimental set (pkg/ottl/xottl).
var ottlKnown = setOf(
	// Standard editors.
	"append", "clear", "delete_index", "delete_key", "delete_matching_keys", "keep_matching_keys",
	"flatten", "keep_keys", "limit", "merge_maps", "replace_all_matches", "replace_all_patterns",
	"replace_match", "replace_pattern", "set", "stringify_all", "truncate_all",
	// Standard converters.
	"Bool", "Decode", "Coalesce", "CommunityID", "Concat", "ContainsValue", "ConvertCase",
	"ConvertAttributesToElementsXML", "ConvertTextToElementsXML", "Day", "Double", "Duration",
	"ExtractPatterns", "ExtractGrokPatterns", "FNV", "Format", "FormatTime", "GetXML", "HasPrefix",
	"HasSuffix", "Hex", "Hour", "Hours", "Index", "InsertXML", "Int", "IsBool", "IsDouble", "IsEmpty",
	"IsInCIDR", "IsInt", "IsRootSpan", "IsMap", "IsMatch", "IsList", "IsString", "Keys", "Len", "Log",
	"IsValidLuhn", "Microseconds", "Milliseconds", "Minute", "Minutes", "Month", "Nanosecond",
	"Nanoseconds", "Now", "ParseCSV", "ParseInt", "ParseJSON", "ParseKeyValue", "ParseSeverity",
	"ParseSimplifiedXML", "ParseXML", "RemoveXML", "Second", "Seconds", "SliceToMap", "Sort", "SpanID",
	"Split", "Trim", "TrimPrefix", "TrimSuffix", "String", "Substring", "Time", "ToCamelCase",
	"ToKeyValueString", "ToLowerCase", "ToSnakeCase", "ToUpperCase", "TraceID", "TruncateTime", "Unix",
	"UnixMicro", "UnixMilli", "UnixNano", "UnixSeconds", "UserAgent", "URL", "UUID", "Values", "Weekday",
	"Year",
	"Base64Encode", "MD5", "Murmur3Hash", "Murmur3Hash128", "SHA1", "SHA256", "SHA512", "UUIDv7",
	"XXH3", "XXH128",
	// The transform processor's own.
	"convert_sum_to_gauge", "convert_gauge_to_sum", "extract_avg_metric", "extract_count_metric",
	"extract_percentile_metric", "extract_sum_metric", "convert_summary_count_val_to_sum",
	"convert_summary_quantile_val_to_gauge", "convert_summary_sum_val_to_sum", "copy_metric",
	"convert_exponential_histogram_to_histogram", "scale_metric", "aggregate_on_attributes",
	"aggregate_on_attribute_value", "merge_histogram_buckets", "set_semconv_span_name",
	"ParseCEF", "ParseCLF", "ParseELF", "ParseLEEF",
	// The filter processor's own.
	"HasAttrKeyOnDatapoint", "HasAttrOnDatapoint",
)

var ottlExperimental = setOf("All", "Any", "Filter", "Find", "MapEach", "MapKeys", "Reduce", "ToValidUTF8", "When")

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func isLowerStart(s string) bool { return s != "" && s[0] >= 'a' && s[0] <= 'z' }
