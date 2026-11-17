package propose

import (
	"strings"
	"testing"
)

func TestOTTLFormFor(t *testing.T) {
	for _, c := range []struct {
		v       string
		context bool
		note    string
		err     string
	}{
		{"0.165.0", true, "", ""},
		{"v0.122.0", true, "", ""},
		{"0.121.0", false, "predates v0.122.0", ""},
		{"0.80.0", false, "predates v0.122.0", ""},
		{"", false, "version unknown", ""},
		{"0.79.9", false, "", "predates v0.80.0"},
		{"1.2", false, "", "MAJOR.MINOR.PATCH"},
	} {
		f, err := OTTLFormFor(c.v)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: err %v, want %q", c.v, err, c.err)
			}
			continue
		}
		if err != nil || f.Context != c.context || (c.note == "") != (f.Note == "") || !strings.Contains(f.Note, c.note) {
			t.Errorf("%q: %+v, %v", c.v, f, err)
		}
	}
}

func TestBarePaths(t *testing.T) {
	in := `metric.is_monotonic == true and IsMatch(metric.name, "^a$") and metric.name == "metric.name" and (metric.unit == "" or metric.unit == "1")`
	want := `is_monotonic == true and IsMatch(name, "^a$") and name == "metric.name" and (unit == "" or unit == "1")`
	if got := barePaths(in); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestLintOTTL(t *testing.T) {
	ctx, bare := OTTLForm{Context: true}, OTTLForm{}
	for _, c := range []struct {
		cond string
		form OTTLForm
		err  string
	}{
		{`metric.name == "a"`, ctx, ""},
		{`name == "a"`, bare, ""},
		{`IsMatch(metric.name, "^a(b)?$")`, ctx, ""},
		{`name == "a"`, ctx, "form"},
		{`metric.name == "a"`, bare, "form"},
		{`ReplacePattern(metric.name, "a", "b")`, ctx, "function ReplacePattern"},
		{`metric.name == "a`, ctx, "quotes"},
		{`IsMatch(metric.name, "a"`, ctx, "parentheses"},
	} {
		err := lintOTTL(c.cond, c.form)
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: %v, want %q", c.cond, err, c.err)
		}
	}
}

// TestOtelcolForms: every condition tvt writes passes the lint in both
// forms (every translated-name shape the name tests use).
func TestOtelcolForms(t *testing.T) {
	src := read(t, "otelcol.yaml")
	for _, v := range []string{"0.165.0", "0.100.0", ""} {
		d := Drop{ID: "x", Scan: 1, PromNames: true, Collector: v, Metrics: []string{
			"coding_agent_cost_usage_USD_total", "http_server_request_duration_seconds", "net_io_bytes_per_second", "cpu_utilization_ratio", "queue_depth", "jobs_total"}}
		out, err := Otelcol(src, []string{"metrics"}, d)
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		hasCtx := strings.Contains(string(out), "metric.name")
		if hasCtx != (v == "0.165.0") {
			t.Errorf("%q: context paths %v", v, hasCtx)
		}
	}
}
