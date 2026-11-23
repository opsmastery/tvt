package check

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/report"
)

// TestLogDedupNames: the log deduplication processor is a collapse under
// its current name (log_dedup), its deprecated one (logdedup), and with
// every setting at its default (no config block at all), which check
// used to skip.
func TestLogDedupNames(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	m := report.Manifest{Version: 2, Repo: "r", GeneratedAt: now,
		PipelineDefs: []report.PipelineDef{{Name: "agent", Kind: "otelcol", Files: []string{"config.yaml"}}},
		Signals: []report.ProtectedSignal{{ID: `loki:stream:{service_name="coding-agent"}`, Kind: "stream",
			Name: `{service_name="coding-agent"}`, Pipeline: "agent", Class: "keep_high"}}}
	base := "receivers:\n  otlp:\n    protocols:\n      http:\nexporters:\n  debug:\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      exporters: [debug]\n"
	files := func(src string) Files {
		return func(path string) ([]byte, error) {
			if path == "config.yaml" {
				return []byte(src), nil
			}
			return nil, errors.New("no such file")
		}
	}
	for name, head := range map[string]string{
		"log_dedup with settings": "receivers:\n  otlp:\n    protocols:\n      http:\nprocessors:\n  log_dedup:\n    interval: 60s\nexporters:\n  debug:\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      processors: [log_dedup]\n      exporters: [debug]\n",
		"log_dedup defaults":      "receivers:\n  otlp:\n    protocols:\n      http:\nprocessors:\n  log_dedup:\nexporters:\n  debug:\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      processors: [log_dedup]\n      exporters: [debug]\n",
		"logdedup (deprecated)":   "receivers:\n  otlp:\n    protocols:\n      http:\nprocessors:\n  logdedup/x:\nexporters:\n  debug:\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      processors: [logdedup/x]\n      exporters: [debug]\n",
	} {
		r := Run(Input{Manifest: m, Base: files(base), Head: files(head), Now: now, SkipConfirmation: true})
		found := false
		for _, f := range r.Findings {
			if f.Rule == RuleCollapse {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no collapse_counts finding: %v", name, r.Findings)
		}
	}
}

// TestLogDedupConditions: with conditions, log_dedup collapses only the
// logs they can match. Scoped away from every read stream and record
// event, it passes; a condition that can match one is refused, and so is
// one check can't decide.
func TestLogDedupConditions(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	m := report.Manifest{Version: 2, Repo: "r", GeneratedAt: now,
		PipelineDefs:  []report.PipelineDef{{Name: "agent", Kind: "otelcol", Files: []string{"config.yaml"}}},
		RecordMatches: report.RecordMatches{LogEvents: []string{`attributes["event.name"] == "tool_result"`}},
		Signals: []report.ProtectedSignal{{ID: `loki:stream:{service_name="coding-agent"}`, Kind: "stream",
			Name: `{service_name="coding-agent"}`, Pipeline: "agent", Class: "keep_high"}}}
	base := "receivers:\n  otlp:\n    protocols:\n      http:\nexporters:\n  debug:\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      exporters: [debug]\n"
	head := func(conds ...string) Files {
		var b strings.Builder
		b.WriteString("receivers:\n  otlp:\n    protocols:\n      http:\nprocessors:\n  log_dedup/tvt:\n    interval: 60s\n    conditions:\n")
		for _, c := range conds {
			b.WriteString("      - '" + strings.ReplaceAll(c, "'", "''") + "'\n")
		}
		b.WriteString("exporters:\n  debug:\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      processors: [log_dedup/tvt]\n      exporters: [debug]\n")
		src := b.String()
		return func(path string) ([]byte, error) {
			if path == "config.yaml" {
				return []byte(src), nil
			}
			return nil, errors.New("no such file")
		}
	}
	files := func(src string) Files {
		return func(string) ([]byte, error) { return []byte(src), nil }
	}
	rules := func(h Files) map[string]bool {
		got := map[string]bool{}
		for _, f := range Run(Input{Manifest: m, Base: files(base), Head: h, Now: now, SkipConfirmation: true}).Findings {
			got[f.Rule] = true
		}
		return got
	}
	// Scoped to the reconciler's stream, records excluded: passes.
	ok := `resource.attributes["service.name"] == "reconciler" and attributes["event.name"] != "tool_result"`
	if got := rules(head(ok)); got[RuleCollapse] || got[RuleRecord] {
		t.Errorf("scoped away from readers and records: %v", got)
	}
	// The read stream's service: refused.
	if got := rules(head(`resource.attributes["service.name"] == "coding-agent" and attributes["event.name"] != "tool_result"`)); !got[RuleCollapse] {
		t.Errorf("the read stream's service: %v", got)
	}
	// Scoped to another service but records not excluded: check can't
	// tell the record events' service, so refused as a record collapse.
	if got := rules(head(`resource.attributes["service.name"] == "reconciler"`)); !got[RuleRecord] {
		t.Errorf("records not excluded: %v", got)
	}
	// A second condition that can match the read stream: refused (any
	// condition true collapses).
	if got := rules(head(ok, `IsMatch(body, "timeout")`)); !got[RuleCollapse] {
		t.Errorf("an open second condition: %v", got)
	}
}
