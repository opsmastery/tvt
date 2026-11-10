package parse

import (
	"reflect"
	"testing"
)

func TestLogStreamSelectors(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  []string // canonical selectors
	}{
		{`sum(count_over_time({app="shop", component="audit"} |= "result=denied" [5m])) > 20`,
			[]string{`{app="shop", component="audit"}`}},
		{`{component="audit",app="shop"}`, []string{`{app="shop", component="audit"}`}},
		{"{app=~`shop|cart`, level!=\"debug\"}", []string{`{app=~"shop|cart", level!="debug"}`}},
		{`{app="shop"} | line_format "{{.msg}} {not=\"a selector\"}"`, []string{`{app="shop"}`}},
		{`sum by (app) (rate({app="a"}[1m])) / sum(rate({app!~"b"}[1m]))`,
			[]string{`{app="a"}`, `{app!~"b"}`}},
		{"# {commented=\"out\"}\n{app=\"shop\"}", []string{`{app="shop"}`}},
		{`{ app = "a\"b" }`, []string{`{app="a\"b"}`}},
	} {
		refs, err := LogStreamSelectors(tc.query)
		if err != nil {
			t.Errorf("%s: %v", tc.query, err)
			continue
		}
		var got []string
		for _, r := range refs {
			if r.Metric != "" || r.Grouping != nil || r.Without != nil {
				t.Errorf("%s: ref has more than matchers: %+v", tc.query, r)
			}
			got = append(got, CanonicalSelector(r.Matchers))
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.query, got, tc.want)
		}
	}
}

func TestLogStreamSelectorsErrors(t *testing.T) {
	for _, q := range []string{
		``,
		`count_over_time([5m])`,
		`{}`,
		`{app}`,
		`{app="shop"`,
		`{app=shop}`,
		`{app="shop" component="x"}`,
		`{app="shop"} |= "unterminated`,
		`{1app="x"}`,
		`app="x"}`,
	} {
		if refs, err := LogStreamSelectors(q); err == nil {
			t.Errorf("%q: got %+v, want an error", q, refs)
		}
	}
}
