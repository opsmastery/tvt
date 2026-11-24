package scan

import (
	"maps"
	"regexp"
	"slices"
	"sort"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/cost"
	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/parse"
)

// AttrStreams is how many of the largest streams a scan samples for key
// sizes (S10 item 7), one tagged query each, and AttrMaxKeys how many keys
// per stream it keeps, largest first.
const (
	AttrStreams = 10
	AttrMaxKeys = 50
)

// streamAttrs turns each sampled stream's key sizes into graph rows: the
// sample's shares scaled to the stream's daily bytes, and how each key is
// read. A reader whose query returns lines (or that tvt can't parse) sees
// every key; a metric query reads the keys it names. Read keys are never
// cap candidates.
func streamAttrs(samples map[string]collect.StreamAttrs, vols []graph.StreamVolume, card *cost.PriceCard, readers streamReaders) []graph.StreamAttr {
	perDay := map[string]int64{}
	for _, v := range vols {
		perDay[v.Selector] = v.BytesPerDay
	}
	var out []graph.StreamAttr
	for _, sel := range slices.Sorted(maps.Keys(samples)) {
		s := samples[sel]
		if s.Lines == 0 || s.LineBytes <= 0 {
			continue
		}
		rs := readers(sel)
		var lineReader string
		for _, c := range rs {
			if c.Unparsed != "" || parse.LogQLReturnsLines(c.Expr) {
				lineReader = c.ID
				break
			}
		}
		keys := s.Keys
		if len(keys) > AttrMaxKeys {
			keys = keys[:AttrMaxKeys]
		}
		day := perDay[sel]
		for _, k := range keys {
			a := graph.StreamAttr{Stream: sel, Key: k.Key, SampledLines: s.Lines, Lines: k.Lines,
				ValueBytes: k.ValueBytes, OverCap: k.OverCap, MaxBytes: k.MaxBytes, Distinct: k.Distinct,
				BytesPerDay:    int64(float64(k.ValueBytes) / float64(s.LineBytes) * float64(day)),
				CapBytesPerDay: int64(float64(k.OverCap) / float64(s.LineBytes) * float64(day)),
			}
			if card != nil && a.CapBytesPerDay > 0 {
				a.CapMonthly = card.LogMonthly(a.CapBytesPerDay)
			}
			switch {
			case lineReader != "":
				a.Read, a.Reader = graph.AttrReadLines, lineReader
			default:
				for _, c := range rs {
					if namesKey(c.Expr, k.Key) {
						a.Read, a.Reader = graph.AttrReadNamed, c.ID
						break
					}
				}
			}
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BytesPerDay > out[j].BytesPerDay })
	return out
}

// namesKey: the query reads the key, by name anywhere in it (a label
// filter, unwrap, a format template, a grouping) or by a parser stage
// that may produce every label, then a vector match on all of them.
func namesKey(expr, key string) bool {
	if regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(key) + `($|[^A-Za-z0-9_])`).MatchString(expr) {
		return true
	}
	refs, err := parse.LogQL(expr)
	if err != nil {
		return true
	}
	for _, r := range refs {
		if r.AllLabels {
			return true
		}
	}
	return false
}
