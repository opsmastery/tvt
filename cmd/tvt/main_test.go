package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string // substring of stdout+stderr
	}{
		{"no args", nil, exitUsage, "Usage:"},
		{"help", []string{"help"}, exitOK, "Commands:"},
		{"unknown", []string{"frobnicate"}, exitUsage, `unknown command "frobnicate"`},
		{"version", []string{"version"}, exitOK, "tvt dev"},
		{"check stub", []string{"check"}, exitNotImplemented, "not implemented"},
		{"scan missing grafana", []string{"scan", "--prom", "http://p:9090"}, exitUsage, "--grafana is required"},
		{"scan missing metrics", []string{"scan", "--grafana", "http://g:3000"}, exitUsage, "--prom or --mimir"},
		{"scan both metrics", []string{"scan", "--grafana", "http://g:3000", "--prom", "http://p", "--mimir", "http://m"}, exitUsage, "mutually exclusive"},
		{"scan bad scheme", []string{"scan", "--grafana", "ftp://g", "--prom", "http://p"}, exitUsage, "scheme must be http"},
		{"scan bad window", []string{"scan", "--grafana", "http://g", "--prom", "http://p", "--window", "0"}, exitUsage, "--window"},
		{"scan valid", []string{"scan", "--grafana", "http://g:3000", "--prom", "http://p:9090", "--loki", "http://l:3100"}, exitNotImplemented, "scan plan: grafana=http://g:3000 metrics=http://p:9090 loki=http://l:3100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(tt.args, &out, &errOut)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, errOut.String())
			}
			if all := out.String() + errOut.String(); !strings.Contains(all, tt.wantOut) {
				t.Errorf("output missing %q:\n%s", tt.wantOut, all)
			}
		})
	}
}
