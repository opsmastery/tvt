package check

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/report"
)

// TestScrapeLimits: a scrape interval past the longest its readers allow
// (the report's intervals, S10 item 5) is refused; up to it passes.
func TestScrapeLimits(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	m := report.Manifest{Version: 2, Repo: "r", GeneratedAt: now,
		PipelineDefs: []report.PipelineDef{{Name: "prom", Kind: "prometheus", Files: []string{"prom.yml"}},
			{Name: "alloy", Kind: "alloy", Files: []string{"config.alloy"}}},
		ScrapeLimits: []report.ScrapeLimit{
			{Job: "loki", MaxSeconds: 120, Why: "a reader without a range keeps two samples in the 5m lookback at 2m"},
			{Job: "shop", MaxSeconds: 10, Why: "Shop Overview / Cart's window is unknown"},
		}}
	prom := func(iv string) string {
		return "scrape_configs:\n  - job_name: loki\n    scrape_interval: " + iv + "\n    static_configs:\n      - targets: [loki:3100]\n"
	}
	alloy := func(iv string) string {
		return "prometheus.scrape \"shop\" {\n  targets         = [{\"__address__\" = \"shop:8080\"}]\n  job_name        = \"shop\"\n  scrape_interval = \"" + iv + "\"\n  forward_to      = []\n}\n"
	}
	files := func(p, a string) Files {
		return func(path string) ([]byte, error) {
			switch path {
			case "prom.yml":
				return []byte(p), nil
			case "config.alloy":
				return []byte(a), nil
			}
			return nil, errors.New("no such file")
		}
	}
	base := files(prom("10s"), alloy("10s"))
	run := func(head Files) []Finding {
		r := Run(Input{Manifest: m, Base: base, Head: head, Now: now, SkipConfirmation: true})
		var out []Finding
		for _, f := range r.Findings {
			if f.Rule == RuleScrape {
				out = append(out, f)
			}
		}
		return out
	}
	if got := run(files(prom("2m"), alloy("10s"))); len(got) != 0 {
		t.Errorf("2m on loki, its limit: %v", got)
	}
	got := run(files(prom("5m"), alloy("30s")))
	if len(got) != 2 {
		t.Fatalf("5m on loki and 30s on shop: %v", got)
	}
	s := got[0].String() + got[1].String()
	for _, want := range []string{"job loki scraped every 5m, past the 2m", "job shop scraped every 30s", "window is unknown"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q: %s", want, s)
		}
	}
}
