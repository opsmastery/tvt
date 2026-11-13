package parse

import (
	"reflect"
	"testing"
)

func TestUsesAbsence(t *testing.T) {
	for q, want := range map[string]bool{
		`absent(up{job="shop"})`:                                  true,
		`absent_over_time(up{job="shop"}[10m]) == 1`:              true,
		`absent_over_time({app="shop", component="worker"}[15m])`: true, // LogQL, parses as PromQL
		`absent_over_time({app="shop"} |= "sync ok" [15m])`:       true, // LogQL the PromQL parser rejects
		`sum(rate(x[5m])) > 0 or absent (x)`:                      true,
		`sum by (job) (absent(up{job="$job"}))`:                   true,
		`label_replace(absent(up), "x", "y", "", "")`:             true,
		`(ABSENT(up{job="a"})) default 0`:                         true, // MetricsQL: unparsed, any case
		"# missing\nabsent(up)":                                   true,
		`x > 10 and absent(m)`:                                    false, // the result is x's
		`x unless absent(m)`:                                      false,
		`x * on() absent(m)`:                                      false, // x must exist for a series
		`WITH (a = absent(m)) a default 0`:                        false, // unparsed, absent() isn't the outer call
		`absent`:                                                  false, // a metric named absent
		`absent_requests_total > 0`:                               false,
		`sum(rate(x[5m])) by (absent)`:                            false,
		`label_replace(up, "x", "absent(", "", "")`:               false, // inside a string
		"up # absent(up)":                                         false, // comment
		`sum(rate(http_requests_total[5m]))`:                      false,
		``:                                                        false,
	} {
		if got := UsesAbsence(q); got != want {
			t.Errorf("UsesAbsence(%q) = %v, want %v", q, got, want)
		}
	}
}

// An absence alert reads its metric and the labels it matches like any
// query: they're hard keeps, and dropping job would change what fires.
func TestAbsenceRefs(t *testing.T) {
	for q, want := range map[string][]Ref{
		`absent(shop_inventory_sync_last_success_timestamp_seconds{job="shop"})`: {{
			Metric:   "shop_inventory_sync_last_success_timestamp_seconds",
			Matchers: []Matcher{{Label: "job", Op: MatchEqual, Value: "shop"}},
		}},
		`absent_over_time(up{job="shop",instance=~"a.*"}[10m])`: {{
			Metric:   "up",
			Matchers: []Matcher{{Label: "job", Op: MatchEqual, Value: "shop"}, {Label: "instance", Op: MatchRegexp, Value: "a.*"}},
		}},
	} {
		got, err := PromQL(q)
		if err != nil {
			t.Fatal(err)
		}
		for i := range got {
			got[i].Grouping, got[i].Without, got[i].Created, got[i].Filters, got[i].StreamLabels, got[i].Extracted, got[i].Reasons = nil, nil, nil, nil, nil, nil, nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", q, got, want)
			continue
		}
		if got[0].ResultLabels {
			t.Errorf("%s: absent() removes the selector's labels from the result", q)
		}
	}
}

func FuzzUsesAbsence(f *testing.F) {
	f.Add(`absent(up{job="a"})`)
	f.Add("`absent(`")
	f.Fuzz(func(t *testing.T, s string) { UsesAbsence(s) })
}
