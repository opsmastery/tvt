package scan

import (
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// S3 item 6: a query over every metric name, or most of the series, is a
// sweep. Its references are kept as weak evidence and keep nothing; a
// metric the same or another consumer names is still read.
func TestSweeps(t *testing.T) {
	at := start.Add(-time.Hour)
	in := baseInputs()
	q := func(expr, client string) collect.LoggedQuery {
		return collect.LoggedQuery{Source: "prometheus", Language: "promql", Expr: expr, At: at, Client: client}
	}
	in.Queries = []collect.LoggedQuery{
		q(`count by (__name__) ({__name__=~".+"})`, "10.0.0.1"),
		// shop_.+ is 5731 of 5756 series: a sweep.
		q(`count({__name__=~"shop_.+"})`, "10.0.0.2"),
		// node_.+ is 17 series: a read of each.
		q(`count({__name__=~"node_.+"})`, "10.0.0.3"),
		// A label restriction: tvt can't tell what it selects; not a sweep.
		q(`count({job="shop"})`, "10.0.0.4"),
		// Sweeping and naming one metric: that one is read.
		q(`count({__name__=~".+", job=~".*"}) + node_load1`, "10.0.0.5"),
	}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	sweep := map[string]bool{} // signal -> consumer address -> sweep
	byClient := map[string]map[string]bool{}
	names := map[string]string{}
	for _, c := range out.Consumers {
		names[c.ID] = c.Name
	}
	for _, r := range out.References {
		name := names[r.ConsumerID]
		if !strings.HasPrefix(name, "ad hoc") || r.Via != "" || !strings.Contains(r.SignalID, ":metric:") {
			continue
		}
		from := name[strings.LastIndex(name, " ")+1:]
		if byClient[from] == nil {
			byClient[from] = map[string]bool{}
		}
		byClient[from][r.SignalID] = r.Sweep
		if r.Sweep {
			sweep[r.SignalID] = true
			if !strings.Contains(strings.Join(r.Reasons, " "), "a sweep") {
				t.Errorf("%s via %s: sweep without a reason: %v", r.SignalID, name, r.Reasons)
			}
		}
	}
	for addr, want := range map[string]bool{"10.0.0.1": true, "10.0.0.2": true, "10.0.0.3": false, "10.0.0.4": false} {
		if len(byClient[addr]) == 0 {
			t.Errorf("%s: no references", addr)
		}
		for sig, got := range byClient[addr] {
			if got != want {
				t.Errorf("%s -> %s: sweep %v, want %v", addr, sig, got, want)
			}
		}
	}
	if byClient["10.0.0.5"]["prom:metric:node_load1"] || !byClient["10.0.0.5"]["prom:metric:node_cpu_seconds_total"] {
		t.Errorf("sweep plus a named metric: %v", byClient["10.0.0.5"])
	}

	// Classes: a metric only swept stays a candidate (or dormant), never
	// read; one read by name elsewhere is read.
	cls := map[string]string{}
	for _, s := range out.Signals {
		cls[s.ID] = s.Class
	}
	if c := cls["prom:metric:shop_http_requests_total"]; c != "candidate" {
		t.Errorf("swept-only metric class %q", c)
	}
}

// Declared consumers never sweep: an alert over every metric depends on
// all of them.
func TestDeclaredNeverSweeps(t *testing.T) {
	in := baseInputs()
	in.Rules = append(in.Rules, collect.Rule{Source: "prometheus", Namespace: "x", Group: "g", Name: "TooMany",
		Kind: collect.RuleAlert, Language: "promql", Expr: `count({__name__=~".+"}) > 1e6`})
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.References {
		if strings.HasSuffix(r.ConsumerID, "/TooMany") && r.Sweep {
			t.Fatalf("alert reference marked sweep: %+v", r)
		}
	}
	for _, s := range out.Signals {
		if s.Kind == graph.KindMetric && s.Class != "hard_keep" {
			t.Errorf("%s: %s, want hard_keep under an alert over every metric", s.ID, s.Class)
		}
	}
}

// A panel picking its metric with a variable tvt can't resolve reads every
// metric, assumed: unknown means used, not a sweep.
func TestVariableNameIsNotASweep(t *testing.T) {
	in := baseInputs()
	in.Dashboards = []collect.Dashboard{{UID: "d", Title: "D", Panels: []collect.Panel{{ID: 1, Title: "Any metric",
		Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: `sum({__name__=~"$metric"})`}}}}}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var refs int
	for _, r := range out.References {
		if strings.HasPrefix(r.ConsumerID, "panel:") {
			refs++
			if r.Sweep || !r.Assumed {
				t.Errorf("%+v", r)
			}
		}
	}
	if refs == 0 {
		t.Fatal("no panel references")
	}
}

func TestSweepShareConfigurable(t *testing.T) {
	in := baseInputs()
	in.SweepShare = 0.999 // shop_.+ is 5731 of 5756 series (99.6%): no longer a sweep
	in.Queries = []collect.LoggedQuery{{Source: "prometheus", Language: "promql", Expr: `count({__name__=~"shop_.+"})`, At: start.Add(-time.Hour)}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.References {
		if strings.HasPrefix(r.ConsumerID, "qlog:") && r.Sweep {
			t.Errorf("sweep at share 0.999: %+v", r)
		}
	}
}
