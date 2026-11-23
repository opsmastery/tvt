package scan

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/graph"
)

// SLO coverage (S6 item 3): services with no SLO, paging alerts that
// aren't burn-rate alerts, and SLOs with no alert. Report only.
//
// A service is a tvt.yaml ownership.owners name. It has an SLO when an
// SLO definition names it as its service, or an SLO's SLI reads a signal
// it owns. A paging alert is an alert rule whose severity label (or
// priority) is one of PagingSeverities, or that carries page="true".

// PagingSeverities are the severity (or priority) label values that page.
var PagingSeverities = []string{"critical", "page", "paging", "emergency", "p1", "sev1", "p0", "sev0"}

// sloOutputRe matches the recorded series SLO tools generate and their
// alerts read: Sloth's slo:sli_error:ratio_rate5m and slo:error_budget:…,
// Pyrra's <metric>:burnrate5m, and hand-named burn_rate/error_budget
// series.
var sloOutputRe = regexp.MustCompile(`(?i)(burn_?rate|sli_error|error_budget)`)

// sloLabels mark a rule an SLO tool generated (Sloth) or tied to an SLO.
var sloLabels = []string{"sloth_slo", "sloth_id", "sloth_service", "slo"}

// paging reports whether an alert pages.
func paging(r collect.Rule) bool {
	if r.Kind != collect.RuleAlert {
		return false
	}
	for _, k := range []string{"severity", "priority"} {
		if slices.Contains(PagingSeverities, strings.ToLower(strings.TrimSpace(r.Labels[k]))) {
			return true
		}
	}
	return strings.EqualFold(r.Labels["page"], "true") || strings.EqualFold(r.Labels["paging"], "true")
}

// sloRule reports whether a rule is part of an SLO's alerting: an alert an
// SLO definition names, a rule carrying an SLO tool's labels, one that
// reads an SLO tool's recorded series, or a multiwindow burn-rate alert
// (the same ratio compared over two windows, joined with and). Such rules
// need long windows, so they're never long_range_window findings.
func sloRule(r collect.Rule, slos []collect.SLO) bool {
	for _, s := range slos {
		if s.Alert != "" && r.Kind == collect.RuleAlert && r.Name == s.Alert {
			return true
		}
	}
	for _, k := range sloLabels {
		if r.Labels[k] != "" {
			return true
		}
	}
	text := r.Expr
	for _, q := range r.Queries {
		text += "\n" + q.Expr
	}
	if sloOutputRe.MatchString(text) || (r.Kind == collect.RuleRecord && sloOutputRe.MatchString(r.Name)) {
		return true
	}
	return r.Kind == collect.RuleAlert && multiwindow(r.Expr)
}

// windowRe matches a range or subquery window, for comparing the two
// sides of a multiwindow alert with their windows taken out.
var windowRe = regexp.MustCompile(`\[[0-9a-zA-Z_$:]+\]`)

// multiwindow reports whether expr is `A and B` (optionally with on() or
// ignoring()) at the top level where A and B are the same expression over
// different windows: a multiwindow burn-rate alert.
func multiwindow(expr string) bool {
	sides := splitAnd(squash(expr))
	if len(sides) < 2 {
		return false
	}
	norm := func(s string) string { return squash(windowRe.ReplaceAllString(s, "[_]")) }
	first := norm(sides[0])
	windows := map[string]bool{}
	for _, s := range sides {
		if norm(s) != first {
			return false
		}
		windows[strings.Join(windowRe.FindAllString(s, -1), ",")] = true
	}
	return len(windows) > 1
}

// splitAnd splits a squashed expression on top-level `and` operators
// (outside parentheses, brackets, braces and strings), dropping an on() or
// ignoring() modifier after each.
func splitAnd(s string) []string {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote != '`' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], "and") && i > 0 && strings.ContainsRune(")]}", rune(s[i-1])):
			parts = append(parts, s[start:i])
			j := i + 3
			for _, mod := range []string{"on(", "ignoring("} {
				if strings.HasPrefix(s[j:], mod) {
					if k := strings.IndexByte(s[j:], ')'); k >= 0 {
						j += k + 1
					}
				}
			}
			start, i = j, j-1
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return append(parts, s[start:])
}

// sloCoverage returns the SLO coverage findings. ruleIDs and sloIDs are
// the consumer IDs of in.Rules and in.SLOs, index for index; signals carry
// their owners (classify ran).
func sloCoverage(in Inputs, ruleIDs, sloIDs []string, signals []graph.Signal, refs []graph.Reference) []graph.SLOFinding {
	var out []graph.SLOFinding
	owner := map[string]string{}
	for _, s := range signals {
		if s.Owner != "" {
			owner[s.ID] = s.Owner
		}
	}
	reads := map[string][]string{} // consumer -> signals it reads, sweeps left out
	for _, r := range refs {
		if !r.Sweep && !slices.Contains(reads[r.ConsumerID], r.SignalID) {
			reads[r.ConsumerID] = append(reads[r.ConsumerID], r.SignalID)
		}
	}

	// Services with no SLO.
	covered := map[string]bool{}
	for i, s := range in.SLOs {
		if s.Service != "" {
			covered[s.Service] = true
		}
		for _, sig := range reads[sloIDs[i]] {
			if o := owner[sig]; o != "" {
				covered[o] = true
			}
		}
	}
	for _, o := range in.Owners {
		if covered[o.Name] {
			continue
		}
		out = append(out, graph.SLOFinding{Kind: graph.SLOServiceWithout, Subject: "service:" + o.Name, Name: o.Name,
			Detail: "no SLO names this service or reads a signal it owns",
			Fix:    "define an SLO on what its users see (availability, latency) and page on its burn rate"})
	}

	// Paging alerts that aren't burn-rate alerts.
	for i, r := range in.Rules {
		if !paging(r) || sloRule(r, in.SLOs) {
			continue
		}
		sev := r.Labels["severity"]
		if sev == "" {
			sev = r.Labels["priority"]
		}
		if sev == "" {
			sev = "page"
		}
		detail := fmt.Sprintf("pages (%s) on a threshold, not on an SLO's error-budget burn", sev)
		for j, s := range in.SLOs {
			if sharesSignal(reads[ruleIDs[i]], reads[sloIDs[j]]) {
				detail += fmt.Sprintf("; SLO %s reads the same signals", s.Name)
				break
			}
		}
		out = append(out, graph.SLOFinding{Kind: graph.SLOPagingNotBurnRate, Subject: ruleIDs[i], Name: r.Name, Detail: detail,
			Fix: "page on a multiwindow burn-rate alert of the SLO this protects, and make this one a ticket"})
	}

	// SLOs with no alert: none of the rulers has an alert tied to it.
	for j, s := range in.SLOs {
		alerted := false
		for i, r := range in.Rules {
			if r.Kind != collect.RuleAlert {
				continue
			}
			if (s.Alert != "" && r.Name == s.Alert && (s.Format != "pyrra" || r.Labels["slo"] == "" || r.Labels["slo"] == s.Short)) ||
				(s.Short != "" && (r.Labels["sloth_slo"] == s.Short || r.Labels["slo"] == s.Short) && (r.Labels["sloth_service"] == "" || r.Labels["sloth_service"] == s.Service)) ||
				(sloRule(r, nil) && sharesSignal(reads[ruleIDs[i]], reads[sloIDs[j]])) {
				alerted = true
				break
			}
		}
		if alerted {
			continue
		}
		detail := "no alert in any ruler is tied to it, so nobody is paged when it burns its error budget"
		if s.Alert != "" {
			detail = fmt.Sprintf("declares alert %s, but no ruler has it loaded", s.Alert)
		}
		out = append(out, graph.SLOFinding{Kind: graph.SLOWithoutAlert, Subject: sloIDs[j], Name: s.Name, Detail: detail,
			Fix: "load the SLO's generated rules (sloth generate, the Pyrra operator) or add a multiwindow burn-rate alert"})
	}
	return out
}

func sharesSignal(a, b []string) bool {
	for _, s := range a {
		if slices.Contains(b, s) {
			return true
		}
	}
	return false
}
