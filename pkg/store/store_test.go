package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	// A space in the path, as in the author's checkout.
	path := filepath.Join(t.TempDir(), "usage graph.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func scan(t *testing.T, s *Store, started time.Time, fn func(*Scan)) int64 {
	t.Helper()
	sc, err := s.BeginScan(ctx, started, 30)
	must(t, err)
	defer sc.Abort()
	fn(sc)
	must(t, sc.Finish(ctx, 0.8))
	return sc.ID
}

func TestRoundTrip(t *testing.T) {
	s, _ := open(t)
	if _, err := s.LatestScan(ctx); !errors.Is(err, ErrNoScan) {
		t.Fatalf("empty db: err = %v, want ErrNoScan", err)
	}

	sig := graph.Signal{ID: "prom:metric:shop_cart_operations_total", Kind: graph.KindMetric,
		Name: "shop_cart_operations_total", Source: "prom", FirstSeen: t0.Add(-48 * time.Hour)}
	lbl := graph.Signal{ID: "prom:label:shop_cart_operations_total:user_id", Kind: graph.KindLabel,
		Name: "user_id", Parent: "shop_cart_operations_total", Source: "prom"} // FirstSeen defaults to scan start
	con := graph.Consumer{ID: "panel:shop-overview:4", Kind: graph.ConsumerDashboardPanel,
		Name: "Shop Overview / Cart operations by type", Expr: "sum by (op) (rate(shop_cart_operations_total[$__rate_interval]))",
		LastViewed: t0.Add(-time.Hour)}
	ref := graph.Reference{SignalID: sig.ID, ConsumerID: con.ID, LastSeen: t0.Add(-30 * time.Minute), Actor: "admin"}
	cost := graph.Cost{SignalID: sig.ID, ActiveSeries: 5091, DistinctValues: 2000, MonthlyCost: 12.5}

	id := scan(t, s, t0, func(sc *Scan) {
		// References first: foreign keys are checked at commit.
		must(t, sc.PutReference(ctx, ref))
		must(t, sc.PutCost(ctx, cost))
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutSignal(ctx, lbl))
		must(t, sc.PutConsumer(ctx, con))
	})

	latest, err := s.LatestScan(ctx)
	must(t, err)
	if latest.ID != id || !latest.StartedAt.Equal(t0) || latest.WindowDays != 30 || latest.LogCoverage != 0.8 ||
		latest.FinishedAt.Before(t0) {
		t.Errorf("latest = %+v", latest)
	}

	sigs, err := s.Signals(ctx, id)
	must(t, err)
	lbl.FirstSeen = t0
	if want := []graph.Signal{lbl, sig}; !reflect.DeepEqual(sigs, want) {
		t.Errorf("signals\n got  %+v\n want %+v", sigs, want)
	}
	cons, err := s.Consumers(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(cons, []graph.Consumer{con}) {
		t.Errorf("consumers = %+v", cons)
	}
	refs, err := s.References(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(refs, []graph.Reference{ref}) {
		t.Errorf("refs = %+v", refs)
	}
	costs, err := s.Costs(ctx, id)
	must(t, err)
	cost.ScanID, cost.ObservedAt = id, t0
	if !reflect.DeepEqual(costs, []graph.Cost{cost}) {
		t.Errorf("costs = %+v", costs)
	}
}

func TestAcrossScans(t *testing.T) {
	s, _ := open(t)
	sig := graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom", FirstSeen: t0.Add(-10 * 24 * time.Hour)}
	keep := graph.Consumer{ID: "alert:A", Kind: graph.ConsumerAlertRule, Name: "A", LastViewed: time.Time{}}
	dash := graph.Consumer{ID: "panel:d:1", Kind: graph.ConsumerDashboardPanel, Name: "D / 1", LastViewed: t0}

	first := scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, keep))
		must(t, sc.PutConsumer(ctx, dash))
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: "m", ConsumerID: "panel:d:1", LastSeen: t0, Actor: "ann"}))
		must(t, sc.PutCost(ctx, graph.Cost{SignalID: "m", ActiveSeries: 100}))
	})

	t1 := t0.Add(24 * time.Hour)
	second := scan(t, s, t1, func(sc *Scan) {
		// The collector now reports a later FirstSeen (e.g. after a TSDB
		// restart): the earliest wins. The dashboard was deleted.
		later := sig
		later.FirstSeen = t1
		must(t, sc.PutSignal(ctx, later))
		k := keep
		k.LastViewed = time.Time{}
		must(t, sc.PutConsumer(ctx, k))
		must(t, sc.PutCost(ctx, graph.Cost{SignalID: "m", ActiveSeries: 40}))
	})

	sigs, _ := s.Signals(ctx, second)
	if len(sigs) != 1 || !sigs[0].FirstSeen.Equal(sig.FirstSeen) {
		t.Errorf("first_seen not kept: %+v", sigs)
	}
	cons, _ := s.Consumers(ctx, second)
	if len(cons) != 1 || cons[0].ID != "alert:A" {
		t.Errorf("deleted dashboard still in latest graph: %+v", cons)
	}
	if refs, _ := s.References(ctx, second); len(refs) != 0 {
		t.Errorf("stale reference in latest graph: %+v", refs)
	}
	// History stays queryable per scan: savings = 100 -> 40 series.
	c1, _ := s.Costs(ctx, first)
	c2, _ := s.Costs(ctx, second)
	if len(c1) != 1 || len(c2) != 1 || c1[0].ActiveSeries != 100 || c2[0].ActiveSeries != 40 {
		t.Errorf("costs: first %+v, second %+v", c1, c2)
	}
	scans, _ := s.Scans(ctx)
	if len(scans) != 2 || scans[0].ID != first || scans[1].ID != second {
		t.Errorf("scans = %+v", scans)
	}
}

func TestLatestEvidenceWins(t *testing.T) {
	s, _ := open(t)
	sig := graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom"}
	con := graph.Consumer{ID: "adhoc:10.0.0.1", Kind: graph.ConsumerAdhocQuery, Name: "10.0.0.1"}
	old := graph.Reference{SignalID: "m", ConsumerID: con.ID, LastSeen: t0.Add(-time.Hour), Actor: "old"}
	newer := graph.Reference{SignalID: "m", ConsumerID: con.ID, LastSeen: t0, Actor: "new"}
	unknown := graph.Reference{SignalID: "m", ConsumerID: con.ID, Actor: "unknown-time"}

	for name, order := range map[string][]graph.Reference{
		"old first":     {old, newer, unknown},
		"newer first":   {newer, old, unknown},
		"unknown first": {unknown, old, newer},
	} {
		id := scan(t, s, t0, func(sc *Scan) {
			must(t, sc.PutSignal(ctx, sig))
			must(t, sc.PutConsumer(ctx, con))
			for _, r := range order {
				must(t, sc.PutReference(ctx, r))
			}
		})
		refs, _ := s.References(ctx, id)
		if len(refs) != 1 || !refs[0].LastSeen.Equal(t0) || refs[0].Actor != "new" {
			t.Errorf("%s: refs = %+v", name, refs)
		}
	}

	// LastViewed: a scan without view data keeps the earlier view.
	v := graph.Consumer{ID: "panel:x:1", Kind: graph.ConsumerDashboardPanel, Name: "x", LastViewed: t0}
	scan(t, s, t0, func(sc *Scan) { must(t, sc.PutConsumer(ctx, v)) })
	v.LastViewed = time.Time{}
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) { must(t, sc.PutConsumer(ctx, v)) })
	cons, _ := s.Consumers(ctx, id)
	if len(cons) != 1 || !cons[0].LastViewed.Equal(t0) {
		t.Errorf("last_viewed lost: %+v", cons)
	}
}

func TestInterruptedScanLeavesNothing(t *testing.T) {
	s, path := open(t)
	sc, err := s.BeginScan(ctx, t0, 30)
	must(t, err)
	must(t, sc.PutSignal(ctx, graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom"}))
	must(t, sc.Abort())
	must(t, sc.Abort()) // idempotent
	if _, err := s.LatestScan(ctx); !errors.Is(err, ErrNoScan) {
		t.Errorf("aborted scan visible: %v", err)
	}
	if err := sc.PutSignal(ctx, graph.Signal{ID: "n", Kind: graph.KindMetric, Name: "n", Source: "prom"}); err == nil {
		t.Error("put after abort accepted")
	}

	// A process that dies mid-scan never commits.
	sc, err = s.BeginScan(ctx, t0, 30)
	must(t, err)
	must(t, sc.PutSignal(ctx, graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom"}))
	sc.tx.Rollback() // what closing the connection without Finish amounts to
	s.Close()
	s2, err := Open(ctx, path)
	must(t, err)
	defer s2.Close()
	var n int
	must(t, s2.db.QueryRowContext(ctx, `SELECT count(*) FROM signals`).Scan(&n))
	if n != 0 {
		t.Errorf("%d signals left by an unfinished scan", n)
	}
}

func TestDanglingReferenceFailsFinish(t *testing.T) {
	s, _ := open(t)
	sc, err := s.BeginScan(ctx, t0, 30)
	must(t, err)
	must(t, sc.PutSignal(ctx, graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom"}))
	must(t, sc.PutReference(ctx, graph.Reference{SignalID: "m", ConsumerID: "missing"}))
	err = sc.Finish(ctx, 1)
	if err == nil || !strings.Contains(err.Error(), "missing signal or consumer") {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.LatestScan(ctx); !errors.Is(err, ErrNoScan) {
		t.Errorf("failed scan visible: %v", err)
	}
	// The store is still usable.
	scan(t, s, t0, func(sc *Scan) {})
}

func TestValidation(t *testing.T) {
	s, _ := open(t)
	if _, err := s.BeginScan(ctx, time.Time{}, 30); err == nil {
		t.Error("zero start accepted")
	}
	if _, err := s.BeginScan(ctx, t0, 0); err == nil {
		t.Error("zero window accepted")
	}

	sc, err := s.BeginScan(ctx, t0, 30)
	must(t, err)
	defer sc.Abort()
	bad := map[string]error{
		"unknown kind":   sc.PutSignal(ctx, graph.Signal{ID: "x", Kind: "histogram", Name: "x", Source: "prom"}),
		"no signal id":   sc.PutSignal(ctx, graph.Signal{Kind: graph.KindMetric, Name: "x", Source: "prom"}),
		"no source":      sc.PutSignal(ctx, graph.Signal{ID: "x", Kind: graph.KindMetric, Name: "x"}),
		"no consumer id": sc.PutConsumer(ctx, graph.Consumer{Kind: graph.ConsumerAlertRule}),
		"no ref ends":    sc.PutReference(ctx, graph.Reference{SignalID: "x"}),
		"no cost signal": sc.PutCost(ctx, graph.Cost{}),
	}
	for name, err := range bad {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := sc.Finish(ctx, 1.5); err == nil {
		t.Error("coverage 1.5 accepted")
	}
	if err := sc.Finish(ctx, 1); err == nil {
		t.Error("second Finish accepted")
	}
}

func TestOpenRefusesForeignOrNewerDatabases(t *testing.T) {
	dir := t.TempDir()

	other := filepath.Join(dir, "other.db")
	db, err := sql.Open("sqlite", other)
	must(t, err)
	_, err = db.Exec(`CREATE TABLE customers (id INTEGER)`)
	must(t, err)
	db.Close()
	if _, err := Open(ctx, other); err == nil || !strings.Contains(err.Error(), "not a tvt database") {
		t.Errorf("foreign db: err = %v", err)
	}

	newer := filepath.Join(dir, "newer.db")
	s, err := Open(ctx, newer)
	must(t, err)
	_, err = s.db.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`)
	must(t, err)
	s.Close()
	if _, err := Open(ctx, newer); err == nil || !strings.Contains(err.Error(), "newer than this tvt") {
		t.Errorf("newer db: err = %v", err)
	}

	// Reopening a current database is fine and keeps its data.
	cur := filepath.Join(dir, "current.db")
	s, err = Open(ctx, cur)
	must(t, err)
	scan(t, s, t0, func(sc *Scan) {})
	s.Close()
	s, err = Open(ctx, cur)
	must(t, err)
	defer s.Close()
	if _, err := s.LatestScan(ctx); err != nil {
		t.Errorf("reopen lost scan: %v", err)
	}
	var fk, jm string
	must(t, s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk))
	must(t, s.db.QueryRow(`PRAGMA journal_mode`).Scan(&jm))
	if fk != "1" || jm != "wal" {
		t.Errorf("pragmas: foreign_keys=%s journal_mode=%s", fk, jm)
	}
}

func TestOpenEmptyPath(t *testing.T) {
	if _, err := Open(ctx, ""); err == nil {
		t.Error("empty path accepted")
	}
}

func TestSourcesAndLoads(t *testing.T) {
	s, _ := open(t)
	since := t0.Add(-30 * 24 * time.Hour)
	srcs := []graph.SourceCoverage{
		{Source: "grafana router log", Configured: true, Coverage: 0.4, WindowStart: since},
		{Source: "prometheus query log", Configured: false, Coverage: 0, WindowStart: since},
	}
	loads := []graph.DashboardLoad{
		{OrgID: 1, DashboardUID: "shop-overview", Login: "admin", Browser: false, Loads: 2, LastAt: t0.Add(-2 * time.Hour)},
		{OrgID: 1, DashboardUID: "shop-overview", Login: "admin", Browser: true, Loads: 12, LastAt: t0},
	}
	id := scan(t, s, t0, func(sc *Scan) {
		for _, src := range srcs {
			must(t, sc.PutSource(ctx, src))
		}
		for _, l := range loads {
			must(t, sc.PutLoad(ctx, l))
		}
		if err := sc.PutSource(ctx, graph.SourceCoverage{Source: "x", Coverage: 2, WindowStart: since}); err == nil {
			t.Error("coverage 2 accepted")
		}
		if err := sc.PutLoad(ctx, graph.DashboardLoad{DashboardUID: "x", LastAt: t0}); err == nil {
			t.Error("zero loads accepted")
		}
	})
	gotSrcs, err := s.Sources(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(gotSrcs, srcs) {
		t.Errorf("sources\n got  %+v\n want %+v", gotSrcs, srcs)
	}
	gotLoads, err := s.Loads(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(gotLoads, loads) {
		t.Errorf("loads\n got  %+v\n want %+v", gotLoads, loads)
	}
}

func TestAssumedReferences(t *testing.T) {
	s, _ := open(t)
	sig := graph.Signal{ID: "loki:stream:{app=\"shop\"}", Kind: graph.KindStream, Name: `{app="shop"}`, Source: "loki"}
	con := graph.Consumer{ID: "rule:loki:a/b/c", Kind: graph.ConsumerAlertRule, Name: "c"}
	exact := graph.Reference{SignalID: sig.ID, ConsumerID: con.ID}
	assumed := graph.Reference{SignalID: sig.ID, ConsumerID: con.ID, Assumed: true}

	// Within a scan the broader, assumed reading wins whatever the order.
	for name, order := range map[string][]graph.Reference{
		"assumed first": {assumed, exact},
		"exact first":   {exact, assumed},
	} {
		id := scan(t, s, t0, func(sc *Scan) {
			must(t, sc.PutSignal(ctx, sig))
			must(t, sc.PutConsumer(ctx, con))
			for _, r := range order {
				must(t, sc.PutReference(ctx, r))
			}
		})
		refs, _ := s.References(ctx, id)
		if len(refs) != 1 || !refs[0].Assumed {
			t.Errorf("%s: refs = %+v, want one assumed", name, refs)
		}
	}

	// A later scan that resolves the reference exactly replaces it.
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, con))
		must(t, sc.PutReference(ctx, exact))
	})
	refs, _ := s.References(ctx, id)
	if len(refs) != 1 || refs[0].Assumed {
		t.Errorf("later exact scan: refs = %+v, want one exact", refs)
	}
}

func TestUpgradeFromVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	s, err := Open(ctx, path)
	must(t, err)
	scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom"}))
		must(t, sc.PutConsumer(ctx, graph.Consumer{ID: "c", Kind: graph.ConsumerAlertRule, Name: "c"}))
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: "m", ConsumerID: "c"}))
	})
	// Turn it into a version 1 database: no refs.assumed.
	for _, q := range []string{
		`ALTER TABLE refs DROP COLUMN assumed`,
		`UPDATE meta SET value = '1' WHERE key = 'schema_version'`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("open version 1 database: %v", err)
	}
	defer s.Close()
	var v string
	must(t, s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v))
	if v != "2" {
		t.Errorf("schema_version = %s, want 2", v)
	}
	info, err := s.LatestScan(ctx)
	must(t, err)
	refs, err := s.References(ctx, info.ID)
	must(t, err)
	if len(refs) != 1 || refs[0].Assumed {
		t.Errorf("refs after upgrade = %+v, want the old reference, not assumed", refs)
	}
	// And it takes new scans with assumed references.
	scan(t, s, t0.Add(time.Hour), func(sc *Scan) {
		must(t, sc.PutSignal(ctx, graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom"}))
		must(t, sc.PutConsumer(ctx, graph.Consumer{ID: "c", Kind: graph.ConsumerAlertRule, Name: "c"}))
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: "m", ConsumerID: "c", Assumed: true}))
	})
}
