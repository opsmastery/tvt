package parse

import (
	"reflect"
	"sort"
	"testing"
)

// FuzzLogStreamSelectors: the stopgap extractor never panics; whatever it
// accepts has matchers with label names, and its canonical form parses back
// to the same matchers. (A rejected query references every stream, so
// rejecting is always safe; accepting wrongly is not.)
func FuzzLogStreamSelectors(f *testing.F) {
	for _, s := range []string{
		`sum(count_over_time({app="shop", component="audit"} |= "result=denied" [5m])) > 20`,
		`{app="shop"} | line_format "{{.msg}}"`,
		"{app=~`a|b`, level!=\"debug\"}",
		`sum by (app) (rate({app="a"}[1m])) / sum(rate({app!~"b"}[1m]))`,
		"# {x=\"y\"}\n{app=\"shop\"}",
		`{app="a\"b"}`, `{}`, `{app}`, `}`, `"{`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		refs, err := LogStreamSelectors(q)
		if err != nil {
			return
		}
		if len(refs) == 0 {
			t.Fatalf("%q: no error and no selectors", q)
		}
		for _, r := range refs {
			if len(r.Matchers) == 0 {
				t.Fatalf("%q: empty selector accepted", q)
			}
			canon := CanonicalSelector(r.Matchers)
			back, err := LogStreamSelectors(canon)
			if err != nil || len(back) != 1 {
				t.Fatalf("%q: canonical %q does not parse back: %v", q, canon, err)
			}
			want := append([]Matcher(nil), r.Matchers...)
			sort.Slice(want, func(i, j int) bool { return CanonicalSelector(want[i:i+1]) < CanonicalSelector(want[j:j+1]) })
			got := append([]Matcher(nil), back[0].Matchers...)
			sort.Slice(got, func(i, j int) bool { return CanonicalSelector(got[i:i+1]) < CanonicalSelector(got[j:j+1]) })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%q: canonical %q parses back to %+v, want %+v", q, canon, got, want)
			}
		}
	})
}
