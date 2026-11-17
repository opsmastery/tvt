package propose

import (
	"regexp"
	"strings"
	"testing"
)

// otelMetric is what a filter condition sees.
type otelMetric struct {
	name, unit string
	monotonic  bool
}

// eval evaluates OtelConditions' output on m: the conditions are a small,
// fixed grammar (==, IsMatch, and, or with one level of parentheses).
func eval(t *testing.T, cond string, m otelMetric) bool {
	t.Helper()
	atom := func(a string) bool {
		a = strings.TrimSpace(a)
		switch {
		case a == "metric.is_monotonic == true":
			return m.monotonic
		case a == "metric.is_monotonic == false":
			return !m.monotonic
		case strings.HasPrefix(a, "metric.name == "):
			return `"`+m.name+`"` == strings.TrimPrefix(a, "metric.name == ")
		case strings.HasPrefix(a, "metric.unit == "):
			return `"`+m.unit+`"` == strings.TrimPrefix(a, "metric.unit == ")
		case strings.HasPrefix(a, "IsMatch(metric.name, "):
			re := strings.TrimSuffix(strings.TrimPrefix(a, `IsMatch(metric.name, "`), `")`)
			return regexp.MustCompile(re).MatchString(m.name)
		case strings.HasPrefix(a, "IsMatch(metric.unit, "):
			re := strings.TrimSuffix(strings.TrimPrefix(a, `IsMatch(metric.unit, "`), `")`)
			return regexp.MustCompile(re).MatchString(m.unit)
		}
		t.Fatalf("unknown atom %q", a)
		return false
	}
	ok := true
	for _, part := range splitTop(cond, " and ") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "(") {
			any := false
			for _, alt := range splitTop(part[1:len(part)-1], " or ") {
				any = any || atom(alt)
			}
			ok = ok && any
		} else {
			ok = ok && atom(part)
		}
	}
	return ok
}

// splitTop splits on sep outside parentheses and quotes.
func splitTop(s, sep string) []string {
	var out []string
	depth, inq, last := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"':
			inq = !inq
		case inq:
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], sep):
			out = append(out, s[last:i])
			last = i + len(sep)
		}
	}
	return append(out, s[last:])
}

func matches(t *testing.T, prom string, m otelMetric) bool {
	cs, ok := OtelConditions(prom)
	if !ok {
		t.Fatalf("%s: not matchable", prom)
	}
	for _, c := range cs {
		if eval(t, c, m) {
			return true
		}
	}
	return false
}

func TestOtelConditions(t *testing.T) {
	for _, c := range []struct {
		prom string
		m    otelMetric
		want bool
	}{
		// The lab's coding agent (tvt-lab genai-agent).
		{"coding_agent_token_usage_total", otelMetric{"coding_agent.token.usage", "{token}", true}, true},
		{"coding_agent_cost_usage_USD_total", otelMetric{"coding_agent.cost.usage", "USD", true}, true},
		{"coding_agent_cost_usage_USD_total", otelMetric{"coding_agent.cost.usage", "EUR", true}, false},
		{"coding_agent_cost_usage_USD_total", otelMetric{"coding_agent.cost.usage", "USD", false}, false},
		{"coding_agent_token_usage_total", otelMetric{"coding_agent.token.usage.total", "", true}, true},
		{"coding_agent_token_usage_total", otelMetric{"coding_agent.token", "", true}, false},
		// Units and per-units.
		{"http_server_request_duration_seconds", otelMetric{"http.server.request.duration", "s", false}, true},
		{"http_server_request_duration_seconds", otelMetric{"http.server.request.duration", "ms", false}, false},
		{"process_runtime_memory_bytes", otelMetric{"process.runtime.memory", "By", false}, true},
		{"net_io_bytes_per_second", otelMetric{"net.io", "By/s", false}, true},
		{"cpu_utilization_ratio", otelMetric{"cpu.utilization", "1", false}, true},
		{"queue_depth", otelMetric{"queue.depth", "", false}, true},
		{"queue_depth", otelMetric{"queue.depth", "{item}", false}, true},
		{"queue_depth", otelMetric{"queue.depth", "", true}, false},
		// A gauge whose name ends in total keeps it.
		{"jobs_total", otelMetric{"jobs.total", "", false}, true},
		{"jobs_total", otelMetric{"jobs", "", false}, false},
		// Untranslated (UTF-8) names match exactly.
		{"queue_depth", otelMetric{"queue_depth", "s", true}, true},
	} {
		if got := matches(t, c.prom, c.m); got != c.want {
			cs, _ := OtelConditions(c.prom)
			t.Errorf("%s vs %+v: %v, want %v\n%s", c.prom, c.m, got, c.want, strings.Join(cs, "\n"))
		}
	}
	for _, p := range []string{"rpc_duration_bucket", "rpc_duration_sum", "rpc_duration_count"} {
		if _, ok := OtelConditions(p); ok {
			t.Errorf("%s: a histogram series must not be matchable", p)
		}
	}
}

func TestOtelcolPromNames(t *testing.T) {
	src := read(t, "otelcol.yaml")
	d := Drop{ID: "otelcol-agent", Scan: 7, PromNames: true, Metrics: []string{"coding_agent_cost_usage_USD_total"}}
	out, err := Otelcol(src, []string{"metrics"}, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `- 'metric.is_monotonic == true and IsMatch(metric.name, "^coding[^A-Za-z0-9:]+agent[^A-Za-z0-9:]+cost[^A-Za-z0-9:]+usage(?:[^A-Za-z0-9:]+total)?$") and metric.unit == "USD"'`) {
		t.Errorf("no translated-name condition:\n%s", out)
	}
	d.Metrics = []string{"rpc_duration_bucket"}
	if _, err := Otelcol(src, []string{"metrics"}, d); err == nil || !strings.Contains(err.Error(), "histogram") {
		t.Errorf("histogram series: err %v", err)
	}
}
