package parse

import (
	"reflect"
	"testing"
)

func TestPromVariable(t *testing.T) {
	for _, tc := range []struct {
		query   string
		want    []Ref
		wantErr bool
	}{
		{query: "label_values(shop_http_requests_total, route)",
			want: []Ref{{Metric: "shop_http_requests_total", Grouping: []string{"route"}}}},
		{query: ` label_values( up{job="api", path=~"/a,b"} , instance ) `,
			want: []Ref{{Metric: "up", Matchers: []Matcher{{Label: "job", Op: MatchEqual, Value: "api"}, {Label: "path", Op: MatchRegexp, Value: "/a,b"}}, Grouping: []string{"instance"}}}},
		{query: "label_values(job)"},
		{query: "label_names()"},
		{query: "label_names(up)", want: []Ref{{Metric: "up", AllLabels: true}}},
		{query: "label_values(up, __name__)", want: []Ref{{Metric: "up"}}},
		{query: "metrics(shop_.*)", want: []Ref{{Matchers: []Matcher{{Label: "__name__", Op: MatchRegexp, Value: ".*(?:shop_.*).*"}}}}},
		{query: "metrics($prefix.*)", want: []Ref{{Matchers: []Matcher{{Label: "__name__", Op: MatchRegexp, Value: ".*(?:$prefix.*).*", Variable: true}}}}},
		{query: "label_values(up, $label)", want: []Ref{{Metric: "up", AllLabels: true, Assumed: true,
			Reasons: []string{"label_values() label $label comes from a variable"}}}},
		{query: "query_result(topk(5, sum by (route) (rate(x[5m]))))", want: []Ref{{Metric: "x", Grouping: []string{"route"}}}},
		{query: "sum by (op) (y)", want: []Ref{{Metric: "y", Grouping: []string{"op"}}}},
		// Not one call over the whole text: plain PromQL.
		{query: "metrics(a) + metrics(b)", wantErr: true},
	} {
		got, err := PromVariable(tc.query)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err = %v", tc.query, err)
			continue
		}
		if tc.wantErr {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q\n got  %+v\n want %+v", tc.query, got, tc.want)
		}
	}
}

func TestLogVariable(t *testing.T) {
	got, err := LogVariable(`label_values({app="shop"}, level)`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || CanonicalSelector(got[0].Matchers) != `{app="shop"}` ||
		!reflect.DeepEqual(got[0].StreamLabels, []string{"app", "level"}) {
		t.Errorf("label_values(sel, level) = %+v", got)
	}
	got, err = LogVariable("label_values(level)")
	if err != nil || len(got) != 1 || len(got[0].Matchers) != 0 || !reflect.DeepEqual(got[0].StreamLabels, []string{"level"}) {
		t.Errorf("label_values(level) = %+v, %v", got, err)
	}
	got, err = LogVariable("label_values([[label]])")
	if err != nil || len(got) != 1 || !got[0].AllLabels || !got[0].Assumed || got[0].StreamLabels != nil {
		t.Errorf("label_values([[label]]) = %+v, %v", got, err)
	}
	got, err = LogVariable(`{app="shop"} | json`)
	if err != nil || len(got) != 1 {
		t.Errorf("plain LogQL = %+v, %v", got, err)
	}
}
