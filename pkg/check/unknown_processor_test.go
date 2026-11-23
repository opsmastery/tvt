package check

import (
	"errors"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// TestUnknownProcessorFailsClosed: a processor type check doesn't model
// could drop or strip a protected signal, so adding one fails closed,
// with or without settings (S8 item 8). Pass-through types still pass.
func TestUnknownProcessorFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	m := report.Manifest{Version: 2, Repo: "r", GeneratedAt: now,
		PipelineDefs: []report.PipelineDef{{Name: "agent", Kind: "otelcol", Files: []string{"config.yaml"}}},
		Signals: []report.ProtectedSignal{{ID: "tempo:span:checkout", Kind: "span",
			Name: "checkout", Pipeline: "agent", Class: "keep_high"}}}
	cfg := func(procs, used string) string {
		s := "receivers:\n  otlp:\n    protocols:\n      http:\n"
		if procs != "" {
			s += "processors:\n" + procs
		}
		s += "exporters:\n  debug:\nservice:\n  pipelines:\n    traces:\n      receivers: [otlp]\n"
		if used != "" {
			s += "      processors: [" + used + "]\n"
		}
		return s + "      exporters: [debug]\n"
	}
	files := func(src string) Files {
		return func(path string) ([]byte, error) {
			if path == "config.yaml" {
				return []byte(src), nil
			}
			return nil, errors.New("no such file")
		}
	}
	base := cfg("", "")
	for _, tc := range []struct {
		name, procs, used string
		fail              bool
	}{
		{"unknown with settings", "  nosuchthing/x:\n    drop: everything\n", "nosuchthing/x", true},
		{"unknown, empty block", "  nosuchthing/x: {}\n", "nosuchthing/x", true},
		{"unknown, no block", "  redaction:\n", "redaction", true},
		{"metricstransform", "  metricstransform:\n    transforms: []\n", "metricstransform", true},
		{"pass-through", "  batch:\n  memory_limiter:\n    limit_mib: 100\n  k8sattributes:\n", "memory_limiter, k8sattributes, batch", false},
	} {
		r := Run(Input{Manifest: m, Base: files(base), Head: files(cfg(tc.procs, tc.used)), Now: now, SkipConfirmation: true})
		got := false
		for _, f := range r.Findings {
			if f.Rule == RuleFailClosed {
				got = true
			}
		}
		if got != tc.fail {
			t.Errorf("%s: fail_closed = %v, want %v (findings %v)", tc.name, got, tc.fail, r.Findings)
		}
		if tc.fail && r.Pass() {
			t.Errorf("%s: check passed", tc.name)
		}
	}
}
