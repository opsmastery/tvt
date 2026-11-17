package propose

import (
	"strings"
	"testing"
)

var otelDrop = Drop{ID: "otelcol-agent", Scan: 7, Evidence: "0123456789ab",
	Reason:  "2 unread metrics, see tvt why",
	Metrics: []string{"gen_ai.client.token.usage", "coding_agent.cost.usage"}}

func TestOtelcolFlow(t *testing.T) {
	src := read(t, "otelcol.yaml")
	out, err := Otelcol(src, []string{"metrics"}, otelDrop)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"  # tvt:begin otelcol-agent scan=7 evidence=0123456789ab\n  # 2 unread metrics, see tvt why\n  filter/tvt:\n    error_mode: ignore\n    metrics:\n      metric:\n" +
			"        - 'metric.name == \"coding_agent.cost.usage\"'\n        - 'metric.name == \"gen_ai.client.token.usage\"'\n  # tvt:end\n",
		"      processors: [filter/tvt, delta_to_cumulative, batch]\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	// Traces and logs are untouched.
	if strings.Count(s, "filter/tvt") != 2 {
		t.Errorf("filter/tvt appears %d times, want 2 (definition, metrics pipeline)", strings.Count(s, "filter/tvt"))
	}
	again, err := Otelcol(out, []string{"metrics"}, otelDrop)
	if err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v): regeneration must be idempotent\n%s", err, again)
	}
	stripped, _, err := stripBlocks(out, "#")
	if err != nil {
		t.Fatal(err)
	}
	if stripped, err = stripOtelRefs(stripped); err != nil || string(stripped) != string(src) {
		t.Errorf("stripping gives back the original? err=%v\n%s", err, stripped)
	}
}

func TestOtelcolBlock(t *testing.T) {
	src := read(t, "otelcol-block.yaml")
	out, err := Otelcol(src, []string{"metrics", "metrics/raw"}, otelDrop)
	if err != nil {
		t.Fatal(err)
	}
	added := onlyAdded(t, src, out)
	s := string(out)
	for _, want := range []string{
		"      processors:\n        # tvt:begin otelcol-agent scan=7 evidence=0123456789ab\n        - filter/tvt\n        # tvt:end\n        - memory_limiter\n",
		"      exporters: [debug]\n      # tvt:begin otelcol-agent scan=7 evidence=0123456789ab\n      processors: [filter/tvt]\n      # tvt:end\n    logs:\n",
		"  batch:\n  # tvt:begin otelcol-agent",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if len(added) != 9+3+3 {
		t.Errorf("added %d lines:\n%s", len(added), strings.Join(added, "\n"))
	}
	if again, err := Otelcol(out, []string{"metrics", "metrics/raw"}, otelDrop); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)", err)
	}
}

func TestOtelcolRefuses(t *testing.T) {
	src := read(t, "otelcol.yaml")
	for _, c := range []struct {
		name      string
		src       string
		pipelines []string
		d         Drop
		want      string
	}{
		{"logs pipeline", string(src), []string{"logs"}, otelDrop, "isn't a metrics pipeline"},
		{"missing pipeline", string(src), []string{"metrics/other"}, otelDrop, "no service.pipelines.metrics/other"},
		{"no pipelines", string(src), nil, otelDrop, "no pipeline"},
		{"bad name", string(src), []string{"metrics"}, Drop{ID: "x", Metrics: []string{"has space"}}, "not an OpenTelemetry metric name"},
		{"quote in name", string(src), []string{"metrics"}, Drop{ID: "x", Metrics: []string{`a"b`}}, "not an OpenTelemetry metric name"},
		{"no processors", "service:\n  pipelines:\n    metrics:\n      receivers: [otlp]\n", []string{"metrics"}, otelDrop, "no processors section"},
		{"hand-written filter/tvt", strings.Replace(string(src), "  batch: {}\n", "  batch: {}\n  filter/tvt: {}\n", 1), []string{"metrics"}, otelDrop, "outside a tvt block"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Otelcol([]byte(c.src), c.pipelines, c.d)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err %v, want %q", err, c.want)
			}
		})
	}
}
