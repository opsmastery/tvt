package report

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/prometheus/common/model"
)

// DefaultGroupWait is Alertmanager's default group_wait: the last term of
// an alert's detection delay when tvt.yaml doesn't give one.
const DefaultGroupWait = 30 * time.Second

// AlertTiming is what an alert's worst-case detection delay depends on
// (S8 item 5): its own range windows, interval and pending period, the
// recording rules it reads, and the scrape interval of the metrics under
// them. The manifest carries it, so tvt check can recompute the delay for
// a changed scrape interval offline.
type AlertTiming struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Source      string    `json:"source"`
	Severity    string    `json:"severity,omitempty"`
	ForSec      float64   `json:"for_seconds"`
	IntervalSec float64   `json:"interval_seconds"`
	RangesSec   []float64 `json:"ranges_seconds,omitempty"`
	// Recorded are the recording rules the alert reads (one level).
	Recorded []RecordedTiming `json:"recorded,omitempty"`
	// Metrics are the stored metrics under the alert (through Recorded),
	// and Jobs their job label values: the scrape configs that set the
	// first term.
	Metrics []string `json:"metrics,omitempty"`
	Jobs    []string `json:"jobs,omitempty"`
	// ScrapeSec is the longest scrape interval of Metrics at the scan; 0
	// when unknown, or for pushed data (Logs).
	ScrapeSec float64 `json:"scrape_seconds"`
	// Logs: a LogQL alert; lines are pushed, with no scrape interval.
	Logs bool `json:"logs,omitempty"`
}

// RecordedTiming is a recording rule under an alert.
type RecordedTiming struct {
	Name        string    `json:"name"`
	IntervalSec float64   `json:"interval_seconds"`
	RangesSec   []float64 `json:"ranges_seconds,omitempty"`
}

// rangeRe finds range selectors ([5m], [1h:1m]) in PromQL or LogQL.
var rangeRe = regexp.MustCompile(`\[\s*([0-9]+(?:ms|s|m|h|d|w|y)(?:[0-9]+(?:ms|s|m|h|d|w|y))*)\s*(?::[^\]]*)?\]`)

// Ranges are the range windows in an expression, in seconds.
func Ranges(expr string) []float64 {
	var out []float64
	for _, m := range rangeRe.FindAllStringSubmatch(expr, -1) {
		d, err := model.ParseDuration(m[1])
		if err != nil {
			continue
		}
		out = append(out, time.Duration(d).Seconds())
	}
	return out
}

func maxOf(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}

// Delay is the alert's worst-case detection delay at a scrape interval
// (seconds; 0 for pushed data), with its terms for the reader:
// scrape + longest range + a recording rule's interval when it reads one
// + its own interval + for + group_wait.
func (a AlertTiming) Delay(scrapeSec float64, groupWait time.Duration) (time.Duration, string) {
	rng := maxOf(a.RangesSec)
	rec := 0.0
	for _, r := range a.Recorded {
		if m := maxOf(r.RangesSec); m > rng {
			rng = m
		}
		rec = max(rec, r.IntervalSec)
	}
	total := scrapeSec + rng + rec + a.IntervalSec + a.ForSec + groupWait.Seconds()
	terms := []string{dur(scrapeSec) + " scrape", dur(rng) + " range"}
	if rec > 0 {
		terms = append(terms, dur(rec)+" recording rule")
	}
	terms = append(terms, dur(a.IntervalSec)+" interval", dur(a.ForSec)+" for", dur(groupWait.Seconds())+" group_wait")
	return time.Duration(total * float64(time.Second)), strings.Join(terms, " + ")
}

func dur(sec float64) string {
	if sec == 0 {
		return "0"
	}
	return model.Duration(time.Duration(sec * float64(time.Second))).String()
}

// AlertTimings lists every alert's timing from a stored scan, by name.
func AlertTimings(in Input) []AlertTiming {
	byID := map[string]graph.Consumer{}
	recByName := map[string]graph.Consumer{}
	for _, c := range in.Consumers {
		byID[c.ID] = c
		if c.Kind == graph.ConsumerRecordingRule {
			recByName[c.Name] = c
		}
	}
	signals := map[string]graph.Signal{}
	for _, s := range in.Signals {
		signals[s.ID] = s
	}
	reads := map[string][]string{} // consumer -> metric signal IDs
	for _, r := range in.References {
		if s, ok := signals[r.SignalID]; ok && s.Kind == graph.KindMetric {
			reads[r.ConsumerID] = append(reads[r.ConsumerID], r.SignalID)
		}
	}
	jobs := map[string][]string{}
	for _, t := range in.PipelineTargets {
		jobs[t.Subject] = t.Jobs
	}
	scrape := map[string]float64{}
	for _, c := range in.Costs {
		if c.Interval > 0 && c.IntervalAssumed == "" {
			scrape[c.SignalID] = max(scrape[c.SignalID], c.Interval.Seconds())
		}
	}
	var out []AlertTiming
	for _, c := range in.Consumers {
		if c.Kind != graph.ConsumerAlertRule {
			continue
		}
		src, _, _ := strings.Cut(strings.TrimPrefix(c.ID, "rule:"), ":")
		a := AlertTiming{ID: c.ID, Name: c.Name, Source: src, Severity: c.Severity, ForSec: c.For.Seconds(),
			IntervalSec: c.Interval.Seconds(), RangesSec: Ranges(c.Expr), Logs: src == "loki" || looksLogQL(c.Expr)}
		metrics := map[string]bool{}
		js := map[string]bool{}
		for _, sid := range reads[c.ID] {
			s := signals[sid]
			if r, ok := recByName[s.Name]; ok {
				a.Recorded = append(a.Recorded, RecordedTiming{Name: r.Name, IntervalSec: r.Interval.Seconds(), RangesSec: Ranges(r.Expr)})
				for _, base := range reads[r.ID] {
					metrics[signals[base].Name] = true
					for _, j := range jobs[base] {
						js[j] = true
					}
					a.ScrapeSec = max(a.ScrapeSec, scrape[base])
				}
				continue
			}
			metrics[s.Name] = true
			for _, j := range jobs[sid] {
				js[j] = true
			}
			a.ScrapeSec = max(a.ScrapeSec, scrape[sid])
		}
		for m := range metrics {
			a.Metrics = append(a.Metrics, m)
		}
		for j := range js {
			a.Jobs = append(a.Jobs, j)
		}
		sort.Strings(a.Metrics)
		sort.Strings(a.Jobs)
		sort.Slice(a.Recorded, func(i, j int) bool { return a.Recorded[i].Name < a.Recorded[j].Name })
		if a.Logs {
			a.ScrapeSec = 0
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// looksLogQL: a stream selector with a line filter or a log range
// function, the way a LogQL rule reads.
func looksLogQL(expr string) bool {
	for _, k := range []string{"count_over_time({", "rate({", "bytes_over_time({", "|=", "|~", "| logfmt", "| json"} {
		if strings.Contains(expr, k) {
			return true
		}
	}
	return false
}

// DetectionLine is one alert's delay for the report.
func DetectionLine(a AlertTiming, groupWait time.Duration, objectives map[string]time.Duration) (delay time.Duration, terms, finding string) {
	delay, terms = a.Delay(a.ScrapeSec, groupWait)
	if obj, ok := objectives[a.Severity]; ok && delay > obj {
		finding = fmt.Sprintf("worst-case detection delay %s is past the %s objective %s (%s)", model.Duration(delay), a.Severity, model.Duration(obj), terms)
	}
	return
}

// RulePastObjective is the rule finding for an alert whose worst-case
// detection delay is past its severity's objective.
const RulePastObjective = "past_detection_objective"

// detection fills in each alert row's severity, for and delay, and adds a
// finding when the delay is past the objective. It returns how many it
// added.
func detection(in Input, rows []Rule) int {
	gw := in.GroupWait
	if gw == 0 {
		gw = DefaultGroupWait
	}
	timings := map[string]AlertTiming{}
	for _, a := range AlertTimings(in) {
		timings[a.ID] = a
	}
	n := 0
	for i := range rows {
		a, ok := timings[rows[i].ID]
		if !ok {
			continue
		}
		d, terms, finding := DetectionLine(a, gw, in.Objectives)
		rows[i].Severity, rows[i].ForSec, rows[i].DetectionDelaySec, rows[i].DetectionTerms = a.Severity, a.ForSec, d.Seconds(), terms
		if finding != "" {
			rows[i].Findings = append(rows[i].Findings, RuleFinding{Kind: RulePastObjective, Detail: finding,
				Fix: "shorten the alert's range window or for, or scrape its metrics more often; for a multiwindow burn-rate alert, its short window decides"})
			n++
		}
	}
	return n
}
