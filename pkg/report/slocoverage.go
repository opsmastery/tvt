package report

import "github.com/Opsmastery-Summit/tvt/pkg/graph"

// SLOCoverage is the SLO coverage report (S6 item 3, version 8): services
// with no SLO, paging alerts that aren't burn-rate alerts, and SLOs with
// no alert. Report only. Services are tvt.yaml ownership.owners names;
// with none configured, services_without_slo is empty and the slo
// definitions source's gap says why.
type SLOCoverage struct {
	ServicesWithoutSLO []SLOFinding `json:"services_without_slo"`
	PagingNotBurnRate  []SLOFinding `json:"paging_not_burn_rate"`
	SLOsWithoutAlert   []SLOFinding `json:"slos_without_alert"`
}

// SLOFinding is one SLO coverage finding. ID is the alert's or SLO's
// consumer ID, or service:<owner>.
type SLOFinding struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

func sloCoverage(findings []graph.SLOFinding) SLOCoverage {
	c := SLOCoverage{ServicesWithoutSLO: []SLOFinding{}, PagingNotBurnRate: []SLOFinding{}, SLOsWithoutAlert: []SLOFinding{}}
	for _, f := range findings {
		row := SLOFinding{ID: f.Subject, Name: f.Name, Detail: f.Detail, Fix: f.Fix}
		switch f.Kind {
		case graph.SLOServiceWithout:
			c.ServicesWithoutSLO = append(c.ServicesWithoutSLO, row)
		case graph.SLOPagingNotBurnRate:
			c.PagingNotBurnRate = append(c.PagingNotBurnRate, row)
		case graph.SLOWithoutAlert:
			c.SLOsWithoutAlert = append(c.SLOsWithoutAlert, row)
		}
	}
	return c
}
