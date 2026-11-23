package scan

import (
	"fmt"
	"strings"

	"github.com/opsmastery/tvt/pkg/collect/kube"
	"github.com/opsmastery/tvt/pkg/graph"
)

// SourceMonitors says which ServiceMonitors and PodMonitors the
// prometheus-operator servers in tvt.yaml's kubernetes.manifests select
// (S5 item 8). A monitor nothing selects scrapes nothing: no series or
// saving is attributed to it, and an edit there changes nothing. Left out
// of LogCoverage; its gaps stay in the report header.
const SourceMonitors = "kubernetes monitors"

// MonitorSelection makes the monitors source. Coverage is the share of
// monitors whose selection tvt can decide (0 when the manifests hold no
// Prometheus or PrometheusAgent). Gap lines: each monitor nothing selects,
// each one tvt can't decide, and each operator scrape pool (pools, from
// /api/v1/targets; nil when unknown) naming a monitor the manifests don't
// show as selected. The summary is for the scan's output.
func MonitorSelection(sel kube.Selection, pools []string) (src graph.SourceCoverage, summary string) {
	src = graph.SourceCoverage{Source: SourceMonitors, Configured: true, Coverage: 1}
	var gaps []string
	not, unsure := sel.NotSelected(), sel.Uncertain()
	switch {
	case len(sel.Servers) == 0:
		src.Coverage = 0
		gaps = append(gaps, fmt.Sprintf("the manifests hold no Prometheus or PrometheusAgent, so which of the %d monitors are scraped is unknown", len(sel.Monitors)))
		summary = fmt.Sprintf("%d monitors, no Prometheus in the manifests", len(sel.Monitors))
	default:
		if len(sel.Monitors) > 0 {
			src.Coverage = float64(len(sel.Monitors)-len(unsure)) / float64(len(sel.Monitors))
		}
		for _, m := range not {
			gaps = append(gaps, fmt.Sprintf("%s %s/%s is selected by no %s: it scrapes nothing, so no series or saving is attributed to it", m.Kind, m.Namespace, m.Name, strings.Join(sel.Servers, ", ")))
		}
		for _, m := range unsure {
			gaps = append(gaps, fmt.Sprintf("%s %s/%s may be selected: %s", m.Kind, m.Namespace, m.Name, strings.Join(m.Unknown, "; ")))
		}
		summary = fmt.Sprintf("%d of %d monitors selected by %d %s", len(sel.Monitors)-len(not)-len(unsure), len(sel.Monitors), len(sel.Servers), pluralWord(len(sel.Servers), "server", "servers"))
		if len(not) > 0 {
			refs := make([]string, len(not))
			for i, m := range not {
				refs[i] = m.Namespace + "/" + m.Name
			}
			summary += ", not selected: " + strings.Join(refs, ", ")
		}
		if len(unsure) > 0 {
			summary += fmt.Sprintf(", %d undecided", len(unsure))
		}
	}
	for _, p := range pools {
		parts := strings.Split(p, "/")
		if len(parts) != 4 || (parts[0] != "serviceMonitor" && parts[0] != "podMonitor") {
			continue
		}
		if _, ok := sel.ForPool(p); !ok {
			gaps = append(gaps, fmt.Sprintf("scrape pool %s names a monitor the manifests don't show as selected (out of date?): its series aren't attributed to a monitor", p))
		}
	}
	src.Gap = strings.Join(gaps, "\n")
	return src, summary
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
