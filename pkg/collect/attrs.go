package collect

import "regexp"

// Attribute sizes (S10 item 7, Jason's decision Nov 21): one more LogQL
// query, an exception to invariant 4 like the per-level bytes. For the
// largest streams, tvt reads a sample of recent lines (a plain stream
// selector, no pipeline, at most AttrLineLimit lines) tagged AttrTag, which
// every query log reader drops from usage, and keeps only each key's name,
// how many lines carry it, its value bytes and a count of distinct values
// by hash. The lines and values are never stored or printed.
const AttrTag = "Source=tvt-attrs"

// AttrCap is the cap tvt proposes for a large key nothing reads (S10 item
// 7): its value cut to this many bytes, plus a marker.
const AttrCap = 512

// AttrLineLimit caps the lines one attribute sample reads.
const AttrLineLimit = 1000

var lokiAttrRe = regexp.MustCompile(`^\{[^{}|]*\}$`)

// LokiAttrQuery is the attribute-sample query for one stream: the plain
// selector.
func LokiAttrQuery(selector string) string { return selector }

// IsLokiAttrQuery: q is an attribute-sample query (a plain selector).
func IsLokiAttrQuery(q string) bool { return lokiAttrRe.MatchString(q) }

// StreamAttrs is one stream's sampled key sizes.
type StreamAttrs struct {
	// Lines is how many lines were sampled; LineBytes their bytes.
	Lines     int
	LineBytes int64
	Keys      []AttrStat
}

// AttrStat is one key's size in a stream's sample.
type AttrStat struct {
	// Key is the name as Loki's json or logfmt parser extracts it (nested
	// JSON keys joined with _, other characters than letters, digits and _
	// replaced with _).
	Key string
	// Lines is how many sampled lines carry the key.
	Lines int
	// ValueBytes is the key's value bytes over those lines; MaxBytes the
	// largest one.
	ValueBytes int64
	MaxBytes   int
	// OverCap is the value bytes past AttrCap, summed over the lines: what
	// a cap would remove.
	OverCap int64
	// Distinct is how many different values the sample held, by hash.
	Distinct int
}
