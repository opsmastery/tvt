package scan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/graph"
)

// Overflow series (S6 item 5): an OpenTelemetry SDK or Collector past its
// cardinality limit folds new attribute sets into one series per metric
// with otel.metric.overflow="true" and none of the capped attributes. The
// backend then shows fewer series than the metric really has (its cost is a
// floor) and a reader that filters or groups on a capped label silently
// misses or misplaces the points that overflowed. An evidence gap, report
// only.

// SourceOverflow is the overflow source: coverage 1 when overflow series
// were looked for, a gap line per metric that has them. Left out of
// LogCoverage.
const SourceOverflow = "overflow series"

// overflowFindings returns a finding per metric with overflow series, with
// the readers that filter or group on a capped label, and the source row.
func overflowFindings(in Inputs, refs []graph.Reference) ([]graph.OverflowFinding, graph.SourceCoverage) {
	src := graph.SourceCoverage{Source: SourceOverflow, Configured: in.OverflowRead || in.OverflowErr != ""}
	if in.OverflowErr != "" {
		src.Gap = "overflow series weren't read: " + in.OverflowErr
		return nil, src
	}
	if !in.OverflowRead {
		return nil, src
	}
	src.Coverage = 1
	metricPrefix := in.MetricsSource + ":metric:"
	labelPrefix := in.MetricsSource + ":label:"
	var out []graph.OverflowFinding
	var gaps []string
	for _, o := range in.Overflow {
		capped := cappedLabels(o)
		id := metricPrefix + o.Metric
		uses := map[string][]string{} // consumer -> capped labels it filters or groups on
		for _, r := range refs {
			if r.Sweep {
				continue
			}
			on := ""
			switch {
			case r.SignalID == id:
			case strings.HasPrefix(r.SignalID, labelPrefix+o.Metric+":"):
				on = strings.TrimPrefix(r.SignalID, labelPrefix+o.Metric+":")
			default:
				continue
			}
			for _, l := range r.Labels {
				if l.Use != graph.LabelMatch && l.Use != graph.LabelCoarse && l.Use != graph.LabelGroup && l.Use != graph.LabelFilter {
					continue
				}
				if on != "" && l.Name != on {
					continue
				}
				if slices.Contains(capped, l.Name) && !slices.Contains(uses[r.ConsumerID], l.Name) {
					uses[r.ConsumerID] = append(uses[r.ConsumerID], l.Name)
				}
			}
		}
		f := graph.OverflowFinding{Signal: id, Series: o.Series, Capped: capped}
		for _, c := range slices.Sorted(mapKeys(uses)) {
			ls := uses[c]
			slices.Sort(ls)
			f.Readers = append(f.Readers, c+"\t"+strings.Join(ls, ","))
		}
		out = append(out, f)
		line := fmt.Sprintf("%s: %d overflow series (otel.metric.overflow): real cardinality is higher than shown, cost is a floor; capped labels %s",
			o.Metric, o.Series, cmpOr(strings.Join(capped, ", "), "unknown"))
		if len(f.Readers) > 0 {
			line += fmt.Sprintf("; %d reader(s) filter or group on them and miss the overflowed points", len(f.Readers))
		}
		gaps = append(gaps, line)
	}
	src.Gap = strings.Join(gaps, "\n")
	return out, src
}

// cappedLabels are the labels the metric's other series carry that its
// overflow series lost; the histogram bound le is never capped.
func cappedLabels(o collect.Overflow) []string {
	var out []string
	for _, l := range o.Labels {
		if l != "le" && !slices.Contains(o.Kept, l) {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
