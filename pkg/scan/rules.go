package scan

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// Rule-efficiency thresholds (S6 item 1).
const (
	// LongRangeMin and LongRangeRatio: a range is "far longer than the
	// interval" when it is at least LongRangeMin and at least
	// LongRangeRatio intervals, so each evaluation re-reads that many
	// intervals of samples to move the answer by one.
	LongRangeMin   = time.Hour
	LongRangeRatio = 120
)

// ruleQuery is one query a rule evaluates, in its language.
type ruleQuery struct {
	expr, lang string
}

// queriesOf lists what a rule evaluates: its expression, or a
// Grafana-managed rule's datasource queries in the languages tvt reads.
func queriesOf(r collect.Rule) []ruleQuery {
	if len(r.Queries) == 0 {
		lang := "promql"
		if r.Language == "logql" {
			lang = "logql"
		}
		return []ruleQuery{{r.Expr, lang}}
	}
	var out []ruleQuery
	for _, q := range r.Queries {
		if lang, ok := collect.QueryLanguage(q.DatasourceType); ok && strings.TrimSpace(q.Expr) != "" {
			out = append(out, ruleQuery{q.Expr, lang})
		}
	}
	return out
}

// ruleAnalysis prices every rule's evaluations and finds what's wrong with
// each (S6 item 1). ids[i] is the consumer ID of in.Rules[i]. Report only:
// nothing here changes a signal's class.
func ruleAnalysis(in Inputs, ids []string, refs []graph.Reference) ([]graph.RuleCost, []graph.RuleFinding) {
	collected := map[string]bool{}
	for _, m := range in.Metrics {
		collected[m.Metric] = true
	}
	metricPrefix := in.MetricsSource + ":metric:"
	direct := map[string][]string{}  // consumer -> metric names it reads directly
	readers := map[string][]string{} // metric signal -> consumers reading it directly, sweeps left out
	for _, r := range refs {
		name, ok := strings.CutPrefix(r.SignalID, metricPrefix)
		if !ok || r.Via != "" {
			continue
		}
		if !slices.Contains(direct[r.ConsumerID], name) {
			direct[r.ConsumerID] = append(direct[r.ConsumerID], name)
		}
		if !r.Sweep {
			readers[r.SignalID] = append(readers[r.SignalID], r.ConsumerID)
		}
	}

	var costs []graph.RuleCost
	var findings []graph.RuleFinding
	add := func(id, kind, detail, fix string) {
		findings = append(findings, graph.RuleFinding{ConsumerID: id, Kind: kind, Detail: detail, Fix: fix})
	}

	// Duplicates: same ruler, kind and expression, whitespace aside.
	dups := map[string][]int{}
	for i, r := range in.Rules {
		var parts []string
		for _, q := range queriesOf(r) {
			parts = append(parts, q.lang+":"+squash(q.expr))
		}
		if len(parts) == 0 {
			continue
		}
		key := r.Source + "\x00" + string(r.Kind) + "\x00" + strings.Join(parts, "\x00")
		dups[key] = append(dups[key], i)
	}

	for i, r := range in.Rules {
		id := ids[i]
		qs := queriesOf(r)
		costs = append(costs, ruleCost(in, r, id, qs))

		var logql, promql []string
		for _, q := range qs {
			if q.lang == "logql" {
				logql = append(logql, q.expr)
			} else {
				promql = append(promql, q.expr)
			}
		}

		if r.EvaluatedBy == collect.EvaluatedByGrafana && r.Kind == collect.RuleAlert && len(qs) > 0 {
			ruler := "Prometheus (or Mimir)"
			if len(logql) > 0 {
				ruler = "Loki"
			}
			add(id, graph.RuleGrafanaManaged,
				fmt.Sprintf("Grafana evaluates it every %s and sends its queries to the datasource each time", intervalText(r.Interval)),
				"move it to the "+ruler+" ruler, or alert on a recording rule there")
		}
		for _, e := range logql {
			if r.Kind == collect.RuleAlert && hasParser(e) {
				add(id, graph.RuleParsesRawLogs,
					fmt.Sprintf("parses raw log lines on every evaluation (every %s)", intervalText(r.Interval)),
					"record the count with a Loki recording rule and alert on its output")
				break
			}
		}
		for _, e := range logql {
			if parser, ok := parserWithoutFilter(e); ok {
				add(id, graph.RuleNoLineFilter,
					fmt.Sprintf("| %s runs on every line the selector returns: no line filter before it", parser),
					"add a line filter (|= \"…\") before the parser, so only matching lines are parsed")
				break
			}
		}
		// An SLO's burn-rate alerts and recorded windows need long ranges.
		if r.Interval > 0 && !sloRule(r, in.SLOs) {
			var longest time.Duration
			for _, q := range qs {
				longest = max(longest, maxRange(q.expr))
			}
			if longest >= LongRangeMin && longest >= LongRangeRatio*r.Interval {
				add(id, graph.RuleLongRange,
					fmt.Sprintf("a [%s] range evaluated every %s: each evaluation re-reads %d intervals of data", model.Duration(longest), intervalText(r.Interval), int(longest/r.Interval)),
					"evaluate it less often (every 15m or more), or record a shorter window and aggregate the recorded series")
			}
		}
		if r.Interval > 0 && len(promql) > 0 && in.Scrape != nil {
			if scrape, ok := scrapeInterval(*in.Scrape, direct[id]); ok && r.Interval < scrape {
				add(id, graph.RuleIntervalBelowScrap,
					fmt.Sprintf("evaluated every %s, but the metrics it reads are scraped every %s or slower", intervalText(r.Interval), intervalText(scrape)),
					fmt.Sprintf("evaluate it every %s or slower: faster evaluations see the same samples", intervalText(scrape)))
			}
		}
		if group := dups[r.Source+"\x00"+string(r.Kind)+"\x00"+dupKey(qs)]; len(group) > 1 {
			var others []string
			for _, j := range group {
				if j != i {
					others = append(others, fmt.Sprintf("%s (%s)", in.Rules[j].Name, in.Rules[j].Group))
				}
			}
			add(id, graph.RuleDuplicate, "same expression as "+strings.Join(others, ", "), "keep one")
		}
		if r.Kind == collect.RuleRecord && r.Name != "" {
			out := metricPrefix + r.Name
			read := false
			for _, c := range readers[out] {
				if c != id {
					read = true
					break
				}
			}
			if !read {
				add(id, graph.RuleUnreadRecording, "no alert, dashboard, query or other rule reads "+r.Name, "remove the rule")
			}
		}
		if len(promql) > 0 && len(logql) == 0 && !usesAbsence(promql) {
			names := direct[id]
			missing := 0
			for _, n := range names {
				if !collected[n] {
					missing++
				}
			}
			if len(names) > 0 && missing == len(names) {
				sorted := slices.Sorted(slices.Values(names))
				add(id, graph.RuleNoSeries,
					fmt.Sprintf("%s %s no series in the TSDB, so the rule can never return anything", strings.Join(sorted, ", "), plural2(len(sorted), "has", "have")),
					"the rule is broken, not unused: fix its metric name or delete it")
			}
		}
		if r.GroupEvaluation > 0 && r.Interval > 0 && r.GroupEvaluation > r.Interval {
			add(id, graph.RuleGroupSlow,
				fmt.Sprintf("group %s took %s to evaluate, longer than its %s interval: evaluations are skipped", r.Group, r.GroupEvaluation.Round(time.Millisecond), intervalText(r.Interval)),
				"split the group, speed up its slowest rule, or lengthen the interval")
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].ConsumerID != findings[j].ConsumerID {
			return findings[i].ConsumerID < findings[j].ConsumerID
		}
		return findings[i].Kind < findings[j].Kind
	})
	return costs, findings
}

func dupKey(qs []ruleQuery) string {
	var parts []string
	for _, q := range qs {
		parts = append(parts, q.lang+":"+squash(q.expr))
	}
	return strings.Join(parts, "\x00")
}

func usesAbsence(exprs []string) bool {
	for _, e := range exprs {
		if parse.UsesAbsence(e) {
			return true
		}
	}
	return false
}

// ruleCost is evaluations per day times the measured cost of one
// evaluation: the datasource ruler's own logged evaluations (Prometheus
// by group and query, Loki by query), or for a Grafana-managed rule the
// instant queries Grafana sent with the rule's query text.
func ruleCost(in Inputs, r collect.Rule, id string, qs []ruleQuery) graph.RuleCost {
	c := graph.RuleCost{ConsumerID: id}
	if r.Interval > 0 {
		c.EvalsPerDay = float64(24*time.Hour) / float64(r.Interval)
	}
	var unknown []string
	if r.Interval <= 0 {
		unknown = append(unknown, "interval unknown")
	}
	switch {
	case len(qs) == 0:
		unknown = append(unknown, "no query tvt reads")
	case r.EvaluatedBy == collect.EvaluatedByGrafana:
		// Each datasource query is logged as its own instant query.
		minEvals := int64(-1)
		for _, q := range qs {
			src := "prometheus"
			if q.lang == "logql" {
				src = "loki"
			}
			want := squash(q.expr)
			var n, samples, bytes int64
			for _, lq := range in.Queries {
				if lq.Instant && lq.Source == src && squash(lq.Expr) == want {
					n++
					samples += lq.Samples
					bytes += lq.Bytes
				}
			}
			if n > 0 {
				c.SamplesPerEval += float64(samples) / float64(n)
				c.BytesPerEval += float64(bytes) / float64(n)
			}
			if minEvals < 0 || n < minEvals {
				minEvals = n
			}
		}
		c.Evaluations = max(minEvals, 0)
	case r.Source == "prometheus" || r.Source == "loki":
		want := squash(qs[0].expr)
		var samples, bytes int64
		for _, e := range in.RuleEvaluations {
			if e.Source != r.Source || squash(e.Expr) != want || (e.Group != "" && e.Group != r.Group) {
				continue
			}
			c.Evaluations++
			samples += e.Samples
			bytes += e.Bytes
		}
		if c.Evaluations > 0 {
			c.SamplesPerEval = float64(samples) / float64(c.Evaluations)
			c.BytesPerEval = float64(bytes) / float64(c.Evaluations)
		}
	default:
		unknown = append(unknown, r.Source+" doesn't log its ruler's evaluations with their cost")
	}
	if c.Evaluations == 0 && len(qs) > 0 && (r.EvaluatedBy == collect.EvaluatedByGrafana || r.Source == "prometheus" || r.Source == "loki") {
		unknown = append(unknown, "no logged evaluation in the window")
	}
	c.Unknown = strings.Join(unknown, "; ")
	return c
}

// scrapeInterval is the shortest scrape interval among the metrics read,
// when every one of them has a known interval (a recording rule's output
// or a remote-written series has none, so no comparison is made).
func scrapeInterval(s collect.Scrape, metrics []string) (time.Duration, bool) {
	var best time.Duration
	for _, m := range metrics {
		fam, ok := s.Family(m)
		if !ok {
			return 0, false
		}
		d, ok := s.Intervals[fam]
		if !ok || d <= 0 {
			return 0, false
		}
		if best == 0 || d < best {
			best = d
		}
	}
	return best, best > 0
}

var (
	stringLit   = regexp.MustCompile("\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`|'(?:[^'\\\\]|\\\\.)*'")
	ruleRangeRe = regexp.MustCompile(`\[\s*([0-9][0-9a-zA-Z]*)\s*(?::[^\]]*)?\]`)
	parserRe    = regexp.MustCompile(`\|\s*(json|logfmt|regexp|pattern|unpack)\b`)
	filterRe    = regexp.MustCompile(`(\|=|\|~|!=|!~)`)
	braceRe     = regexp.MustCompile(`\{[^{}]*\}`)
)

// strip blanks out string literals, so their contents never look like
// syntax.
func strip(expr string) string {
	return stringLit.ReplaceAllStringFunc(expr, func(s string) string { return `""` })
}

// squash removes whitespace outside string literals and parentheses that
// wrap the whole expression, so two rules that differ only in layout
// compare equal (Loki logs a binary expression wrapped in parentheses).
func squash(expr string) string {
	s := squashSpace(expr)
	for len(s) >= 2 && s[0] == '(' && closes(s) == len(s)-1 {
		s = s[1 : len(s)-1]
	}
	return s
}

// closes is the index of the parenthesis closing s[0], outside string
// literals; -1 when there is none.
func closes(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '`', '\'':
			q := s[i]
			for i++; i < len(s) && s[i] != q; i++ {
				if s[i] == '\\' && q != '`' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func squashSpace(expr string) string {
	var b strings.Builder
	last := 0
	for _, loc := range stringLit.FindAllStringIndex(expr, -1) {
		b.WriteString(strings.Join(strings.Fields(expr[last:loc[0]]), ""))
		b.WriteString(expr[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(strings.Join(strings.Fields(expr[last:]), ""))
	return b.String()
}

// maxRange is the longest range or subquery window in a PromQL or LogQL
// expression; 0 when there is none (or it is a variable).
func maxRange(expr string) time.Duration {
	var best time.Duration
	for _, m := range ruleRangeRe.FindAllStringSubmatch(strip(expr), -1) {
		if d, err := model.ParseDuration(m[1]); err == nil && time.Duration(d) > best {
			best = time.Duration(d)
		}
	}
	return best
}

// hasParser reports whether a LogQL expression parses lines.
func hasParser(expr string) bool {
	return parserRe.MatchString(strip(expr))
}

// parserWithoutFilter finds a parser stage with no line filter between it
// and its stream selector, returning the parser's name.
func parserWithoutFilter(expr string) (string, bool) {
	s := strip(expr)
	for _, loc := range parserRe.FindAllStringSubmatchIndex(s, -1) {
		before := s[:loc[0]]
		end := strings.LastIndex(before, "}")
		if end < 0 {
			continue
		}
		// The pipeline since the selector, matchers blanked out.
		pipeline := braceRe.ReplaceAllString(before[end+1:], "")
		if !filterRe.MatchString(pipeline) {
			return s[loc[2]:loc[3]], true
		}
	}
	return "", false
}

func intervalText(d time.Duration) string {
	if d <= 0 {
		return "unknown interval"
	}
	return model.Duration(d).String()
}

func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
