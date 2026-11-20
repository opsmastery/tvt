package check

import (
	"errors"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
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
