package scan

import (
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/config"
	"github.com/opsmastery/tvt/pkg/graph"
)

func TestMarkRecords(t *testing.T) {
	stream := func(sel string) graph.Signal {
		return graph.Signal{ID: "loki:stream:" + sel, Kind: graph.KindStream, Name: sel, Source: "loki"}
	}
	metric := func(name string) graph.Signal {
		return graph.Signal{ID: "prom:metric:" + name, Kind: graph.KindMetric, Name: name, Source: "prom"}
	}
	signals := []graph.Signal{
		metric("k8s_audit_events_total"),
		metric("argo_deployment_events_total"),
		metric("payments_ledger_entries_total"),
		metric("shop_auditor_bonus_total"), // "auditor" isn't the token audit
		metric("shop_http_requests_total"),
		{ID: "prom:label:k8s_audit_events_total:verb", Kind: graph.KindLabel, Name: "verb", Parent: "k8s_audit_events_total", Source: "prom"},
		stream(`{app="shop", component="audit", level="info"}`),
		stream(`{app="shop", level=~"info|warn|error"}`),
		stream(`{app="shop", component="api", level="debug"}`),
		stream(`{app="billing"}`),
		stream(AllStreams),
	}
	labelSets := []map[string]string{
		{"app": "shop", "component": "audit", "level": "info"},
		{"app": "shop", "component": "api", "level": "info"},
		{"app": "shop", "component": "api", "level": "debug"},
		{"app": "billing", "component": "ledger"},
	}
	rc := config.Records{Metrics: []string{"payments_ledger_.*"}, Streams: []string{`{app="billing"}`}}
	if err := markRecords(signals, rc, labelSets); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"prom:metric:k8s_audit_events_total":                        "named like a record",
		"prom:metric:argo_deployment_events_total":                  "named like a record",
		"prom:metric:payments_ledger_entries_total":                 "declared (records.metrics payments_ledger_.*)",
		"prom:metric:shop_auditor_bonus_total":                      "",
		"prom:metric:shop_http_requests_total":                      "",
		"prom:label:k8s_audit_events_total:verb":                    "named like a record",
		`loki:stream:{app="shop", component="audit", level="info"}`: `named like a record (component="audit")`,
		`loki:stream:{app="shop", level=~"info|warn|error"}`:        `selects a record stream: named like a record (component="audit")`,
		`loki:stream:{app="shop", component="api", level="debug"}`:  "",
		`loki:stream:{app="billing"}`:                               `declared (records.streams {app="billing"})`,
		"loki:stream:" + AllStreams:                                 "selects a record stream: ",
	}
	for _, s := range signals {
		w := want[s.ID]
		switch {
		case w == "selects a record stream: ":
			if !strings.HasPrefix(s.Record, w) {
				t.Errorf("%s: record %q, want it to select a record stream", s.ID, s.Record)
			}
		case s.Record != w:
			t.Errorf("%s: record %q, want %q", s.ID, s.Record, w)
		}
	}
	if err := markRecords(signals, config.Records{Streams: []string{"{"}}, nil); err == nil {
		t.Error("a bad records.streams selector was accepted")
	}
}
