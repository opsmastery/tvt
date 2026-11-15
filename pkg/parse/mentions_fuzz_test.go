package parse

import (
	"slices"
	"strings"
	"testing"
)

// Mentions never panics; it returns sorted, unique, metric-shaped words
// that occur in the text.
func FuzzMentions(f *testing.F) {
	for _, s := range []string{
		"check node_load15 first", "words: then job:rate5m:sum:", "<p>shop_cache_hits_total</p>", "a_b", "x:", "::::", "é_ü node_cpu_seconds_total",
	} {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, text string, isHTML bool) {
		got := Mentions(text, isHTML)
		if !slices.IsSorted(got) {
			t.Fatalf("unsorted: %q", got)
		}
		for i, m := range got {
			if i > 0 && got[i-1] == m {
				t.Fatalf("duplicate %q", m)
			}
			if len(m) < 4 || !strings.ContainsAny(m, "_:") || strings.HasSuffix(m, ":") {
				t.Fatalf("not metric-shaped: %q", m)
			}
			if !isHTML && !strings.Contains(text, m) {
				t.Fatalf("%q isn't in the text", m)
			}
		}
	})
}
