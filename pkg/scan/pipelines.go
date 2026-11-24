package scan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/config"
	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/parse"
)

// Pipeline targets (S6 item 4, ADR 0007): each signal maps to exactly one
// pipeline, the one whose handles match its labels. No match, or several,
// is unmapped: reported with its savings, never proposable, and a gap.
//
// A metric's labels come from Inputs.LabelMetrics (which values of each
// handled label the metric carries). A matcher holds when any of the
// metric's values for its label satisfies it, or "" when the metric has
// none; matchers are checked one by one, not per series, so a metric whose
// series split across pipelines on two labels at once is read as matching
// both (and so unmapped, never silently mapped). A label signal takes its
// metric's pipeline. A stream signal takes the pipeline of every stream it
// selects (the volume API's label sets); streams on different pipelines
// make it unmapped. A recording rule's output belongs to its ruler: never
// unmapped, never a pipeline target.

// Pipeline notes on graph.PipelineTarget.
const (
	PipelineNoMatch    = "no pipeline's handles match"
	PipelineRuleOutput = "a rule's output (a recording rule, or ALERTS and ALERTS_FOR_STATE from alerting rules): changed in the rule, not a pipeline"
)

// SourcePipelines is the pipelines source: the share of priced signals
// mapped to one pipeline. Left out of LogCoverage.
const SourcePipelines = "pipelines"

type compiledPipeline struct {
	name    string
	handles [][]compiledMatcher
}

func compilePipelines(ps []config.Pipeline) []compiledPipeline {
	var out []compiledPipeline
	for _, p := range ps {
		cp := compiledPipeline{name: p.Name}
		for _, h := range p.Handles {
			ms, err := config.StreamMatchers(h)
			if err != nil {
				continue // config validation rejects these
			}
			if cms, ok := compileMatchers(ms); ok {
				cp.handles = append(cp.handles, cms)
			}
		}
		out = append(out, cp)
	}
	return out
}

// HandledLabels are the label names any pipeline's handles match on: the
// labels whose values the scan reads per metric (collect.LabelMetrics).
func HandledLabels(ps []config.Pipeline) []string { return handledLabels(ps) }

func handledLabels(ps []config.Pipeline) []string {
	var out []string
	for _, cp := range compilePipelines(ps) {
		for _, h := range cp.handles {
			for _, m := range h {
				if m.Label != "__name__" && !slices.Contains(out, m.Label) {
					out = append(out, m.Label)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// target names the one pipeline in hits, or why there isn't one.
func target(hits []string) (string, string) {
	switch len(hits) {
	case 0:
		return "", PipelineNoMatch
	case 1:
		return hits[0], ""
	}
	return "", "claimed by several pipelines: " + strings.Join(hits, ", ")
}

// pipelineTargets maps every metric, label and stream signal, and every
// stream the volume API listed, to its pipeline. It returns the targets
// and the pipelines source row.
func pipelineTargets(in Inputs, signals []graph.Signal, consumers []graph.Consumer, costs []graph.Cost) ([]graph.PipelineTarget, graph.SourceCoverage) {
	src := graph.SourceCoverage{Source: SourcePipelines, Configured: len(in.Pipelines) > 0}
	if len(in.Pipelines) == 0 {
		return nil, src
	}
	cps := compilePipelines(in.Pipelines)
	labels := handledLabels(in.Pipelines)
	recorded := map[string]bool{}
	for _, c := range consumers {
		if c.Kind == graph.ConsumerRecordingRule {
			recorded[c.Name] = true
		}
	}
	// metric -> label -> values it carries.
	carried := map[string]map[string][]string{}
	for l, byValue := range in.LabelMetrics {
		for v, names := range byValue {
			for _, m := range names {
				if carried[m] == nil {
					carried[m] = map[string][]string{}
				}
				carried[m][l] = append(carried[m][l], v)
			}
		}
	}
	// Unread: the lookup failed or was cut off. A truncated label's missing
	// values could put a metric's other series on another pipeline, so a
	// partial answer maps nothing rather than map wrongly. A handled label
	// no metric carries (a stream label such as app) is read, with no values.
	unread := ""
	if (in.LabelMetrics == nil || in.LabelMetricsErr != "") && len(labels) > 0 {
		unread = strings.Join(labels, ", ")
	}
	metricHits := func(metric string) []string {
		var hits []string
		for _, cp := range cps {
			for _, h := range cp.handles {
				ok := true
				for _, m := range h {
					if m.Label == "__name__" {
						ok = m.matches(metric)
					} else {
						vals := carried[metric][m.Label]
						if len(vals) == 0 {
							vals = []string{""}
						}
						ok = slices.ContainsFunc(vals, m.matches)
					}
					if !ok {
						break
					}
				}
				if ok {
					hits = append(hits, cp.name)
					break
				}
			}
		}
		return hits
	}
	streamHits := func(ls map[string]string) []string {
		var hits []string
		for _, cp := range cps {
			if slices.ContainsFunc(cp.handles, func(h []compiledMatcher) bool { return matchesAll(h, ls) }) {
				hits = append(hits, cp.name)
			}
		}
		return hits
	}

	var out []graph.PipelineTarget
	metricTarget := map[string]graph.PipelineTarget{}
	metricPrefix := in.MetricsSource + ":metric:"
	for _, s := range signals {
		name, ok := strings.CutPrefix(s.ID, metricPrefix)
		if !ok {
			continue
		}
		t := graph.PipelineTarget{Subject: s.ID, Jobs: in.MetricJobs[name]}
		switch {
		case recorded[name] || name == "ALERTS" || name == "ALERTS_FOR_STATE":
			// ALERTS carries the alerted series' labels (job="shop"), so it
			// would match a pipeline's handles; the ruler writes it.
			t.Note = PipelineRuleOutput
		case unread != "":
			t.Note = fmt.Sprintf("labels %s weren't read (%s)", unread, cmpOr(in.LabelMetricsErr, "no label source"))
		default:
			t.Pipeline, t.Note = target(metricHits(name))
		}
		metricTarget[name] = t
		out = append(out, t)
	}
	labelPrefix := in.MetricsSource + ":label:"
	for _, s := range signals {
		rest, ok := strings.CutPrefix(s.ID, labelPrefix)
		if !ok {
			continue
		}
		metric := rest
		if i := strings.LastIndex(rest, ":"); i >= 0 {
			metric = rest[:i]
		}
		t, ok := metricTarget[metric]
		if !ok {
			t = graph.PipelineTarget{Note: PipelineNoMatch, Jobs: in.MetricJobs[metric]}
			t.Pipeline, t.Note = target(metricHits(metric))
		}
		t.Subject = s.ID
		out = append(out, t)
	}
	streams := streamLabelSets(in.StreamVolumes)
	for i, sv := range in.StreamVolumes {
		var ms []parse.Matcher
		for k, v := range sv.Labels {
			ms = append(ms, parse.Matcher{Label: k, Op: parse.MatchEqual, Value: v})
		}
		t := graph.PipelineTarget{Subject: StreamSubject(parse.CanonicalSelector(ms))}
		t.Pipeline, t.Note = target(streamHits(streams[i]))
		out = append(out, t)
	}
	for _, s := range signals {
		if s.Kind != graph.KindStream {
			continue
		}
		t := graph.PipelineTarget{Subject: s.ID}
		var ms []compiledMatcher
		ok := false
		if s.Name != AllStreams {
			if refs, err := parse.LogQL(s.Name); err == nil && len(refs) == 1 && len(refs[0].Matchers) > 0 {
				ms, ok = compileMatchers(refs[0].Matchers)
			}
		}
		var hits []string
		selected := 0
		for i := range streams {
			if ok && !matchesAll(ms, streams[i]) {
				continue
			}
			selected++
			for _, h := range streamHits(streams[i]) {
				if !slices.Contains(hits, h) {
					hits = append(hits, h)
				}
			}
			if len(streamHits(streams[i])) != 1 {
				hits = append(hits, "") // a selected stream no single pipeline claims
			}
		}
		switch {
		case selected == 0:
			t.Note = "selects no stream the volume API listed"
		case slices.Contains(hits, ""):
			t.Note = "selects streams no single pipeline claims"
			if hs := slices.DeleteFunc(slices.Clone(hits), func(h string) bool { return h == "" }); len(hs) > 0 {
				t.Note += " (others: " + strings.Join(hs, ", ") + ")"
			}
		default:
			slices.Sort(hits)
			t.Pipeline, t.Note = target(hits)
		}
		out = append(out, t)
	}

	held := holdUnfollowable(in, out, metricPrefix, labelPrefix)

	// Coverage: the share of priced metric and stream signals (rule
	// outputs aside) mapped to one pipeline; the gap lists the rest.
	priced := map[string]bool{}
	for _, c := range costs {
		if c.MonthlyCost > 0 || c.BytesPerDay > 0 || c.ActiveSeries > 0 {
			priced[c.SignalID] = true
		}
	}
	var total, mapped int
	var gaps []string
	for _, t := range out {
		if !priced[t.Subject] || t.Note == PipelineRuleOutput || strings.HasPrefix(t.Subject, labelPrefix) {
			continue
		}
		total++
		if t.Pipeline != "" {
			mapped++
			continue
		}
		if len(gaps) < 20 {
			gaps = append(gaps, fmt.Sprintf("%s: unmapped (%s)", t.Subject, t.Note))
		}
	}
	for _, t := range out {
		if strings.HasPrefix(t.Subject, streamSubjectPrefix) && t.Pipeline == "" && len(gaps) < 40 {
			gaps = append(gaps, fmt.Sprintf("stream %s: unmapped (%s)", strings.TrimPrefix(t.Subject, streamSubjectPrefix), t.Note))
		}
	}
	if in.LabelMetricsErr != "" {
		gaps = append(gaps, "pipeline labels: "+in.LabelMetricsErr)
	}
	gaps = append(gaps, held...)
	for _, e := range in.RewriteErrs {
		gaps = append(gaps, "pipeline rewrites: "+e)
	}
	src.Coverage = 1
	if total > 0 {
		src.Coverage = float64(mapped) / float64(total)
	}
	src.Gap = strings.Join(gaps, "\n")
	return out, src
}

// streamSubjectPrefix marks a stream (a full label set from the volume
// API) rather than a signal in graph.PipelineTarget.Subject.
const streamSubjectPrefix = "stream:"

// StreamSubject is the PipelineTarget subject for one stream's label set.
func StreamSubject(selector string) string { return streamSubjectPrefix + selector }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// MetricJobs inverts the job label's values into metric -> jobs (S7 item
// 3), sorted. Nil when the job label wasn't read.
func MetricJobs(lm collect.LabelMetrics) map[string][]string {
	byValue, ok := lm["job"]
	if !ok {
		return nil
	}
	out := map[string][]string{}
	for job, names := range byValue {
		for _, m := range names {
			out[m] = append(out[m], job)
		}
	}
	for m := range out {
		slices.Sort(out[m])
	}
	return out
}

// PipelineUnfollowable starts the note on a metric that passes a pipeline
// rewrite tvt can't follow (S7 item 9): an evidence gap, so the metric is
// unmapped (never proposable) while the rewrite stands.
const PipelineUnfollowable = "passes a rewrite tvt can't follow"

// holdUnfollowable unmaps every metric (and its labels) that passes a
// rewrite tvt can't follow, in place, and returns one gap line per
// rewrite. Renames tvt follows change nothing here: readers already match
// the stored name, and writers add their rules after the rename.
func holdUnfollowable(in Inputs, out []graph.PipelineTarget, metricPrefix, labelPrefix string) []string {
	pipelines := make([]string, 0, len(in.Rewrites))
	for p := range in.Rewrites {
		pipelines = append(pipelines, p)
	}
	slices.Sort(pipelines)
	mapped := map[string]string{} // metric -> its pipeline target
	for _, t := range out {
		if name, ok := strings.CutPrefix(t.Subject, metricPrefix); ok {
			mapped[name] = t.Pipeline
		}
	}
	held := map[string]string{} // metric -> note
	heldStreams := map[string]string{}
	heldLabels := map[string]string{} // label subject -> note
	var gaps []string
	for _, p := range pipelines {
		for _, rw := range in.Rewrites[p] {
			if rw.Unfollowable == "" {
				continue
			}
			if rw.Label {
				note := fmt.Sprintf("%s (%s %s in %s: %s)", PipelineUnfollowable, p, rw.Component, rw.File, rw.Unfollowable)
				n := 0
				for _, t := range out {
					rest, ok := strings.CutPrefix(t.Subject, labelPrefix)
					j := strings.LastIndex(rest, ":")
					if !ok || j < 0 || t.Pipeline != p {
						continue
					}
					metric, label := rest[:j], rest[j+1:]
					hit := rw.All || slices.Contains(rw.Names, metric)
					for _, pre := range rw.Prefixes {
						hit = hit || strings.HasPrefix(metric, pre)
					}
					for _, jb := range in.MetricJobs[metric] {
						hit = hit || slices.Contains(rw.Jobs, jb)
					}
					if !hit || (len(rw.Labels) > 0 && !slices.ContainsFunc(rw.Labels, func(l string) bool { return storedLabel(l) == label })) {
						continue
					}
					n++
					if _, ok := heldLabels[t.Subject]; !ok {
						heldLabels[t.Subject] = note
					}
				}
				gaps = append(gaps, fmt.Sprintf("pipeline %s: %s in %s sets labels tvt doesn't place a removal around (%s): %d labels held, none proposed", p, rw.Component, rw.File, rw.Unfollowable, n))
				continue
			}
			if rw.Streams {
				note := fmt.Sprintf("%s (%s %s in %s: %s)", PipelineUnfollowable, p, rw.Component, rw.File, rw.Unfollowable)
				n := 0
				for _, t := range out {
					isStream := strings.HasPrefix(t.Subject, streamSubjectPrefix) || strings.HasPrefix(t.Subject, "loki:stream:")
					if isStream && t.Pipeline == p {
						if _, ok := heldStreams[t.Subject]; !ok {
							heldStreams[t.Subject] = note
						}
						if strings.HasPrefix(t.Subject, streamSubjectPrefix) {
							n++
						}
					}
				}
				gaps = append(gaps, fmt.Sprintf("pipeline %s: %s in %s can't be followed (%s): %d streams held, none proposed", p, rw.Component, rw.File, rw.Unfollowable, n))
				continue
			}
			note := fmt.Sprintf("%s (%s %s in %s: %s)", PipelineUnfollowable, p, rw.Component, rw.File, rw.Unfollowable)
			n := 0
			for name, pipe := range mapped {
				hit := rw.All && pipe == p || slices.Contains(rw.Names, name)
				for _, pre := range rw.Prefixes {
					hit = hit || strings.HasPrefix(name, pre)
				}
				for _, j := range in.MetricJobs[name] {
					hit = hit || slices.Contains(rw.Jobs, j)
				}
				if !hit {
					continue
				}
				n++
				if _, ok := held[name]; !ok {
					held[name] = note
				}
			}
			gaps = append(gaps, fmt.Sprintf("pipeline %s: %s in %s can't be followed (%s): %d metrics held, none proposed", p, rw.Component, rw.File, rw.Unfollowable, n))
		}
	}
	for i, t := range out {
		if note, ok := heldStreams[t.Subject]; ok {
			out[i].Pipeline, out[i].Note = "", note
			continue
		}
		if note, ok := heldLabels[t.Subject]; ok {
			out[i].Pipeline, out[i].Note = "", note
			continue
		}
		name, ok := strings.CutPrefix(t.Subject, metricPrefix)
		if !ok {
			rest, ok := strings.CutPrefix(t.Subject, labelPrefix)
			if !ok {
				continue
			}
			name = rest
			if j := strings.LastIndex(rest, ":"); j >= 0 {
				name = rest[:j]
			}
		}
		if note, ok := held[name]; ok && t.Note != PipelineRuleOutput {
			out[i].Pipeline, out[i].Note = "", note
		}
	}
	return gaps
}

// storedLabel is the label name a pipeline's label key is stored under:
// OTel attribute keys (deployment.environment) become Prometheus labels
// with every character outside [A-Za-z0-9_] replaced by _, as Prometheus's
// OTLP translation does; Prometheus and Alloy names are unchanged.
func storedLabel(key string) string {
	b := []byte(key)
	for i, c := range b {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '_'
		}
	}
	if len(b) > 0 && b[0] >= '0' && b[0] <= '9' {
		return "key_" + string(b)
	}
	return string(b)
}
