package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/report"
	"github.com/opsmastery/tvt/pkg/store"
)

// TestRuleFindings (S6 item 1) scans the rule-efficiency state and checks
// every rule finding against ground truth's rule_findings: each listed
// rule gets exactly its listed kinds, each no_findings rule gets none, and
// every lab rule is one or the other, so any other finding is a false
// positive. It also checks the cost of each rule the lab's rulers log.
func TestRuleFindings(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "rule-efficiency" {
			st = s
		}
	}
	if st.Name == "" {
		t.Fatal("no rule-efficiency state in benchmark.yaml")
	}
	dir := dirs[st.Name]
	g, _, gt := scanState(t, st, dir)
	if len(gt.RuleFindings) == 0 {
		t.Fatal("ground truth has no rule_findings")
	}

	db := benchDB(t, st, dir)
	costs, findings := ruleResults(t, db)

	// Rule consumers by ground-truth name (alert name, recorded metric or
	// Grafana title) and source.
	byName := map[string]string{}
	for id, c := range g.consumers {
		if !strings.HasPrefix(id, "rule:") || (c.Kind != graph.ConsumerAlertRule && c.Kind != graph.ConsumerRecordingRule) {
			continue
		}
		src := strings.SplitN(strings.TrimPrefix(id, "rule:"), ":", 2)[0]
		byName[src+"/"+c.Name] = id
	}
	got := map[string][]string{}
	for _, f := range findings {
		got[f.ConsumerID] = append(got[f.ConsumerID], f.Kind)
		if f.Detail == "" || f.Fix == "" {
			t.Errorf("%s %s: detail %q, fix %q: both are required", f.ConsumerID, f.Kind, f.Detail, f.Fix)
		}
	}

	expected := map[string]bool{}
	for name, node := range gt.RuleFindings {
		if name == "no_findings" {
			var names []string
			if err := node.Decode(&names); err != nil {
				t.Fatal(err)
			}
			for _, n := range names {
				id := findRule(t, byName, gt, n)
				expected[id] = true
				if len(got[id]) > 0 {
					t.Errorf("%s: findings %v, want none", n, got[id])
				}
			}
			continue
		}
		var want gtRuleFindings
		if err := node.Decode(&want); err != nil {
			t.Fatalf("rule_findings %s: %v", name, err)
		}
		id, ok := byName[want.Source+"/"+name]
		if !ok {
			t.Errorf("rule_findings %s: no %s rule consumer", name, want.Source)
			continue
		}
		expected[id] = true
		var kinds []string
		for _, f := range want.Findings {
			kinds = append(kinds, f.Kind)
		}
		slices.Sort(kinds)
		have := slices.Sorted(slices.Values(got[id]))
		if !slices.Equal(have, kinds) {
			t.Errorf("%s: findings %v, want %v", name, have, kinds)
		}
	}
	for id, kinds := range got {
		if !expected[id] {
			t.Errorf("%s: findings %v on a rule ground truth doesn't list (false positive)", id, kinds)
		}
	}
	// Every lab rule is listed, one way or the other.
	for key, id := range byName {
		if !expected[id] && !strings.HasPrefix(key, "mimir/") {
			t.Errorf("%s (%s) is in neither rule_findings nor no_findings", key, id)
		}
	}

	// Cost per rule: every Prometheus and Loki ruler rule and the Grafana
	// alerts that ran in the snapshot are priced from logged evaluations;
	// each costs evaluations per day times what one evaluation read.
	byID := map[string]graph.RuleCost{}
	for _, c := range costs {
		byID[c.ConsumerID] = c
	}
	for key, id := range byName {
		c, ok := byID[id]
		if !ok {
			t.Errorf("%s: no rule cost", key)
			continue
		}
		if c.EvalsPerDay <= 0 {
			t.Errorf("%s: %v evaluations a day; every lab rule has an interval", key, c.EvalsPerDay)
		}
		if c.Unknown != "" || c.Evaluations == 0 {
			t.Errorf("%s: cost unknown (%s, %d evaluations logged)", key, c.Unknown, c.Evaluations)
		}
	}
	checkCost := func(key string, wantPerDay float64, samples bool) {
		t.Helper()
		c := byID[byName[key]]
		got := c.BytesPerDay()
		if samples {
			got = c.SamplesPerDay()
		}
		if got <= 0 || (wantPerDay > 0 && (got < wantPerDay/2 || got > wantPerDay*2)) {
			t.Errorf("%s: %.0f per day (%.1f evals/day × samples %.1f, bytes %.1f), want about %.0f", key, got, c.EvalsPerDay, c.SamplesPerEval, c.BytesPerEval, wantPerDay)
		}
	}
	// The 5s group evaluates 17,280 times a day; the [24h] rule re-reads a
	// day of checkout counters on each of its 1,440.
	if c := byID[byName["prometheus/ShopPaymentQueueSpike"]]; c.EvalsPerDay != 17280 {
		t.Errorf("ShopPaymentQueueSpike: %v evaluations a day, want 17280", c.EvalsPerDay)
	}
	if long, short := byID[byName["prometheus/ShopDailyCheckoutsLow"]], byID[byName["prometheus/ShopHighErrorRate"]]; long.SamplesPerEval <= 10*short.SamplesPerEval {
		t.Errorf("the [24h] rule reads %.0f samples per evaluation, the [5m] one %.0f: want far more", long.SamplesPerEval, short.SamplesPerEval)
	}
	checkCost("grafana/Shop checkout errors (logs)", 0, false)
	checkCost("loki/shop:checkout_errors:count5m", 0, false)
}

// findRule finds a no_findings rule by name in whichever ruler has it.
func findRule(t *testing.T, byName map[string]string, gt groundTruth, name string) string {
	t.Helper()
	src := "prometheus"
	if r, ok := gt.Rules[name]; ok && r.Source != "" {
		src = r.Source
	}
	id, ok := byName[src+"/"+name]
	if !ok {
		t.Errorf("no_findings %s: no %s rule consumer", name, src)
	}
	return id
}

// benchDB scans a benchmark state into a database and returns its path.
// benchDBs caches each capture's scanned database across tests (a scan of
// a capture takes about 20s, and the package has a 5-minute pre-push
// budget): each caller gets its own copy, so tests can't see each other's
// writes.
var (
	benchMu  sync.Mutex
	benchDBs = map[string]string{}
)

func benchDB(t *testing.T, st benchState, dir string) string {
	t.Helper()
	benchMu.Lock()
	defer benchMu.Unlock()
	if cached, ok := benchDBs[dir]; ok {
		return copyDB(t, cached)
	}
	db := scanBenchDB(t, st, dir)
	if t.Failed() {
		return db
	}
	keep, err := os.MkdirTemp("", "tvt-benchdb-")
	if err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(keep, "tvt.db")
	if err := copyFile(db, cached); err != nil {
		t.Fatal(err)
	}
	benchDBs[dir] = cached
	return db
}

func copyDB(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "tvt.db")
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// copyFile copies a SQLite database with its WAL files, if any are left.
func copyFile(src, dst string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(src + suffix)
		if err != nil {
			if suffix != "" && os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := os.WriteFile(dst+suffix, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func scanBenchDB(t *testing.T, st benchState, dir string) string {
	t.Helper()
	l := newLabAt(t, dir, nil)
	db := filepath.Join(t.TempDir(), "tvt.db")
	o := labOptionsAt(dir, l, db)
	o.LokiQueryLogs = stringList{l.lokiLog}
	var out strings.Builder
	if err := runScan(context.Background(), o, env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "rules ") {
			t.Log(line)
			return db
		}
	}
	t.Errorf("scan output has no rules line:\n%s", out.String())
	return db
}

func ruleResults(t *testing.T, db string) ([]graph.RuleCost, []graph.RuleFinding) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := s.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	costs, err := s.RuleCosts(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := s.RuleFindings(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	return costs, findings
}

// TestQueryLoadRuleEfficiency (S6 item 2) scans the rule-efficiency state:
// the logged queries' measured cost lands on their consumers, adds up to
// what the query logs recorded, and the report splits it by client with
// the agents' share.
func TestQueryLoadRuleEfficiency(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	var st benchState
	for _, s := range bm.States {
		if s.Name == "rule-efficiency" {
			st = s
		}
	}
	db := benchDB(t, st, dirs[st.Name])
	in, _, err := loadReport(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.QueryCosts) == 0 {
		t.Fatal("no query costs stored")
	}
	r := report.Build(in, 50)
	ql := r.QueryLoad
	t.Logf("query load: %.0f queries, %.0f samples, %.0f bytes", ql.Queries, ql.Samples, ql.Bytes)
	clients := map[string]report.ClientLoad{}
	var shares float64
	for _, c := range ql.Clients {
		t.Logf("  %-10s %6.0f queries %12.0f samples (%.1f%%) %12.0f bytes (%.1f%%)", c.Client, c.Queries, c.Samples, 100*c.SampleShare, c.Bytes, 100*c.ByteShare)
		clients[c.Client] = c
		shares += c.SampleShare
	}
	if ql.Samples <= 0 || ql.Bytes <= 0 {
		t.Errorf("samples %v, bytes %v: the lab logs both", ql.Samples, ql.Bytes)
	}
	if shares < 0.999 || shares > 1.001 {
		t.Errorf("sample shares sum to %v", shares)
	}
	// The lab runs two agents and a viewed dashboard; Grafana evaluates
	// its managed alerts through the datasources (client rule).
	for _, want := range []string{"agent", "dashboard", "rule"} {
		if clients[want].Queries == 0 {
			t.Errorf("no %s queries in the load: %+v", want, ql.Clients)
		}
	}
	if clients["agent"].Samples == 0 {
		t.Errorf("agents drove no samples: %+v", clients["agent"])
	}
	if len(ql.Top) == 0 || len(ql.Top) > report.TopQueryRunners {
		t.Errorf("top %d", len(ql.Top))
	}
}
