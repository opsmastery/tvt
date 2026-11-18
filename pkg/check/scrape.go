package check

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/grafana/alloy/syntax/ast"
	"github.com/grafana/alloy/syntax/parser"
	"github.com/prometheus/common/model"
)

// scrapeEffects judges scrape intervals against the manifest's alerts (S8
// item 5). For each alert whose metrics come through a scrape config the
// repo defines, at this version's interval: a range window that holds
// fewer than four samples, a nonzero for shorter than the interval, and a
// worst-case detection delay past the severity's objective are effects.
// Run compares them with Base's, so only a change that causes one is
// refused.
func scrapeEffects(m report.Manifest, ix *index, files Files, objectives map[string]time.Duration) []effect {
	if len(m.Alerts) == 0 {
		return nil
	}
	intervals, where := scrapeIntervals(m, files)
	if len(intervals) == 0 {
		return nil
	}
	if objectives == nil {
		objectives = map[string]time.Duration{}
		for sev, s := range m.Detection.ObjectivesSec {
			objectives[sev] = time.Duration(s * float64(time.Second))
		}
	}
	gw := time.Duration(m.Detection.GroupWaitSec * float64(time.Second))
	var out []effect
	for _, a := range m.Alerts {
		if a.Logs {
			continue
		}
		iv, job := 0.0, ""
		for _, j := range a.Jobs {
			if v, ok := intervals[j]; ok && v > iv {
				iv, job = v, j
			}
		}
		if iv == 0 {
			continue
		}
		e := effect{rule: RuleScrape, pipeline: where[job].pipeline, file: where[job].file, component: "job " + job, signal: a.Name, at: where[job].at}
		windows := append([]float64{}, a.RangesSec...)
		for _, r := range a.Recorded {
			windows = append(windows, r.RangesSec...)
		}
		seen := map[float64]bool{}
		for _, w := range windows {
			if seen[w] || w <= 0 {
				continue
			}
			seen[w] = true
			if n := math.Floor(w / iv); n < 4 {
				x := e
				x.reason = fmt.Sprintf("%s: a %s window holds %d samples at the %s scrape interval of job %s (fewer than four, so rate and increase go blank or jump)", a.Name, secs(w), int(n), secs(iv), job)
				out = append(out, x)
			}
		}
		if a.ForSec > 0 && a.ForSec < iv {
			x := e
			x.reason = fmt.Sprintf("%s: its for (%s) is shorter than the %s scrape interval of job %s, so it can fire on one sample", a.Name, secs(a.ForSec), secs(iv), job)
			out = append(out, x)
		}
		if obj, ok := objectives[a.Severity]; ok {
			if d, terms := a.Delay(iv, gw); d > obj {
				x := e
				x.reason = fmt.Sprintf("%s (%s): worst-case detection delay past the %s objective", a.Name, a.Severity, model.Duration(obj))
				x.detail = fmt.Sprintf("%s: %s", model.Duration(d), terms)
				out = append(out, x)
			}
		}
	}
	return out
}

func secs(s float64) string { return model.Duration(time.Duration(s * float64(time.Second))).String() }

type place struct{ pipeline, file, at string }

// scrapeIntervals: every job's scrape interval in this version of the
// repo, from Prometheus scrape configs (a job's own, else the file's
// global, else Prometheus' 1m default) and Alloy prometheus.scrape
// components (default 1m).
func scrapeIntervals(m report.Manifest, files Files) (map[string]float64, map[string]place) {
	out := map[string]float64{}
	where := map[string]place{}
	for _, p := range m.PipelineDefs {
		for _, path := range p.Files {
			src, err := files(path)
			if err != nil {
				continue
			}
			switch p.Kind {
			case "prometheus":
				root, err := yamlRoot(src)
				if err != nil {
					continue
				}
				global := "1m"
				if g := scalar(field(root, "global"), "scrape_interval"); g != "" {
					global = g
				}
				for _, sc := range scrapeConfigs(root) {
					v := scalar(sc, "scrape_interval")
					if v == "" {
						v = global
					}
					if d, err := model.ParseDuration(v); err == nil {
						job := scalar(sc, "job_name")
						out[job] = time.Duration(d).Seconds()
						where[job] = place{p.Name, path, fmt.Sprintf("line %d", sc.Line)}
					}
				}
			case "alloy":
				f, err := parser.ParseFile(path, src)
				if err != nil {
					continue
				}
				for _, st := range f.Body {
					b, ok := st.(*ast.BlockStmt)
					if !ok || strings.Join(b.Name, ".") != "prometheus.scrape" {
						continue
					}
					job := "prometheus.scrape." + b.Label
					if e, ok := attrOf(b.Body, "job_name"); ok {
						if v, ok := literal(e); ok {
							job = v
						}
					}
					iv := "1m"
					if e, ok := attrOf(b.Body, "scrape_interval"); ok {
						if v, ok := literal(e); ok {
							iv = v
						}
					}
					if d, err := model.ParseDuration(iv); err == nil {
						out[job] = time.Duration(d).Seconds()
						where[job] = place{p.Name, path, fmt.Sprintf("line %d", ast.StartPos(b).Position().Line)}
					}
				}
			}
		}
	}
	return out, where
}
