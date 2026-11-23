package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/graph"
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
	ref := graph.Reference{Evidence: graph.EvidenceObserved, SignalID: sig.ID, ConsumerID: con.ID, LastSeen: t0.Add(-30 * time.Minute), Actor: "admin"}
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
	ref.FirstSeen = t0
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
		must(t, sc.PutReference(ctx, graph.Reference{Evidence: graph.EvidenceObserved, SignalID: "m", ConsumerID: "panel:d:1", LastSeen: t0, Actor: "ann"}))
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
	old := graph.Reference{Evidence: graph.EvidenceObserved, SignalID: "m", ConsumerID: con.ID, LastSeen: t0.Add(-time.Hour), Actor: "old"}
	newer := graph.Reference{Evidence: graph.EvidenceObserved, SignalID: "m", ConsumerID: con.ID, LastSeen: t0, Actor: "new"}
	unknown := graph.Reference{Evidence: graph.EvidenceObserved, SignalID: "m", ConsumerID: con.ID, Actor: "unknown-time"}

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
	must(t, sc.PutReference(ctx, graph.Reference{Evidence: graph.EvidenceObserved, SignalID: "m", ConsumerID: "missing"}))
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
		"no ref ends":    sc.PutReference(ctx, graph.Reference{Evidence: graph.EvidenceObserved, SignalID: "x"}),
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
	exact := graph.Reference{Evidence: graph.EvidenceObserved, SignalID: sig.ID, ConsumerID: con.ID}
	assumed := graph.Reference{Evidence: graph.EvidenceObserved, SignalID: sig.ID, ConsumerID: con.ID, Assumed: true}

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

// versionOneDB writes a database with one scan and turns it into schema
// version 1 (no refs.assumed). It returns the open store.
func versionOneDB(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(ctx, path)
	must(t, err)
	scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom"}))
		must(t, sc.PutConsumer(ctx, graph.Consumer{ID: "c", Kind: graph.ConsumerAlertRule, Name: "c"}))
		must(t, sc.PutReference(ctx, graph.Reference{Evidence: graph.EvidenceObserved, SignalID: "m", ConsumerID: "c"}))
	})
	for _, q := range append(append([]string{}, v8Drops...), []string{
		`ALTER TABLE refs DROP COLUMN assumed`,
		`ALTER TABLE refs DROP COLUMN evidence`,
		`ALTER TABLE refs DROP COLUMN labels`,
		`ALTER TABLE refs DROP COLUMN all_labels`,
		`ALTER TABLE refs DROP COLUMN reason`,
		`ALTER TABLE refs DROP COLUMN result_labels`,
		`ALTER TABLE scan_sources DROP COLUMN gap`,
		`ALTER TABLE consumers DROP COLUMN evaluated_by`,
		`ALTER TABLE consumers DROP COLUMN interval_ms`,
		`ALTER TABLE consumers DROP COLUMN datasources`,
		`ALTER TABLE signals DROP COLUMN class`,
		`ALTER TABLE signals DROP COLUMN dormant`,
		`ALTER TABLE signals DROP COLUMN owner`,
		`ALTER TABLE signals DROP COLUMN tier`,
		`ALTER TABLE consumers DROP COLUMN unparsed`,
		`UPDATE meta SET value = '1' WHERE key = 'schema_version'`,
	}...) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return s
}

func TestUpgradeFromVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	versionOneDB(t, path).Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open version 1 database: %v", err)
	}
	defer s.Close()
	var v string
	must(t, s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v))
	if v != strconv.Itoa(SchemaVersion) {
		t.Errorf("schema_version = %s, want %d", v, SchemaVersion)
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
		must(t, sc.PutReference(ctx, graph.Reference{Evidence: graph.EvidenceObserved, SignalID: "m", ConsumerID: "c", Assumed: true}))
	})
}

// An upgrade that stops after its ALTER commits (before migrate runs) must
// leave a database the next Open accepts, not one that reports version 1
// and fails on a duplicate column.
func TestUpgradeInterrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	s := versionOneDB(t, path)
	must(t, s.upgrade(ctx, 1))
	s.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open after interrupted upgrade: %v", err)
	}
	defer s.Close()
	info, err := s.LatestScan(ctx)
	must(t, err)
	refs, err := s.References(ctx, info.ID)
	must(t, err)
	if len(refs) != 1 {
		t.Errorf("refs = %+v, want the old reference", refs)
	}
}

func TestReferenceLabelDetail(t *testing.T) {
	s, _ := open(t)
	sig := graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom"}
	con := graph.Consumer{ID: "c", Kind: graph.ConsumerAlertRule, Name: "c"}
	a := graph.Reference{SignalID: "m", ConsumerID: "c", Evidence: graph.EvidenceDeclared,
		Labels:  []graph.LabelRef{{Name: "route", Use: graph.LabelGroup}, {Name: "job", Use: graph.LabelMatch}, {Name: "instance", Use: graph.LabelLegend}},
		Assumed: true, ResultLabels: true, Reasons: []string{"matcher value from $x"}}
	b := graph.Reference{SignalID: "m", ConsumerID: "c", Evidence: graph.EvidenceDeclared,
		Labels:    []graph.LabelRef{{Name: "job", Use: graph.LabelMatch}, {Name: "odd\nname", Use: graph.LabelWithout}},
		AllLabels: true, Reasons: []string{"query did not parse:\nline 2"}}

	// Within a scan, repeats merge: union of labels and reasons, and the
	// broader flags.
	id := scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, con))
		must(t, sc.PutReference(ctx, a))
		must(t, sc.PutReference(ctx, b))
	})
	refs, err := s.References(ctx, id)
	must(t, err)
	want := graph.Reference{SignalID: "m", ConsumerID: "c", Evidence: graph.EvidenceDeclared,
		Labels: []graph.LabelRef{
			{Name: "instance", Use: graph.LabelLegend}, {Name: "job", Use: graph.LabelMatch}, {Name: "odd\nname", Use: graph.LabelWithout}, {Name: "route", Use: graph.LabelGroup}},
		AllLabels: true, Assumed: true, ResultLabels: true,
		Reasons: []string{"matcher value from $x", "query did not parse: line 2"}, FirstSeen: t0}
	if len(refs) != 1 || !reflect.DeepEqual(refs[0], want) {
		t.Errorf("merged ref\n got  %+v\n want %+v", refs, want)
	}

	// A later scan replaces the detail with its own reading.
	id = scan(t, s, t0.Add(time.Hour), func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, con))
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: "m", ConsumerID: "c", Evidence: graph.EvidenceDeclared}))
	})
	refs, err = s.References(ctx, id)
	must(t, err)
	if len(refs) != 1 || refs[0].Labels != nil || refs[0].AllLabels || refs[0].Assumed || refs[0].ResultLabels || refs[0].Reasons != nil {
		t.Errorf("later scan: refs = %+v, want plain", refs)
	}

	// Evidence is required.
	scan(t, s, t0.Add(2*time.Hour), func(sc *Scan) {
		if err := sc.PutReference(ctx, graph.Reference{SignalID: "m", ConsumerID: "c"}); err == nil {
			t.Error("reference without evidence accepted")
		}
		if err := sc.PutReference(ctx, graph.Reference{SignalID: "m", ConsumerID: "c", Evidence: "seen"}); err == nil {
			t.Error("reference with evidence \"seen\" accepted")
		}
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, con))
	})
}

// A version 2 database: its assumed references covered every label, so the
// upgrade sets all_labels from assumed.
func TestUpgradeFromVersion2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	s, err := Open(ctx, path)
	must(t, err)
	scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom"}))
		must(t, sc.PutConsumer(ctx, graph.Consumer{ID: "c", Kind: graph.ConsumerAlertRule, Name: "c"}))
		must(t, sc.PutConsumer(ctx, graph.Consumer{ID: "d", Kind: graph.ConsumerAlertRule, Name: "d"}))
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: "m", ConsumerID: "c", Evidence: graph.EvidenceDeclared, Assumed: true}))
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: "m", ConsumerID: "d", Evidence: graph.EvidenceDeclared}))
	})
	for _, q := range append(append([]string{}, v8Drops...), []string{
		`ALTER TABLE refs DROP COLUMN evidence`,
		`ALTER TABLE refs DROP COLUMN labels`,
		`ALTER TABLE refs DROP COLUMN all_labels`,
		`ALTER TABLE refs DROP COLUMN reason`,
		`ALTER TABLE refs DROP COLUMN result_labels`,
		`ALTER TABLE scan_sources DROP COLUMN gap`,
		`ALTER TABLE consumers DROP COLUMN evaluated_by`,
		`ALTER TABLE consumers DROP COLUMN interval_ms`,
		`ALTER TABLE consumers DROP COLUMN datasources`,
		`ALTER TABLE signals DROP COLUMN class`,
		`ALTER TABLE signals DROP COLUMN dormant`,
		`ALTER TABLE signals DROP COLUMN owner`,
		`ALTER TABLE signals DROP COLUMN tier`,
		`ALTER TABLE consumers DROP COLUMN unparsed`,
		`UPDATE meta SET value = '2' WHERE key = 'schema_version'`,
	}...) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	info, err := s.LatestScan(ctx)
	must(t, err)
	refs, err := s.References(ctx, info.ID)
	must(t, err)
	if len(refs) != 2 || !refs[0].Assumed || !refs[0].AllLabels || refs[0].Evidence != "" || refs[1].AllLabels {
		t.Errorf("refs after upgrade = %+v, want c assumed and all labels, d neither, evidence unknown", refs)
	}
}

// A read seen once, long ago, outlives every later scan that doesn't see it.
func TestLastReadsNeverDecay(t *testing.T) {
	s, _ := open(t)
	sig := graph.Signal{ID: "prom:metric:m", Kind: graph.KindMetric, Name: "m", Source: "prom"}
	other := graph.Signal{ID: "prom:metric:n", Kind: graph.KindMetric, Name: "n", Source: "prom"}
	adhoc := graph.Consumer{ID: "qlog:prometheus:abc", Kind: graph.ConsumerAdhocQuery, Name: "ad hoc"}
	panel := graph.Consumer{ID: "panel:1:d:1:0", Kind: graph.ConsumerDashboardPanel, Name: "D / p"}
	nineMonths := t0.Add(-270 * 24 * time.Hour)
	scan(t, s, t0.Add(-269*24*time.Hour), func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, adhoc))
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: sig.ID, ConsumerID: adhoc.ID, Evidence: graph.EvidenceObserved,
			LastSeen: nineMonths, Actor: "alice"}))
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: sig.ID, ConsumerID: adhoc.ID, Evidence: graph.EvidenceObserved,
			LastSeen: nineMonths.Add(-time.Hour), Actor: "bob"}))
	})
	latest := scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutSignal(ctx, other))
		must(t, sc.PutConsumer(ctx, panel))
		// A dashboard reference has no last_seen: not a logged read.
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: other.ID, ConsumerID: panel.ID, Evidence: graph.EvidenceObserved}))
	})
	refs, err := s.References(ctx, latest)
	must(t, err)
	for _, r := range refs {
		if r.SignalID == sig.ID {
			t.Errorf("latest scan still lists the old read: %+v", r)
		}
	}
	reads, err := s.LastReads(ctx)
	must(t, err)
	want := []SignalRead{{SignalID: sig.ID, ConsumerID: adhoc.ID, Actor: "alice", At: nineMonths}}
	if !reflect.DeepEqual(reads, want) {
		t.Errorf("LastReads\n got  %+v\n want %+v", reads, want)
	}
}

// Version 3 databases gain scan_sources.gap; a source can record a gap.
func TestUpgradeFromVersion3AndGaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	s, err := Open(ctx, path)
	must(t, err)
	src := graph.SourceCoverage{Source: "prometheus query log", Configured: true, Coverage: 0.5, WindowStart: t0.Add(-time.Hour)}
	scan(t, s, t0, func(sc *Scan) { must(t, sc.PutSource(ctx, src)) })
	for _, q := range append(append([]string{}, v8Drops...), []string{
		`ALTER TABLE scan_sources DROP COLUMN gap`,
		`ALTER TABLE consumers DROP COLUMN evaluated_by`,
		`ALTER TABLE consumers DROP COLUMN interval_ms`,
		`ALTER TABLE consumers DROP COLUMN datasources`,
		`ALTER TABLE signals DROP COLUMN class`,
		`ALTER TABLE signals DROP COLUMN dormant`,
		`ALTER TABLE signals DROP COLUMN owner`,
		`ALTER TABLE signals DROP COLUMN tier`,
		`ALTER TABLE consumers DROP COLUMN unparsed`,
		`UPDATE meta SET value = '3' WHERE key = 'schema_version'`,
	}...) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	gapped := graph.SourceCoverage{Source: "mimir query log", Configured: true, WindowStart: t0, Gap: "probe marker not found"}
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) { must(t, sc.PutSource(ctx, gapped)) })
	got, err := s.Sources(ctx, id)
	must(t, err)
	if len(got) != 1 || got[0] != gapped {
		t.Errorf("sources = %+v, want %+v", got, gapped)
	}
	old, err := s.Sources(ctx, id-1)
	must(t, err)
	if len(old) != 1 || old[0].Gap != "" || old[0].Coverage != 0.5 {
		t.Errorf("upgraded sources = %+v", old)
	}
}

// Each signal gets its single latest read however many references it has;
// ties go to the first consumer by ID.
func TestLastReadsManyRefsAndTies(t *testing.T) {
	s, _ := open(t)
	a := graph.Signal{ID: "prom:metric:a", Kind: graph.KindMetric, Name: "a", Source: "prom"}
	b := graph.Signal{ID: "prom:metric:b", Kind: graph.KindMetric, Name: "b", Source: "prom"}
	scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, a))
		must(t, sc.PutSignal(ctx, b))
		for i := range 5 {
			c := graph.Consumer{ID: fmt.Sprintf("qlog:prometheus:%d", i), Kind: graph.ConsumerAdhocQuery, Name: "ad hoc"}
			must(t, sc.PutConsumer(ctx, c))
			// a: consumers 3 and 1 tie on the latest read.
			at := t0.Add(-time.Duration(5-i) * time.Hour)
			if i == 1 || i == 3 {
				at = t0
			}
			must(t, sc.PutReference(ctx, graph.Reference{SignalID: a.ID, ConsumerID: c.ID, Evidence: graph.EvidenceObserved,
				LastSeen: at, Actor: fmt.Sprintf("u%d", i)}))
			// b: consumer 2 read it last.
			must(t, sc.PutReference(ctx, graph.Reference{SignalID: b.ID, ConsumerID: c.ID, Evidence: graph.EvidenceObserved,
				LastSeen: t0.Add(-time.Duration((i-2)*(i-2)) * time.Minute), Actor: fmt.Sprintf("u%d", i)}))
		}
	})
	reads, err := s.LastReads(ctx)
	must(t, err)
	want := []SignalRead{
		{SignalID: a.ID, ConsumerID: "qlog:prometheus:1", Actor: "u1", At: t0},
		{SignalID: b.ID, ConsumerID: "qlog:prometheus:2", Actor: "u2", At: t0},
	}
	if !reflect.DeepEqual(reads, want) {
		t.Errorf("LastReads\n got  %+v\n want %+v", reads, want)
	}
}

// Version 4 databases gain the rule columns on consumers; old rows read
// back empty, and a Grafana-managed rule keeps who evaluates it, how often
// and which datasources it queries.
func TestUpgradeFromVersion4AndRuleColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	s, err := Open(ctx, path)
	must(t, err)
	old := graph.Consumer{ID: "rule:prometheus:f/g/A", Kind: graph.ConsumerAlertRule, Name: "A", Expr: "up == 0"}
	scan(t, s, t0, func(sc *Scan) { must(t, sc.PutConsumer(ctx, old)) })
	for _, q := range append(append([]string{}, v8Drops...), []string{
		`ALTER TABLE consumers DROP COLUMN evaluated_by`,
		`ALTER TABLE consumers DROP COLUMN interval_ms`,
		`ALTER TABLE consumers DROP COLUMN datasources`,
		`ALTER TABLE signals DROP COLUMN class`,
		`ALTER TABLE signals DROP COLUMN dormant`,
		`ALTER TABLE signals DROP COLUMN owner`,
		`ALTER TABLE signals DROP COLUMN tier`,
		`ALTER TABLE consumers DROP COLUMN unparsed`,
		`UPDATE meta SET value = '4' WHERE key = 'schema_version'`,
	}...) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	got, err := s.Consumers(ctx, 1)
	must(t, err)
	if len(got) != 1 || !reflect.DeepEqual(got[0], old) {
		t.Errorf("upgraded consumers = %+v, want %+v", got, old)
	}
	grafana := graph.Consumer{ID: "rule:grafana:folder/g/uid", Kind: graph.ConsumerAlertRule, Name: "Lag", Expr: "A: x\nB: y",
		EvaluatedBy: "grafana", Interval: 30 * time.Second, Datasources: []string{"loki", "prometheus"}}
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) { must(t, sc.PutConsumer(ctx, grafana)) })
	got, err = s.Consumers(ctx, id)
	must(t, err)
	if len(got) != 1 || !reflect.DeepEqual(got[0], grafana) {
		t.Errorf("consumers = %+v, want %+v", got, grafana)
	}
}

// Version 5 databases gain signal classification; old rows read back with
// no class, and a scan stores class, dormant reason, owner and tier.
func TestUpgradeFromVersion5AndClasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	s, err := Open(ctx, path)
	must(t, err)
	old := graph.Signal{ID: "prom:metric:m", Kind: graph.KindMetric, Name: "m", Source: "prom", FirstSeen: t0}
	scan(t, s, t0, func(sc *Scan) { must(t, sc.PutSignal(ctx, old)) })
	for _, q := range append(append([]string{}, v8Drops...), []string{
		`ALTER TABLE signals DROP COLUMN class`,
		`ALTER TABLE signals DROP COLUMN dormant`,
		`ALTER TABLE signals DROP COLUMN owner`,
		`ALTER TABLE signals DROP COLUMN tier`,
		`ALTER TABLE consumers DROP COLUMN unparsed`,
		`UPDATE meta SET value = '5' WHERE key = 'schema_version'`,
	}...) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	got, err := s.Signals(ctx, 1)
	must(t, err)
	if len(got) != 1 || !reflect.DeepEqual(got[0], old) {
		t.Errorf("upgraded signals = %+v, want %+v", got, old)
	}
	tier := 0
	classified := graph.Signal{ID: "prom:metric:m", Kind: graph.KindMetric, Name: "m", Source: "prom", FirstSeen: t0,
		Class: "dormant", Dormant: "tier 0 service", Owner: "payments", Tier: &tier}
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) { must(t, sc.PutSignal(ctx, classified)) })
	got, err = s.Signals(ctx, id)
	must(t, err)
	if len(got) != 1 || !reflect.DeepEqual(got[0], classified) {
		t.Errorf("signals = %+v, want %+v", got, classified)
	}
	// An unknown class is refused by the schema.
	bad := classified
	bad.Class = "maybe"
	sc, err := s.BeginScan(ctx, t0.Add(2*time.Hour), 30)
	must(t, err)
	if err := sc.PutSignal(ctx, bad); err == nil {
		t.Error("class maybe was stored")
	}
	sc.Abort()
}

// v22Drops turns a current database's columns back into version 21's
// (versions 16-21 only added tables, which CREATE TABLE IF NOT EXISTS adds).
var v22Drops = append(append([]string{}, v23Drops...),
	`ALTER TABLE costs DROP COLUMN merges`,
	`ALTER TABLE costs DROP COLUMN summable`,
)

// v23Drops turns a current database's tables back into version 22's.
var v23Drops = append(append([]string{}, v24Drops...),
	`ALTER TABLE signals DROP COLUMN record`,
)

// v24Drops turns a current database's tables back into version 23's.
var v24Drops = append(append([]string{}, v25Drops...),
	`ALTER TABLE pipeline_targets DROP COLUMN jobs`,
)

// v31Drops turns a current database's tables back into version 30's.
var v31Drops = []string{
	`DROP TABLE stream_attrs`,
}

// v30Drops turns a current database's tables back into version 29's.
var v30Drops = append(append([]string{}, v31Drops...),
	`DROP TABLE collector_sent`,
)

// v29Drops turns a current database's tables back into version 28's.
var v29Drops = append(append([]string{}, v30Drops...),
	`DROP TABLE merges`,
)

// v28Drops turns a current database's tables back into version 27's.
var v28Drops = append(append([]string{}, v29Drops...),
	`ALTER TABLE costs DROP COLUMN classes`,
)

// v27Drops turns a current database's tables back into version 26's.
var v27Drops = append(append([]string{}, v28Drops...),
	`ALTER TABLE stream_volumes DROP COLUMN levels`,
)

// v26Drops turns a current database's tables back into version 25's.
var v26Drops = append(append([]string{}, v27Drops...),
	`ALTER TABLE signals DROP COLUMN owner_source`,
	`ALTER TABLE signals DROP COLUMN owner_inferred`,
)

// v25Drops turns a current database's tables back into version 24's.
var v25Drops = append(append([]string{}, v26Drops...),
	`ALTER TABLE consumers DROP COLUMN for_ms`,
	`ALTER TABLE consumers DROP COLUMN severity`,
)

// v15Drops turns a current database's tables back into version 14's
// (v22Drops first).
var v15Drops = append(append([]string{}, v22Drops...),
	`ALTER TABLE refs DROP COLUMN first_scan`,
)

// v14Drops turns a current database's tables back into version 13's
// (v15Drops first).
var v14Drops = append(append([]string{}, v15Drops...),
	`DROP TABLE scores`,
)

// v13Drops turns a current database's tables back into version 12's
// (v14Drops first).
var v13Drops = append(append([]string{}, v14Drops...),
	`ALTER TABLE refs DROP COLUMN sweep`,
)

// v12Drops turns a current database's tables back into version 11's
// (v13Drops first).
var v12Drops = append(append([]string{}, v13Drops...),
	`ALTER TABLE consumers DROP COLUMN client`,
)

// v11Drops turns a current database's tables back into version 10's
// (v12Drops first).
var v11Drops = append(append([]string{}, v12Drops...),
	`ALTER TABLE scans DROP COLUMN tsdb_measured`,
	`ALTER TABLE scans DROP COLUMN tsdb_bytes_per_sample`,
	`ALTER TABLE scans DROP COLUMN tsdb_head_series`,
	`ALTER TABLE scans DROP COLUMN tsdb_head_bytes`,
	`ALTER TABLE scans DROP COLUMN tsdb_blocks_bytes`,
	`ALTER TABLE scans DROP COLUMN tsdb_retention_ms`,
	`ALTER TABLE scans DROP COLUMN tsdb_retention_bytes`,
)

// v10Drops turns a current database's tables back into version 9's
// (v11Drops first).
var v10Drops = append(append([]string{}, v11Drops...),
	`DROP TABLE stream_volumes`,
	`ALTER TABLE scans DROP COLUMN volume_truncated`,
)

// v9Drops turns a current database's tables back into version 8's
// (v10Drops first).
var v9Drops = append(append([]string{}, v10Drops...),
	`ALTER TABLE scans DROP COLUMN price_kind`,
	`ALTER TABLE scans DROP COLUMN bytes_per_sample`,
	`ALTER TABLE scans DROP COLUMN bytes_per_sample_from`,
	`ALTER TABLE scans DROP COLUMN retention_ms`,
	`ALTER TABLE scans DROP COLUMN retention_from`,
	`ALTER TABLE scans DROP COLUMN head_bytes_per_series`,
	`ALTER TABLE costs DROP COLUMN disk_bytes`,
)

// v8Drops turns a current database's tables back into version 7's, for the
// upgrade tests that start from an older version (v9Drops first).
var v8Drops = append(append([]string{}, v9Drops...),
	`ALTER TABLE scans DROP COLUMN pricecard`,
	`ALTER TABLE scans DROP COLUMN currency`,
	`ALTER TABLE scans DROP COLUMN price_preset`,
	`ALTER TABLE costs DROP COLUMN interval_ms`,
	`ALTER TABLE costs DROP COLUMN interval_assumed`,
	`ALTER TABLE costs DROP COLUMN buckets`,
	`ALTER TABLE costs DROP COLUMN exemplars`,
)

// Version 7 databases gain the scan's price card and the cost columns for
// scrape intervals and histograms; old cost rows keep their numbers.
func TestUpgradeFromVersion7AndCostColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	s, err := Open(ctx, path)
	must(t, err)
	sig := graph.Signal{ID: "prom:metric:up", Kind: graph.KindMetric, Name: "up", Source: "prom"}
	old := graph.Cost{SignalID: sig.ID, ActiveSeries: 6, MonthlyCost: 0.5, ObservedAt: t0}
	scan(t, s, t0, func(sc *Scan) { must(t, sc.PutSignal(ctx, sig)); must(t, sc.PutCost(ctx, old)) })
	for _, q := range append(append([]string{}, v8Drops...), `UPDATE meta SET value = '7' WHERE key = 'schema_version'`) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	got, err := s.Costs(ctx, 1)
	must(t, err)
	if len(got) != 1 || got[0].MonthlyCost != 0.5 || got[0].Interval != 0 || got[0].Buckets != 0 {
		t.Errorf("upgraded costs = %+v", got)
	}
	bucket := graph.Signal{ID: "prom:metric:h_bucket", Kind: graph.KindMetric, Name: "h_bucket", Source: "prom"}
	c := graph.Cost{SignalID: bucket.ID, ActiveSeries: 120, MonthlyCost: 0.78, ObservedAt: t0.Add(time.Hour),
		Interval: 15 * time.Second, IntervalAssumed: "no targets API", Buckets: 12, Exemplars: true}
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) {
		must(t, sc.PutSignal(ctx, bucket))
		must(t, sc.PutCost(ctx, c))
		must(t, sc.SetPriceCard(ctx, "grafana-cloud-pro", "USD", true, "vendor"))
	})
	got, err = s.Costs(ctx, id)
	must(t, err)
	c.ScanID = id
	if len(got) != 1 || !reflect.DeepEqual(got[0], c) {
		t.Errorf("costs = %+v, want %+v", got, c)
	}
	info, err := s.LatestScan(ctx)
	must(t, err)
	if info.PriceCard != "grafana-cloud-pro" || info.Currency != "USD" || !info.PricePreset {
		t.Errorf("scan price card = %+v", info)
	}
}

// Version 6 databases gain consumers.unparsed (Sprint 2 item 19): old rows
// read back with none, and a scan stores why a panel couldn't be read.
func TestUpgradeFromVersion6AndUnparsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.db")
	s, err := Open(ctx, path)
	must(t, err)
	old := graph.Consumer{ID: "panel:1:d:1:0", Kind: graph.ConsumerDashboardPanel, Name: "D / p", Expr: "up"}
	scan(t, s, t0, func(sc *Scan) { must(t, sc.PutConsumer(ctx, old)) })
	for _, q := range append(append([]string{}, v8Drops...), []string{
		`ALTER TABLE consumers DROP COLUMN unparsed`,
		`UPDATE meta SET value = '6' WHERE key = 'schema_version'`,
	}...) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	got, err := s.Consumers(ctx, 1)
	must(t, err)
	if len(got) != 1 || !reflect.DeepEqual(got[0], old) {
		t.Errorf("upgraded consumers = %+v, want %+v", got, old)
	}
	flame := graph.Consumer{ID: "panel:1:d:2:0", Kind: graph.ConsumerDashboardPanel, Name: "D / flame",
		Expr: `process_cpu:cpu:nanoseconds:cpu:nanoseconds{service_name="shop"}`, Unparsed: "datasource type grafana-pyroscope-datasource"}
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) { must(t, sc.PutConsumer(ctx, flame)) })
	got, err = s.Consumers(ctx, id)
	must(t, err)
	if len(got) != 1 || !reflect.DeepEqual(got[0], flame) {
		t.Errorf("consumers = %+v, want %+v", got, flame)
	}
}

// Version 8 databases gain the price card's kind, the self-hosted model's
// inputs and costs.disk_bytes; a card already recorded was a vendor card.
func TestUpgradeFromVersion8AndSelfHosted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")
	s, err := Open(ctx, path)
	must(t, err)
	sig := graph.Signal{ID: "prom:metric:up", Kind: graph.KindMetric, Name: "up", Source: "prom"}
	scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutCost(ctx, graph.Cost{SignalID: sig.ID, ActiveSeries: 6, MonthlyCost: 0.5, ObservedAt: t0}))
	})
	if _, err := s.db.Exec(`UPDATE scans SET pricecard = 'grafana-cloud', currency = 'USD', price_preset = 1`); err != nil {
		t.Fatal(err)
	}
	for _, q := range append(append([]string{}, v9Drops...), `UPDATE meta SET value = '8' WHERE key = 'schema_version'`) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	info, err := s.LatestScan(ctx)
	must(t, err)
	if info.PriceKind != "vendor" || info.SelfHosted != nil {
		t.Errorf("upgraded scan = %+v", info)
	}
	c := graph.Cost{SignalID: sig.ID, ActiveSeries: 6, MonthlyCost: 0.02, ObservedAt: t0.Add(time.Hour), DiskBytes: 123456789}
	sh := SelfHosted{BytesPerSample: 0.62, BytesPerSampleFrom: "measured", Retention: 45 * 24 * time.Hour, RetentionFrom: "measured", HeadBytesPerSeries: 4000}
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutCost(ctx, c))
		must(t, sc.SetPriceCard(ctx, "on-prem", "USD", false, "self_hosted"))
		must(t, sc.SetSelfHosted(ctx, sh))
	})
	got, err := s.Costs(ctx, id)
	must(t, err)
	if len(got) != 1 || got[0].DiskBytes != c.DiskBytes {
		t.Errorf("costs = %+v", got)
	}
	info, err = s.LatestScan(ctx)
	must(t, err)
	if info.PriceKind != "self_hosted" || info.SelfHosted == nil || *info.SelfHosted != sh {
		t.Errorf("scan = %+v %+v", info, info.SelfHosted)
	}
}

// Version 9 databases gain stream_volumes and scans.volume_truncated.
func TestUpgradeFromVersion9AndStreamVolumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.db")
	s, err := Open(ctx, path)
	must(t, err)
	scan(t, s, t0, func(sc *Scan) {})
	for _, q := range append(append([]string{}, v10Drops...), `UPDATE meta SET value = '9' WHERE key = 'schema_version'`) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	if info, err := s.LatestScan(ctx); err != nil || info.VolumeTruncated {
		t.Fatalf("upgraded scan = %+v, %v", info, err)
	}
	vols := []graph.StreamVolume{{Selector: `{app="a"}`, BytesPerDay: 5}, {Selector: `{app="b"}`, BytesPerDay: 0}}
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) {
		for _, v := range vols {
			must(t, sc.PutStreamVolume(ctx, v))
		}
		must(t, sc.SetVolumeTruncated(ctx))
		if err := sc.PutStreamVolume(ctx, graph.StreamVolume{Selector: `{app="c"}`, BytesPerDay: -1}); err == nil {
			t.Error("negative bytes: no error")
		}
	})
	got, err := s.StreamVolumes(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(got, vols) {
		t.Errorf("volumes = %+v", got)
	}
	if info, _ := s.LatestScan(ctx); !info.VolumeTruncated {
		t.Error("truncation not recorded")
	}
}

// Version 10 databases gain the scan's TSDB measurements.
func TestUpgradeFromVersion10AndTSDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v10.db")
	s, err := Open(ctx, path)
	must(t, err)
	scan(t, s, t0, func(sc *Scan) {})
	for _, q := range append(append([]string{}, v11Drops...), `UPDATE meta SET value = '10' WHERE key = 'schema_version'`) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	if info, err := s.LatestScan(ctx); err != nil || info.TSDB != nil {
		t.Fatalf("upgraded scan = %+v, %v", info, err)
	}
	want := collect.Storage{BytesPerSample: 0.62, HeadSeries: 60239, HeadBytes: 2.4e8, BlocksBytes: 2.3e9,
		RetentionTime: 45 * 24 * time.Hour, RetentionBytes: 21474836480}
	scan(t, s, t0.Add(time.Hour), func(sc *Scan) { must(t, sc.SetTSDB(ctx, want)) })
	info, err := s.LatestScan(ctx)
	must(t, err)
	if info.TSDB == nil || *info.TSDB != want {
		t.Errorf("tsdb = %+v", info.TSDB)
	}
}

// Version 11 databases gain consumers.client.
func TestUpgradeFromVersion11AndClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v11.db")
	s, err := Open(ctx, path)
	must(t, err)
	old := graph.Consumer{ID: "qlog:prometheus:abc", Kind: graph.ConsumerAdhocQuery, Name: "q", Expr: "up"}
	scan(t, s, t0, func(sc *Scan) { must(t, sc.PutConsumer(ctx, old)) })
	for _, q := range append(append([]string{}, v12Drops...), `UPDATE meta SET value = '11' WHERE key = 'schema_version'`) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	got, err := s.Consumers(ctx, 1)
	must(t, err)
	if len(got) != 1 || got[0].Client != "" {
		t.Fatalf("upgraded consumers = %+v", got)
	}
	c := graph.Consumer{ID: "qlog:prometheus:agent:finance:abc", Kind: graph.ConsumerAgentQuery, Name: "agent finance", Expr: "up", Client: "agent"}
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) { must(t, sc.PutConsumer(ctx, c)) })
	got, err = s.Consumers(ctx, id)
	must(t, err)
	if len(got) != 1 || got[0].Client != "agent" {
		t.Errorf("consumers = %+v", got)
	}
}

// Version 12 databases gain refs.sweep; within one scan, a named read of a
// signal wins over the same consumer's sweep of it.
func TestUpgradeFromVersion12AndSweep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v12.db")
	s, err := Open(ctx, path)
	must(t, err)
	sig := graph.Signal{ID: "prom:metric:up", Kind: graph.KindMetric, Name: "up", Source: "prom"}
	con := graph.Consumer{ID: "qlog:prometheus:abc", Kind: graph.ConsumerAdhocQuery, Name: "q", Expr: "up"}
	ref := graph.Reference{SignalID: sig.ID, ConsumerID: con.ID, Evidence: graph.EvidenceObserved}
	scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, con))
		must(t, sc.PutReference(ctx, ref))
	})
	for _, q := range append(append([]string{}, v13Drops...), `UPDATE meta SET value = '12' WHERE key = 'schema_version'`) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	got, err := s.References(ctx, 1)
	must(t, err)
	if len(got) != 1 || got[0].Sweep {
		t.Fatalf("upgraded refs = %+v", got)
	}
	swept := ref
	swept.Sweep = true
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, con))
		must(t, sc.PutReference(ctx, swept))
	})
	if got, _ = s.References(ctx, id); len(got) != 1 || !got[0].Sweep {
		t.Errorf("swept refs = %+v", got)
	}
	id = scan(t, s, t0.Add(2*time.Hour), func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, con))
		must(t, sc.PutReference(ctx, swept))
		must(t, sc.PutReference(ctx, ref))
	})
	if got, _ = s.References(ctx, id); len(got) != 1 || got[0].Sweep {
		t.Errorf("named read after sweep = %+v", got)
	}
}

// Version 13 databases gain scores; a scan's scores read back highest
// priority first, and bad rows are refused.
func TestUpgradeFromVersion13AndScores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v13.db")
	s, err := Open(ctx, path)
	must(t, err)
	a := graph.Signal{ID: "prom:metric:a", Kind: graph.KindMetric, Name: "a", Source: "prom", Class: "candidate"}
	b := graph.Signal{ID: "prom:metric:b", Kind: graph.KindMetric, Name: "b", Source: "prom", Class: "candidate"}
	scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, a))
	})
	for _, q := range append(append([]string{}, v14Drops...), `UPDATE meta SET value = '13' WHERE key = 'schema_version'`) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	if got, err := s.Scores(ctx, 1); err != nil || len(got) != 0 {
		t.Fatalf("upgraded scores = %+v, %v", got, err)
	}
	want := []graph.Score{
		{SignalID: b.ID, MonthlyCost: 40, UsageWeight: 0.25, Strongest: "sweep", Confidence: 1, Priority: 32},
		{SignalID: a.ID, MonthlyCost: 10, UsageWeight: 0, Strongest: "none", Confidence: 0, Priority: 0, Held: "window"},
	}
	id := scan(t, s, t0.Add(time.Hour), func(sc *Scan) {
		must(t, sc.PutSignal(ctx, a))
		must(t, sc.PutSignal(ctx, b))
		must(t, sc.PutScore(ctx, want[1]))
		must(t, sc.PutScore(ctx, want[0]))
	})
	got, err := s.Scores(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Scores\n got  %+v\n want %+v", got, want)
	}

	sc, err := s.BeginScan(ctx, t0.Add(2*time.Hour), 30)
	must(t, err)
	defer sc.Abort()
	must(t, sc.PutSignal(ctx, a))
	for _, bad := range []graph.Score{
		{SignalID: a.ID},
		{SignalID: a.ID, Strongest: "none", Confidence: 1.5},
		{SignalID: a.ID, Strongest: "none", Held: "maybe"},
		{SignalID: a.ID, Strongest: "none", UsageWeight: -1},
	} {
		if err := sc.PutScore(ctx, bad); err == nil {
			t.Errorf("stored bad score %+v", bad)
		}
	}
}

// A sweep over every metric name is not a read: LastReads skips it, so it
// can't make an unread signal look recently read.
func TestLastReadsSkipSweeps(t *testing.T) {
	s, _ := open(t)
	sig := graph.Signal{ID: "prom:metric:m", Kind: graph.KindMetric, Name: "m", Source: "prom"}
	named := graph.Consumer{ID: "qlog:prometheus:named", Kind: graph.ConsumerAdhocQuery, Name: "q", Expr: "m"}
	sweep := graph.Consumer{ID: "qlog:prometheus:agent:sweep:1", Kind: graph.ConsumerAgentQuery, Name: "sweep", Expr: `count by (__name__) ({__name__=~".+"})`}
	scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, named))
		must(t, sc.PutConsumer(ctx, sweep))
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: sig.ID, ConsumerID: named.ID, Evidence: graph.EvidenceObserved, LastSeen: t0.Add(-48 * time.Hour)}))
		must(t, sc.PutReference(ctx, graph.Reference{SignalID: sig.ID, ConsumerID: sweep.ID, Evidence: graph.EvidenceObserved, LastSeen: t0.Add(-time.Hour), Sweep: true}))
	})
	reads, err := s.LastReads(ctx)
	must(t, err)
	if len(reads) != 1 || reads[0].ConsumerID != named.ID || !reads[0].At.Equal(t0.Add(-48*time.Hour)) {
		t.Errorf("LastReads = %+v", reads)
	}
}

// A reference's first sighting stays the scan that first stored it, however
// often later scans see it again.
func TestReferenceFirstSeen(t *testing.T) {
	s, _ := open(t)
	sig := graph.Signal{ID: "m", Kind: graph.KindMetric, Name: "m", Source: "prom"}
	con := graph.Consumer{ID: "c", Kind: graph.ConsumerAlertRule, Name: "c"}
	ref := graph.Reference{SignalID: "m", ConsumerID: "c", Evidence: graph.EvidenceDeclared}
	put := func(sc *Scan) {
		must(t, sc.PutSignal(ctx, sig))
		must(t, sc.PutConsumer(ctx, con))
		must(t, sc.PutReference(ctx, ref))
	}
	scan(t, s, t0, put)
	id := scan(t, s, t0.Add(48*time.Hour), put)
	refs, err := s.References(ctx, id)
	must(t, err)
	if len(refs) != 1 || !refs[0].FirstSeen.Equal(t0) {
		t.Errorf("refs = %+v, want first seen %v", refs, t0)
	}
}

// Line findings (schema v16) round-trip, most bytes first, and a finding
// without a stream or pattern, or out of range, is refused.
func TestLineFindings(t *testing.T) {
	s, _ := open(t)
	want := []graph.LineFinding{
		{Stream: `{app="shop", level="warn"}`, Pattern: `msg="x" key=y`, Kind: graph.LineRepeated, Lines: 10000, Window: 90 * time.Second, Share: 0.55, BytesPerDay: 9e6, MonthlyCost: 1.5},
		{Stream: `{app="shop", level="info"}`, Pattern: `route=/healthz status=200 replica=<_>`, Kind: graph.LineHandoff, Lines: 300000, Window: 3 * time.Hour, Share: 0.6, BytesPerDay: 3e9, MonthlyCost: 40},
	}
	id := scan(t, s, t0, func(sc *Scan) {
		for _, f := range want {
			must(t, sc.PutLineFinding(ctx, f))
		}
		for _, bad := range []graph.LineFinding{
			{Pattern: "p", Kind: graph.LineRepeated},
			{Stream: "s", Kind: graph.LineRepeated},
			{Stream: "s", Pattern: "p", Kind: "line_filter"},
			{Stream: "s", Pattern: "q", Kind: graph.LineRepeated, Share: 1.5},
		} {
			if err := sc.PutLineFinding(ctx, bad); err == nil {
				t.Errorf("line finding %+v accepted", bad)
			}
		}
	})
	got, err := s.LineFindings(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(got, []graph.LineFinding{want[1], want[0]}) {
		t.Errorf("line findings\n got %+v\nwant %+v", got, []graph.LineFinding{want[1], want[0]})
	}
	if got, err := s.LineFindings(ctx, id+1); err != nil || len(got) != 0 {
		t.Errorf("another scan's findings: %+v %v", got, err)
	}
}

// Rule costs and findings (schema v17) round-trip; a finding of an unknown
// kind, a negative cost or a rule that isn't a stored consumer is refused.
func TestRuleCostsAndFindings(t *testing.T) {
	s, _ := open(t)
	con := graph.Consumer{ID: "rule:prometheus:/g/A", Kind: graph.ConsumerAlertRule, Name: "A"}
	costs := []graph.RuleCost{{ConsumerID: con.ID, EvalsPerDay: 2880, SamplesPerEval: 12.5, Evaluations: 40}}
	findings := []graph.RuleFinding{
		{ConsumerID: con.ID, Kind: graph.RuleDuplicate, Detail: "same expression as B (g)", Fix: "keep one"},
		{ConsumerID: con.ID, Kind: graph.RuleLongRange, Detail: "a [1d] range", Fix: "evaluate it less often"},
	}
	id := scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutConsumer(ctx, con))
		for _, c := range costs {
			must(t, sc.PutRuleCost(ctx, c))
		}
		for _, f := range findings {
			must(t, sc.PutRuleFinding(ctx, f))
		}
		if err := sc.PutRuleFinding(ctx, graph.RuleFinding{ConsumerID: con.ID, Kind: "rewrite_rule"}); err == nil {
			t.Error("unknown finding kind accepted")
		}
		if err := sc.PutRuleCost(ctx, graph.RuleCost{ConsumerID: "rule:prometheus:/g/B", EvalsPerDay: -1}); err == nil {
			t.Error("negative cost accepted")
		}
		if err := sc.PutRuleCost(ctx, graph.RuleCost{}); err == nil {
			t.Error("cost without a rule accepted")
		}
	})
	gotCosts, err := s.RuleCosts(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(gotCosts, costs) {
		t.Errorf("rule costs %+v, want %+v", gotCosts, costs)
	}
	gotFindings, err := s.RuleFindings(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(gotFindings, findings) {
		t.Errorf("rule findings %+v, want %+v", gotFindings, findings)
	}
	sc, err := s.BeginScan(ctx, t0.Add(time.Hour), 30)
	must(t, err)
	defer sc.Abort()
	must(t, sc.PutRuleCost(ctx, graph.RuleCost{ConsumerID: "rule:nobody", EvalsPerDay: 1}))
	if err := sc.Finish(ctx, 1); err == nil {
		t.Error("a cost for a rule that isn't a consumer committed")
	}
}

func TestQueryCosts(t *testing.T) {
	s, _ := open(t)
	a := graph.Consumer{ID: "qlog:prometheus:agent:finance:abc", Kind: graph.ConsumerAgentQuery, Name: "agent finance", Client: "agent"}
	b := graph.Consumer{ID: "qlog:loki:def", Kind: graph.ConsumerAdhocQuery, Name: "ad hoc", Client: "api"}
	costs := []graph.QueryCost{{ConsumerID: b.ID, Queries: 0.5, Bytes: 4096}, {ConsumerID: a.ID, Queries: 2, Samples: 120}} // by consumer ID
	id := scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutConsumer(ctx, a))
		must(t, sc.PutConsumer(ctx, b))
		for _, c := range costs {
			must(t, sc.PutQueryCost(ctx, c))
		}
		if err := sc.PutQueryCost(ctx, graph.QueryCost{ConsumerID: a.ID, Samples: -1}); err == nil {
			t.Error("negative cost accepted")
		}
		if err := sc.PutQueryCost(ctx, graph.QueryCost{}); err == nil {
			t.Error("cost without a consumer accepted")
		}
	})
	got, err := s.QueryCosts(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(got, costs) {
		t.Errorf("query costs %+v, want %+v", got, costs)
	}
	sc, err := s.BeginScan(ctx, t0.Add(time.Hour), 30)
	must(t, err)
	defer sc.Abort()
	must(t, sc.PutQueryCost(ctx, graph.QueryCost{ConsumerID: "qlog:nobody", Queries: 1}))
	if err := sc.Finish(ctx, 1); err == nil {
		t.Error("a cost for a consumer that isn't stored committed")
	}
}

func TestSLOFindings(t *testing.T) {
	s, _ := open(t)
	findings := []graph.SLOFinding{
		{Kind: graph.SLOPagingNotBurnRate, Subject: "rule:prometheus:shop.yml/g/A", Name: "A", Detail: "pages", Fix: "burn rate"},
		{Kind: graph.SLOServiceWithout, Subject: "service:reconciler", Name: "reconciler", Detail: "no SLO", Fix: "define one"},
		{Kind: graph.SLOWithoutAlert, Subject: "slo:sloth:x.yml:shop/a", Name: "shop/a", Detail: "declares B", Fix: "load it"},
	}
	id := scan(t, s, t0, func(sc *Scan) {
		for _, f := range findings {
			must(t, sc.PutSLOFinding(ctx, f))
		}
		if err := sc.PutSLOFinding(ctx, graph.SLOFinding{Kind: "rewrite_alert", Subject: "x"}); err == nil {
			t.Error("unknown kind accepted")
		}
		if err := sc.PutSLOFinding(ctx, graph.SLOFinding{Kind: graph.SLOWithoutAlert}); err == nil {
			t.Error("finding without a subject accepted")
		}
	})
	got, err := s.SLOFindings(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(got, findings) {
		t.Errorf("SLO findings %+v, want %+v", got, findings)
	}
}

func TestPipelineTargets(t *testing.T) {
	s, _ := open(t)
	targets := []graph.PipelineTarget{
		{Subject: "prom:metric:a", Pipeline: "alloy-apps"},
		{Subject: "prom:metric:b", Note: "claimed by several pipelines: x, y"},
		{Subject: `stream:{app="r"}`, Note: "no pipeline's handles match"},
	}
	id := scan(t, s, t0, func(sc *Scan) {
		for _, pt := range targets {
			must(t, sc.PutPipelineTarget(ctx, pt))
		}
		if err := sc.PutPipelineTarget(ctx, graph.PipelineTarget{Subject: "prom:metric:c"}); err == nil {
			t.Error("unmapped target without a note accepted")
		}
		if err := sc.PutPipelineTarget(ctx, graph.PipelineTarget{Subject: "prom:metric:a", Pipeline: "other"}); err == nil {
			t.Error("a second target for one subject accepted")
		}
	})
	got, err := s.PipelineTargets(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(got, targets) {
		t.Errorf("pipeline targets %+v, want %+v", got, targets)
	}
}

func TestOverflowFindings(t *testing.T) {
	s, _ := open(t)
	findings := []graph.OverflowFinding{
		{Signal: "prom:metric:a", Series: 1, Capped: []string{"customer_id", "span_name"}, Readers: []string{"qlog:prom:x\tcustomer_id"}},
		{Signal: "prom:metric:b_bucket", Series: 7, Capped: []string{"customer_id"}},
	}
	id := scan(t, s, t0, func(sc *Scan) {
		for _, f := range findings {
			must(t, sc.PutOverflowFinding(ctx, f))
		}
		if err := sc.PutOverflowFinding(ctx, graph.OverflowFinding{Signal: "prom:metric:c"}); err == nil {
			t.Error("finding without series accepted")
		}
	})
	got, err := s.OverflowFindings(ctx, id)
	must(t, err)
	if !reflect.DeepEqual(got, findings) {
		t.Errorf("overflow findings %+v, want %+v", got, findings)
	}
}

// Label drop safety (S6 item 7): merges round-trip, unknown stays unknown,
// and a version 21 database's rows read as unknown, never as "no merges".
func TestCostMerges(t *testing.T) {
	s, path := open(t)
	id := scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, graph.Signal{ID: "prom:metric:m", Kind: graph.KindMetric, Name: "m", Source: "prom"}))
		for _, l := range []string{"a", "b", "c"} {
			must(t, sc.PutSignal(ctx, graph.Signal{ID: "prom:label:m:" + l, Kind: graph.KindLabel, Name: l, Parent: "m", Source: "prom"}))
		}
		must(t, sc.PutCost(ctx, graph.Cost{SignalID: "prom:label:m:a", Merges: 0, MergesKnown: true, Summable: true}))
		must(t, sc.PutCost(ctx, graph.Cost{SignalID: "prom:label:m:b", Merges: 12, MergesKnown: true, Classes: 3}))
		must(t, sc.PutCost(ctx, graph.Cost{SignalID: "prom:label:m:c"}))
	})
	costs, err := s.Costs(ctx, id)
	must(t, err)
	got := map[string]graph.Cost{}
	for _, c := range costs {
		got[c.SignalID] = c
	}
	if a := got["prom:label:m:a"]; !a.MergesKnown || a.Merges != 0 || !a.Summable {
		t.Errorf("a %+v", a)
	}
	if b := got["prom:label:m:b"]; !b.MergesKnown || b.Merges != 12 || b.Summable || b.Classes != 3 {
		t.Errorf("b %+v", b)
	}
	if c := got["prom:label:m:c"]; c.MergesKnown || c.Merges != 0 {
		t.Errorf("c %+v: want unknown", c)
	}
	s.Close()

	// Back to version 21, then upgrade: the old rows are unknown.
	raw, err := sql.Open("sqlite", path)
	must(t, err)
	for _, q := range append(v22Drops, `UPDATE meta SET value = '21' WHERE key = 'schema_version'`) {
		_, err := raw.Exec(q)
		must(t, err)
	}
	raw.Close()
	s, err = Open(ctx, path)
	must(t, err)
	defer s.Close()
	costs, err = s.Costs(ctx, id)
	must(t, err)
	for _, c := range costs {
		if c.MergesKnown || c.Summable {
			t.Errorf("upgraded %s: %+v, want unknown", c.SignalID, c)
		}
	}
}

// TestRecordUpgrade (schema 23, S7 item 2): a version 22 database gains
// signals.record, empty for old rows, and a record round-trips.
func TestRecordUpgrade(t *testing.T) {
	s, path := open(t)
	id := scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, graph.Signal{ID: "loki:stream:{component=\"audit\"}", Kind: graph.KindStream, Name: `{component="audit"}`, Source: "loki", Record: "named like a record"}))
	})
	sigs, err := s.Signals(ctx, id)
	must(t, err)
	if len(sigs) != 1 || sigs[0].Record != "named like a record" {
		t.Fatalf("signals %+v", sigs)
	}
	s.Close()
	raw, err := sql.Open("sqlite", path)
	must(t, err)
	for _, q := range append(append([]string{}, v23Drops...), `UPDATE meta SET value = '22' WHERE key = 'schema_version'`) {
		_, err := raw.Exec(q)
		must(t, err)
	}
	raw.Close()
	s2, err := Open(ctx, path)
	must(t, err)
	defer s2.Close()
	if sigs, err = s2.Signals(ctx, id); err != nil || len(sigs) != 1 || sigs[0].Record != "" {
		t.Fatalf("upgraded signals %+v, %v: want one, not a record", sigs, err)
	}
}

// TestPipelineJobsUpgrade: a pipeline target's jobs round-trip (S7 item
// 3), and a version 23 database upgrades with no jobs recorded.
func TestPipelineJobsUpgrade(t *testing.T) {
	s, path := open(t)
	id := scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutSignal(ctx, graph.Signal{ID: "prom:metric:a", Kind: graph.KindMetric, Name: "a", Source: "prom"}))
		must(t, sc.PutPipelineTarget(ctx, graph.PipelineTarget{Subject: "prom:metric:a", Pipeline: "scrape", Jobs: []string{"node", "shop"}}))
	})
	ts, err := s.PipelineTargets(ctx, id)
	must(t, err)
	if len(ts) != 1 || strings.Join(ts[0].Jobs, ",") != "node,shop" {
		t.Fatalf("targets %+v", ts)
	}
	s.Close()
	raw, err := sql.Open("sqlite", path)
	must(t, err)
	for _, q := range append(append([]string{}, v24Drops...), `UPDATE meta SET value = '23' WHERE key = 'schema_version'`) {
		_, err := raw.Exec(q)
		must(t, err)
	}
	raw.Close()
	s2, err := Open(ctx, path)
	must(t, err)
	defer s2.Close()
	if ts, err = s2.PipelineTargets(ctx, id); err != nil || len(ts) != 1 || ts[0].Jobs != nil || ts[0].Pipeline != "scrape" {
		t.Fatalf("upgraded targets %+v, %v: want one, with no jobs", ts, err)
	}
}

// TestStreamLevels (schema 27, S10 item 3): a stream's per-level bytes
// round-trip, a stream without them reads back nil, and a version 26
// database gains the column, empty for its rows.
func TestStreamLevels(t *testing.T) {
	s, path := open(t)
	id := scan(t, s, t0, func(sc *Scan) {
		must(t, sc.PutStreamVolume(ctx, graph.StreamVolume{Selector: `{service_name="coding-agent"}`, BytesPerDay: 1000,
			Levels: map[string]int64{"debug": 700, "info": 290, "warn": 10}}))
		must(t, sc.PutStreamVolume(ctx, graph.StreamVolume{Selector: `{app="shop", level="info"}`, BytesPerDay: 50}))
	})
	vs, err := s.StreamVolumes(ctx, id)
	must(t, err)
	if len(vs) != 2 || vs[1].Levels["debug"] != 700 || len(vs[1].Levels) != 3 || vs[0].Levels != nil {
		t.Fatalf("volumes %+v", vs)
	}
	s.Close()
	raw, err := sql.Open("sqlite", path)
	must(t, err)
	for _, q := range append(append([]string{}, v27Drops...), `UPDATE meta SET value = '26' WHERE key = 'schema_version'`) {
		_, err := raw.Exec(q)
		must(t, err)
	}
	raw.Close()
	s2, err := Open(ctx, path)
	must(t, err)
	defer s2.Close()
	if vs, err = s2.StreamVolumes(ctx, id); err != nil || len(vs) != 2 || vs[1].Levels != nil {
		t.Fatalf("upgraded volumes %+v, %v: want both, no levels", vs, err)
	}
}

// TestMerges: a merged tvt change is recorded by the first scan that sees
// it; later scans' copies are ignored (S11 item 1).
func TestMerges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tvt.db")
	s, err := Open(ctx, path)
	must(t, err)
	defer s.Close()
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	m := graph.Merge{Repo: "github.com/o/p", ProposalScan: 3, Recommendation: "drop:prom:metric:x", SignalID: "prom:metric:x", MergedAt: at, Expected: 1.5}
	var first int64
	for i := 0; i < 2; i++ {
		sc, err := s.BeginScan(ctx, at.Add(time.Duration(i+1)*time.Hour), 30)
		must(t, err)
		if i == 0 {
			first = sc.ID
			must(t, sc.PutCollectorSent(ctx, graph.CollectorSent{Pipeline: "agent", Exporter: "otlp_http/loki", Signal: "logs", Sent: 42}))
		}
		must(t, sc.PutMerge(ctx, m))
		must(t, sc.Finish(ctx, 1))
	}
	got, err := s.Merges(ctx)
	must(t, err)
	sent, err := s.CollectorSent(ctx)
	must(t, err)
	if len(sent) != 1 || len(sent[first]) != 1 || sent[first][0].Sent != 42 || sent[first][0].Exporter != "otlp_http/loki" {
		t.Errorf("collector sent %v", sent)
	}
	if len(got) != 1 || got[0].FirstScan != first || !got[0].MergedAt.Equal(at) || got[0].Expected != 1.5 || got[0].SignalID != "prom:metric:x" {
		t.Fatalf("merges %+v (first scan %d)", got, first)
	}
}
