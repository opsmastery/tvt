package propose

import (
	"strings"
	"testing"
)

var agg = Aggregate{ID: "otelcol-agent/labels", Scan: 8, Collector: "0.165.0", Reason: "session_id, user_email: unread, one series per session",
	Labels: map[string][]string{
		"coding_agent_token_usage_total":    {"session_id", "user_email"},
		"coding_agent_cost_usage_USD_total": {"session_id", "user_email"},
	}}

func TestOtelcolAggregate(t *testing.T) {
	src := read(t, "otelcol.yaml")
	out, err := OtelcolChanges(src, []string{"metrics"}, nil, &agg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"      processors: [transform/tvt_aggregate, interval/tvt, delta_to_cumulative, batch]\n",
		"  transform/tvt_aggregate:\n    error_mode: ignore\n    metric_statements:\n      - context: datapoint\n        statements:\n",
		`        - 'delete_matching_keys(datapoint.attributes, "^session[^A-Za-z0-9:]+id$") where (metric.name == "coding_agent_cost_usage_USD_total")`,
		"  interval/tvt:\n    interval: 60s\n  # tvt:end\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if again, err := OtelcolChanges(out, []string{"metrics"}, nil, &agg); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	// With the filter drop too, in one pass; a rerun keeps both.
	d := otelDrop
	both, err := OtelcolChanges(src, []string{"metrics"}, &d, &agg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(both), "processors: [filter/tvt, transform/tvt_aggregate, interval/tvt, delta_to_cumulative, batch]") {
		t.Errorf("both:\n%s", both)
	}
	if again, err := OtelcolChanges(both, []string{"metrics"}, &d, &agg); err != nil || string(again) != string(both) {
		t.Errorf("rerun of both changed the file (err %v)", err)
	}
	stripped, _, _ := stripBlocks(both, "#")
	if stripped, _ = stripOtelRefs(stripped); string(stripped) != string(src) {
		t.Errorf("stripping gives back the original?\n%s", stripped)
	}
}

func TestOtelcolAggregateRefuses(t *testing.T) {
	src := read(t, "otelcol.yaml")
	noDelta := strings.Replace(string(src), "processors: [delta_to_cumulative, batch]", "processors: [batch]", 1)
	old := agg
	old.Collector = "0.110.0"
	bad := agg
	bad.Labels = map[string][]string{"rpc_duration_bucket": {"session_id"}}
	for _, c := range []struct {
		name, src string
		a         Aggregate
		want      string
	}{
		{"no delta_to_cumulative", noDelta, agg, "no delta_to_cumulative"},
		{"old collector", string(src), old, "v0.122.0 and later"},
		{"histogram series", string(src), bad, "histogram"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := OtelcolChanges([]byte(c.src), []string{"metrics"}, nil, &c.a); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err %v, want %q", err, c.want)
			}
		})
	}
}

// TestOtelcolBucket is S10 item 1's writer: a bucketed label is rewritten
// to its class in the same ordered pipeline as label removals, so the
// window sums the series that merge.
func TestOtelcolBucket(t *testing.T) {
	src := read(t, "otelcol.yaml")
	b := Aggregate{ID: "otelcol-agent/labels", Scan: 9, Collector: "0.165.0", Reason: "http_status_code read only by class: bucketed",
		Buckets: map[string][]string{"coding_agent_token_usage_total": {"http_status_code"}}}
	out, err := OtelcolChanges(src, []string{"metrics"}, nil, &b)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"processors: [transform/tvt_aggregate, interval/tvt, delta_to_cumulative, batch]",
		`replace_pattern(datapoint.attributes["http_status_code"], "^([0-9])[0-9][0-9]$", "$${1}00") where (metric.name == "coding_agent_token_usage_total")`,
		`replace_pattern(datapoint.attributes["http.status.code"], "^([0-9])[0-9][0-9]$", "$${1}00")`,
		"# delete or bucket the labels",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "delete_matching_keys") {
		t.Error("a bucket deletes the label")
	}
	if again, err := OtelcolChanges(out, []string{"metrics"}, nil, &b); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	// Both in one block: removals and buckets.
	both := agg
	both.Buckets = b.Buckets
	if out, err := OtelcolChanges(src, []string{"metrics"}, nil, &both); err != nil || !strings.Contains(string(out), "delete_matching_keys") || !strings.Contains(string(out), "replace_pattern") {
		t.Errorf("both: %v\n%s", err, out)
	}
	for _, l := range []string{"job", "__name__"} {
		bad := b
		bad.Buckets = map[string][]string{"coding_agent_token_usage_total": {l}}
		if _, err := OtelcolChanges(src, []string{"metrics"}, nil, &bad); err == nil {
			t.Errorf("bucketed %s", l)
		}
	}
}
