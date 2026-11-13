package parse

import "testing"

// A WITH filter used as a whole selector selects every metric with its
// labels, so it stays a match-every-metric reference; spliced into a named
// selector it adds nothing.
func TestMetricsQLWithFilterUse(t *testing.T) {
	for _, tc := range []struct {
		q        string
		wildcard bool
		metrics  []string
	}{
		{`WITH (cf = {job="api"}) sum(rate(http_requests_total{cf}[5m]))`, false, []string{"http_requests_total"}},
		{`WITH (cf = {job="api"}) count(cf)`, true, nil},
		{`WITH (cf = {job="api"}, x = cf) count(x) + up`, true, []string{"up"}},
		{`WITH (s = {job="api"} + 1) s`, true, nil},
	} {
		refs, err := PromQL(tc.q)
		if err == nil {
			t.Errorf("%s: parsed as PromQL; the test needs a MetricsQL-only query", tc.q)
		}
		wildcard := false
		var metrics []string
		for _, r := range refs {
			if r.Metric == "" {
				wildcard = true
				continue
			}
			metrics = append(metrics, r.Metric)
		}
		if wildcard != tc.wildcard || len(metrics) != len(tc.metrics) {
			t.Errorf("%s: wildcard %v metrics %v, want %v %v", tc.q, wildcard, metrics, tc.wildcard, tc.metrics)
			continue
		}
		for i := range metrics {
			if metrics[i] != tc.metrics[i] {
				t.Errorf("%s: metrics %v, want %v", tc.q, metrics, tc.metrics)
			}
		}
	}
}
