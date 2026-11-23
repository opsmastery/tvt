package check

import (
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/report"
)

// TestLeastLossy (S8 item 3, ADR 0010): a drop of a metric with readers
// names the less lossy action; a stream drop where a cold tier exists
// must be a reroute first. Both log rules also carry the error guard.
func TestLeastLossy(t *testing.T) {
	m := report.Manifest{Version: 2, GeneratedAt: time.Now(),
		PipelineDefs: []report.PipelineDef{
			{Name: "scrape", Kind: "prometheus", Files: []string{"p.yml"}},
			{Name: "fb", Kind: "fluentbit", Files: []string{"fb.yaml"}, Cold: "cold"},
		},
		Read: []report.ReadSignal{{ID: "prom:metric:http_requests_total", Kind: "metric", Name: "http_requests_total", Pipeline: "scrape",
			Labels: []string{"route"}, Readers: 2}}}
	pBase := "scrape_configs:\n  - job_name: app\n"
	pHead := pBase + "    metric_relabel_configs:\n      - source_labels: [__name__]\n        regex: http_requests_total\n        action: drop\n"
	fbBase := "pipeline:\n  inputs:\n    - name: tail\n      tag: app\n"
	fbHead := fbBase + "  filters:\n    - name: grep\n      match: app\n      exclude: log healthz\n"
	r := Run(Input{Manifest: m, Now: time.Now(), SkipConfirmation: true,
		Base: mapFiles(map[string]string{"p.yml": pBase, "fb.yaml": fbBase}),
		Head: mapFiles(map[string]string{"p.yml": pHead, "fb.yaml": fbHead})})
	var metric, stream bool
	for _, f := range r.Findings {
		switch {
		case f.Rule == RuleLeastLossy && strings.Contains(f.Reason, "use only route: remove the other labels"):
			metric = true
		case f.Rule == RuleLeastLossy && strings.Contains(f.Reason, "reroute them to the cold output with rewrite_tag first"):
			stream = true
		}
	}
	if !metric || !stream || rulesOf(r)[RuleErrorGuard] != 1 {
		t.Errorf("least_lossy metric %v stream %v, error guard %d:\n%v", metric, stream, rulesOf(r)[RuleErrorGuard], r.Findings)
	}
}
