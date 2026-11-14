package collect

import "testing"

func TestQueryLanguage(t *testing.T) {
	for typ, want := range map[string]string{
		"prometheus": "promql", "victoriametrics-metrics-datasource": "promql",
		"victoriametrics-datasource": "promql", "loki": "logql",
		"": "", "tempo": "", "Prometheus": "", "elasticsearch": "",
	} {
		got, ok := QueryLanguage(typ)
		if got != want || ok != (want != "") {
			t.Errorf("QueryLanguage(%q) = %q, %v; want %q", typ, got, ok, want)
		}
	}
}
