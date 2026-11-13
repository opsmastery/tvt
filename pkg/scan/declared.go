package scan

import (
	"fmt"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// SLOs and runbooks are declared dependencies (Sprint 2 item 7): an SLO's
// SLI and a runbook step that runs a query are evaluated by definition,
// whether or not a log ever shows them. Both consumer kinds are hard keeps.
//
//	slo:<format>:<origin>:<name>   an SLO (origin: the file it was read from)
//	runbook:<link>                 a runbook link from alert annotations

// sloConsumers adds one consumer per SLO and its SLI queries. It returns a
// gap line per SLO whose SLI tvt can't read.
func sloConsumers(slos []collect.SLO, ids *idSet, out *Output, queries *[]query) []string {
	var gaps []string
	for _, s := range slos {
		var exprs []string
		for _, q := range s.Queries {
			exprs = append(exprs, q.Expr)
		}
		c := graph.Consumer{
			ID:   ids.unique("slo:" + s.Format + ":" + s.Origin + ":" + s.Name),
			Kind: graph.ConsumerSLO,
			Name: s.Name,
			Expr: strings.Join(exprs, "\n"),
		}
		out.Consumers = append(out.Consumers, c)
		for _, q := range s.Queries {
			lang := "promql"
			if q.Language == "logql" {
				lang = "logql"
			}
			*queries = append(*queries, query{consumer: c, expr: q.Expr, lang: lang})
		}
		if s.Unreadable != "" {
			gaps = append(gaps, fmt.Sprintf("SLO %s (%s): %s", s.Name, s.Origin, s.Unreadable))
		}
	}
	return gaps
}

// runbookConsumers adds one consumer per runbook link and the queries its
// steps run. Code steps resolve only against collected metrics: a block
// that happens to parse as PromQL mustn't invent a signal. A dashboard
// step reads what the dashboard's panels and query variables read.
//
// It returns gap lines and how many links couldn't be fully read (fetch
// failed, a dashboard tvt didn't collect, an Explore query in no language
// tvt reads).
func runbookConsumers(rbs []collect.Runbook, dashboards []collect.Dashboard, metrics []string, ids *idSet, out *Output, queries *[]query) ([]string, int) {
	byUID := make(map[string]collect.Dashboard, len(dashboards))
	for _, d := range dashboards {
		byUID[d.UID] = d
	}
	var gaps []string
	bad := 0
	for _, rb := range rbs {
		alerts := strings.Join(rb.Alerts, ", ")
		if rb.Err != "" {
			gaps = append(gaps, fmt.Sprintf("runbook %s (%s): %s", rb.URL, alerts, rb.Err))
			bad++
			continue
		}
		c := graph.Consumer{
			ID:   ids.unique("runbook:" + rb.URL),
			Kind: graph.ConsumerRunbook,
			Name: "runbook " + rb.URL,
		}
		if alerts != "" {
			c.Name += " (" + alerts + ")"
		}
		var exprs, missing []string
		var steps []query
		for _, st := range rb.Steps {
			switch st.Kind {
			case parse.StepDashboard:
				exprs = append(exprs, "dashboard "+st.DashboardUID)
				d, ok := byUID[st.DashboardUID]
				if !ok {
					missing = append(missing, "dashboard "+st.DashboardUID+" wasn't collected")
					continue
				}
				vars := dashboardVars(d, metrics)
				for _, p := range d.Panels {
					for _, q := range p.Queries {
						if lang, ok := queryLanguage(q.DatasourceType); ok {
							steps = append(steps, query{expr: q.Expr, lang: lang, vars: vars})
						}
					}
				}
				for _, v := range d.Variables {
					if v.Type != "query" && v.Type != "" {
						continue
					}
					if lang, ok := queryLanguage(v.DatasourceType); ok {
						steps = append(steps, query{expr: v.Query, lang: lang, variable: true, vars: vars})
					}
				}
			default:
				exprs = append(exprs, st.Expr)
				if st.Language == "" {
					missing = append(missing, fmt.Sprintf("%s query %q is in no language tvt reads", st.Kind, st.Expr))
					continue
				}
				steps = append(steps, query{expr: st.Expr, lang: st.Language, collectedOnly: st.Kind == parse.StepCode})
			}
		}
		c.Expr = strings.Join(exprs, "\n")
		out.Consumers = append(out.Consumers, c)
		for _, q := range steps {
			q.consumer = c
			*queries = append(*queries, q)
		}
		if len(missing) > 0 {
			gaps = append(gaps, fmt.Sprintf("runbook %s (%s): %s", rb.URL, alerts, strings.Join(missing, "; ")))
			bad++
		}
	}
	return gaps, bad
}

// maxGapLines caps how many unreadable definitions a gap lists by name.
const maxGapLines = 5

// definitionSource is the coverage row for SLO definitions or runbooks:
// the share of them tvt could read, with the ones it couldn't as the gap.
// Configured with nothing found is full coverage (tvt looked and there
// was nothing to read); not configured is 0.
func definitionSource(name string, configured bool, total, bad int, gaps []string) graph.SourceCoverage {
	s := graph.SourceCoverage{Source: name, Configured: configured}
	if !configured {
		return s
	}
	s.Coverage = 1
	if total > 0 {
		s.Coverage = float64(total-bad) / float64(total)
	}
	if len(gaps) > maxGapLines {
		gaps = append(gaps[:maxGapLines:maxGapLines], fmt.Sprintf("and %d more", len(gaps)-maxGapLines))
	}
	s.Gap = strings.Join(gaps, "\n")
	return s
}
