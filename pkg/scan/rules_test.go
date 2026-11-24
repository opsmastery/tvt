package scan

import (
	"slices"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/graph"
)

func TestMaxRange(t *testing.T) {
	for expr, want := range map[string]time.Duration{
		`sum(increase(x[24h])) < 10`:                       24 * time.Hour,
		`rate(x[5m]) / rate(y[1h])`:                        time.Hour,
		`max_over_time(rate(x[5m])[2h:1m])`:                2 * time.Hour,
		`count_over_time({a="b"} |= "[7d]" [5m])`:          5 * time.Minute, // a string isn't a range
		`rate(x[$__rate_interval])`:                        0,
		`x{le="[1d]"}`:                                     0,
		`sum(count_over_time({app="x"} | logfmt [1h30m]))`: 90 * time.Minute,
		`up`: 0,
	} {
		if got := maxRange(expr); got != want {
			t.Errorf("maxRange(%q) = %v, want %v", expr, got, want)
		}
	}
}

func TestParserWithoutFilter(t *testing.T) {
	for expr, want := range map[string]string{
		`sum(count_over_time({app="shop", level="error"} | logfmt | route="/checkout" [5m]))`: "logfmt",
		`sum(count_over_time({app="shop"} |= "route=/checkout" | logfmt [5m]))`:               "",
		`sum(count_over_time({app="shop"} != "healthz" | json [5m]))`:                         "",
		`sum(count_over_time({app="shop"} |~ "a|b" | pattern "<_> <x>" [5m]))`:                "",
		`{app!="x"} | json`: "json", // != inside the selector isn't a filter
		`{app="x"} | json | line_format "{{.a}}" |= "y"`:                             "json", // a filter after the parser doesn't narrow it
		`count_over_time({a="b"} |= "| json" [1m])`:                                  "",     // a parser named in a string isn't one
		`sum(rate({a="b"} | logfmt [1m])) / sum(rate({a="b"} |= "x" | logfmt [1m]))`: "logfmt",
		`count_over_time({a="b"} [1m])`:                                              "",
	} {
		got, ok := parserWithoutFilter(expr)
		if got != want || ok != (want != "") {
			t.Errorf("parserWithoutFilter(%q) = %q, %v; want %q", expr, got, ok, want)
		}
	}
}

func TestSquash(t *testing.T) {
	for _, pair := range [][2]string{
		{"sum(rate(x[5m]))\n  / sum(rate(y[5m])) > 0.05", "sum(rate(x[5m]))/sum(rate(y[5m]))>0.05"},
		{`(sum(count_over_time({a="b c"} |= "x y"[5m])) > 20)`, `sum(count_over_time({a="b c"}|="x y"[5m]))>20`},
		{`((up))`, `up`},
		{`(a) or (b)`, `(a)or(b)`},
		{`("(")`, `"("`},
		{`(`, `(`},
	} {
		if got := squash(pair[0]); got != pair[1] {
			t.Errorf("squash(%q) = %q, want %q", pair[0], got, pair[1])
		}
	}
}

// TestRuleAnalysisCases covers what the lab doesn't: an unknown interval
// is an unknown cost, never zero; Mimir's ruler isn't priced; a slow group;
// an absent() rule is never no_series; a duplicate needs the same kind.
func TestRuleAnalysisCases(t *testing.T) {
	in := Inputs{
		MetricsSource: "prom",
		Metrics:       []collect.MetricCardinality{{Metric: "up", ActiveSeries: 3}},
		Rules: []collect.Rule{
			{Source: "prometheus", Group: "g", Name: "NoInterval", Kind: collect.RuleAlert, Expr: "up == 0", Language: "promql"},
			{Source: "mimir", Group: "m", Name: "MimirRule", Kind: collect.RuleAlert, Expr: "up == 0", Language: "promql", Interval: time.Minute},
			{Source: "prometheus", Group: "slow", Name: "Slow", Kind: collect.RuleAlert, Expr: "up > 1", Language: "promql", Interval: 30 * time.Second, GroupEvaluation: 45 * time.Second},
			{Source: "prometheus", Group: "g", Name: "Gone", Kind: collect.RuleAlert, Expr: "absent(gone_metric)", Language: "promql", Interval: time.Minute},
			{Source: "prometheus", Group: "g", Name: "up:copy", Kind: collect.RuleRecord, Expr: "up == 0", Language: "promql", Interval: time.Minute},
		},
		RuleEvaluations: []collect.RuleEvaluation{
			{Source: "prometheus", Group: "slow", Expr: "up  >  1", Samples: 3},
			{Source: "prometheus", Group: "other", Expr: "up > 1", Samples: 999}, // another group's rule
		},
	}
	ids := []string{"r:noint", "r:mimir", "r:slow", "r:gone", "r:copy"}
	refs := []graph.Reference{
		{SignalID: "prom:metric:up", ConsumerID: "r:noint"},
		{SignalID: "prom:metric:up", ConsumerID: "r:slow"},
		{SignalID: "prom:metric:gone_metric", ConsumerID: "r:gone"},
	}
	costs, findings := ruleAnalysis(in, ids, refs)
	byID := map[string]graph.RuleCost{}
	for _, c := range costs {
		byID[c.ConsumerID] = c
	}
	if c := byID["r:noint"]; c.EvalsPerDay != 0 || c.Unknown == "" {
		t.Errorf("no interval: %+v, want an unknown cost", c)
	}
	if c := byID["r:mimir"]; c.EvalsPerDay != 1440 || c.Unknown == "" {
		t.Errorf("mimir: %+v, want 1440 evals and an unknown per-evaluation cost", c)
	}
	if c := byID["r:slow"]; c.Evaluations != 1 || c.SamplesPerEval != 3 || c.SamplesPerDay() != 3*2880 || c.Unknown != "" {
		t.Errorf("slow: %+v", c)
	}
	kinds := map[string][]string{}
	for _, f := range findings {
		kinds[f.ConsumerID] = append(kinds[f.ConsumerID], f.Kind)
	}
	if !slices.Equal(kinds["r:slow"], []string{graph.RuleGroupSlow}) {
		t.Errorf("slow: %v", kinds["r:slow"])
	}
	if len(kinds["r:gone"]) != 0 {
		t.Errorf("absent() rule: %v, want none", kinds["r:gone"])
	}
	if slices.Contains(kinds["r:noint"], graph.RuleDuplicate) || !slices.Contains(kinds["r:copy"], graph.RuleUnreadRecording) {
		t.Errorf("an alert and a recording rule with one expression aren't duplicates: %v / %v", kinds["r:noint"], kinds["r:copy"])
	}
}

func FuzzSquash(f *testing.F) {
	for _, s := range []string{`(sum(x) > 1)`, `"(" + ")"`, "((a)", `'\''`, "`(`"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a := squash(s)
		if squash(a) != a {
			t.Fatalf("squash not idempotent: %q -> %q -> %q", s, a, squash(a))
		}
		_ = maxRange(s)
		_, _ = parserWithoutFilter(s)
	})
}
