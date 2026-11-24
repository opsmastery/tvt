package scan

import (
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
)

// FuzzFilterSelects: any expression and pattern give an answer without a
// panic, and a consumer with no line filter at all never picks lines out.
func FuzzFilterSelects(f *testing.F) {
	for _, s := range [][2]string{
		{`{app="shop"} |= "route=/healthz" != "status=200"`, "route=/healthz status=200 probe=<_>"},
		{`{app="shop"} | logfmt | status != "200"`, "status=200"},
		{"sum(rate({a=\"b\"} |~ `x` [5m]))", "x"},
		{`{a="{{"} |= "\"`, `"`},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, expr, pattern string) {
		got := filterSelects(expr, pattern)
		if got && !strings.ContainsAny(expr, "|") {
			t.Fatalf("filterSelects(%q, %q) = true with no positive line filter", expr, pattern)
		}
	})
}

// FuzzMergePatterns: merging keeps every line and every byte, never adds
// a pattern, and comes out most bytes first.
func FuzzMergePatterns(f *testing.F) {
	f.Add("route=/healthz probe=a x=<_>\nroute=/healthz probe=b x=<_>\nroute=<_> user=<_>", uint16(3))
	f.Add("a b c\na b d\na e d\n\n<_>", uint16(1))
	f.Fuzz(func(t *testing.T, text string, lines uint16) {
		var ps []collect.LinePattern
		var wantLines, wantBytes int64
		for i, p := range strings.Split(text, "\n") {
			if i >= 32 {
				break
			}
			n := int64(lines) + int64(i)
			ps = append(ps, collect.LinePattern{Pattern: p, Lines: n, Window: time.Duration(i) * time.Minute})
			wantLines += n
			wantBytes += n * patternBytes(p)
		}
		out := mergePatterns(ps)
		var gotLines, gotBytes int64
		for i, m := range out {
			gotLines += m.Lines
			gotBytes += m.bytes
			if i > 0 && m.bytes > out[i-1].bytes {
				t.Fatalf("not sorted: %+v", out)
			}
		}
		if len(out) > len(ps) || gotLines != wantLines || gotBytes != wantBytes {
			t.Fatalf("merged %d patterns into %d: lines %d→%d, bytes %d→%d", len(ps), len(out), wantLines, gotLines, wantBytes, gotBytes)
		}
	})
}
