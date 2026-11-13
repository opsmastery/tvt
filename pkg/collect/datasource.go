package collect

// QueryLanguage maps a Grafana datasource plugin type to the query
// language tvt parses: promql or logql. VictoriaMetrics' plugins speak
// MetricsQL, a PromQL superset, and are read as PromQL: what the PromQL
// parser rejects becomes assumed references (Sprint 2 item 17). Every
// other type, and an unresolved (empty) type, returns false; callers
// decide what an unknown type means.
//
// This is the one list of datasource types tvt reads queries from; the
// Grafana collector and the scan both use it.
func QueryLanguage(dsType string) (string, bool) {
	switch dsType {
	case "prometheus", "victoriametrics-metrics-datasource", "victoriametrics-datasource":
		return "promql", true
	case "loki":
		return "logql", true
	}
	return "", false
}
