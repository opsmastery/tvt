package scan

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// recordName finds records by name (S7 item 2, ADR 0011): an audit trail,
// deploy events or change events, as a token of a metric name or a stream
// label value. Agent tool-call spans are records too, but tvt has no span
// signals yet; the protection manifest and check cover them by attribute.
var recordName = regexp.MustCompile(`(?i)(^|[^a-z])(audit|deploy(ment)?_?events?|change_?events?)([^a-z]|$)`)

// markRecords sets Record on every signal that is a record, or whose
// streams include one: tvt never drops, samples or collapses a record,
// whatever its readers, so a stream signal selecting any record stream is
// protected as a whole. labelSets are the streams the volume API listed.
// Record says why: declared in tvt.yaml records:, named like one, or
// selecting such a stream.
func markRecords(signals []graph.Signal, rc config.Records, labelSets []map[string]string) error {
	var metricRes []*regexp.Regexp
	for i, expr := range rc.Metrics {
		re, err := config.MetricRegexp(expr)
		if err != nil {
			return fmt.Errorf("scan: records.metrics[%d]: %w", i, err)
		}
		metricRes = append(metricRes, re)
	}
	var streamSels [][]parse.Matcher
	for i, sel := range rc.Streams {
		ms, err := config.StreamMatchers(sel)
		if err != nil {
			return fmt.Errorf("scan: records.streams[%d]: %w", i, err)
		}
		streamSels = append(streamSels, ms)
	}
	// streamRecord says why a stream (a full label set) is a record.
	streamRecord := func(ls map[string]string) string {
		for i, sel := range streamSels {
			if ok, _ := parse.MatchesLabels(sel, ls); ok {
				return fmt.Sprintf("declared (records.streams %s)", rc.Streams[i])
			}
		}
		keys := make([]string, 0, len(ls))
		for k := range ls {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if recordName.MatchString(ls[k]) {
				return fmt.Sprintf("named like a record (%s=%q)", k, ls[k])
			}
		}
		return ""
	}
	for i := range signals {
		s := &signals[i]
		switch s.Kind {
		case graph.KindMetric, graph.KindLabel:
			name := s.Name
			if s.Kind == graph.KindLabel {
				name = s.Parent
			}
			for j, re := range metricRes {
				if re.MatchString(name) {
					s.Record = fmt.Sprintf("declared (records.metrics %s)", rc.Metrics[j])
					break
				}
			}
			if s.Record == "" && recordName.MatchString(name) {
				s.Record = "named like a record"
			}
		case graph.KindStream:
			if s.Name == AllStreams {
				for _, ls := range labelSets {
					if why := streamRecord(ls); why != "" {
						s.Record = "selects a record stream: " + why
						break
					}
				}
				continue
			}
			refs, err := parse.LogQL(s.Name)
			if err != nil || len(refs) != 1 {
				continue
			}
			ms := refs[0].Matchers
			// The selector's own equality matchers: a stream it selects
			// carries these values, listed or not.
			eq := map[string]string{}
			for _, m := range ms {
				if m.Op == parse.MatchEqual {
					eq[m.Label] = m.Value
				}
			}
			for j, sel := range streamSels {
				if selectorImplies(ms, sel) {
					s.Record = fmt.Sprintf("declared (records.streams %s)", rc.Streams[j])
					break
				}
			}
			if s.Record == "" {
				if why := streamRecord(eq); why != "" {
					s.Record = why
				}
			}
			if s.Record != "" {
				continue
			}
			for _, ls := range labelSets {
				if ok, _ := parse.MatchesLabels(ms, ls); !ok {
					continue
				}
				if why := streamRecord(ls); why != "" {
					s.Record = "selects a record stream: " + why
					break
				}
			}
		}
	}
	return nil
}
