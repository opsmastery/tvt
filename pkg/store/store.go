// Package store persists the usage graph in SQLite (pure Go,
// modernc.org/sqlite, no CGO).
//
// A scan writes the whole current graph in one transaction (BeginScan …
// Finish), so readers only ever see complete scans and an interrupted scan
// leaves nothing behind. Every row carries the id of the last scan that saw
// it; the graph as of a scan is the rows with that id.
//
// The store is meant for one process (the CLI). It holds a single
// connection, so don't read through the Store while a Scan of the same
// Store is open in the same goroutine: the read waits for the scan.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// SchemaVersion is the version of graph.Schema this package writes.
const SchemaVersion = 7

// ErrNoScan means the database holds no completed scan yet.
var ErrNoScan = errors.New("store: no completed scan")

// Store is an open usage graph database.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and brings its schema up to
// date. It refuses a database written by a newer tvt, and a SQLite file
// that isn't a tvt database.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	// A plain path (not a file: URI) so spaces and '#' need no escaping;
	// the driver strips the query string and applies the pragmas.
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var tables int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
		return err
	}
	var hasMeta int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'`).Scan(&hasMeta); err != nil {
		return err
	}
	if tables > 0 && hasMeta == 0 {
		return errors.New("not a tvt database (tables present but no meta table)")
	}
	if hasMeta == 1 {
		var v string
		err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("bad schema_version %q", v)
			}
			if n > SchemaVersion {
				return fmt.Errorf("database schema version %d is newer than this tvt supports (%d); upgrade tvt", n, SchemaVersion)
			}
			if n < SchemaVersion {
				if err := s.upgrade(ctx, n); err != nil {
					return fmt.Errorf("upgrade schema from version %d: %w", n, err)
				}
			}
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, graph.Schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('schema_version', ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, strconv.Itoa(SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// upgrade brings a database at schema version from up to the columns that
// graph.Schema's CREATE TABLE IF NOT EXISTS statements can't add to an
// existing table. Each ALTER commits together with the version it reaches,
// so an upgrade interrupted at any point leaves a database whose recorded
// version matches its columns and the next Open resumes from there. migrate
// then applies the schema.
func (s *Store) upgrade(ctx context.Context, from int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if from < 2 {
		if _, err := tx.ExecContext(ctx,
			`ALTER TABLE refs ADD COLUMN assumed INTEGER NOT NULL DEFAULT 0 CHECK (assumed IN (0, 1))`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE meta SET value = '2' WHERE key = 'schema_version'`); err != nil {
			return err
		}
	}
	if from < 3 {
		for _, q := range []string{
			`ALTER TABLE refs ADD COLUMN evidence TEXT NOT NULL DEFAULT '' CHECK (evidence IN ('', 'declared', 'incident', 'observed'))`,
			`ALTER TABLE refs ADD COLUMN labels TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE refs ADD COLUMN all_labels INTEGER NOT NULL DEFAULT 0 CHECK (all_labels IN (0, 1))`,
			`ALTER TABLE refs ADD COLUMN reason TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE refs ADD COLUMN result_labels INTEGER NOT NULL DEFAULT 0 CHECK (result_labels IN (0, 1))`,
			// Before version 3 an assumed reference covered every label.
			`UPDATE refs SET all_labels = assumed`,
			`UPDATE meta SET value = '3' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 4 {
		for _, q := range []string{
			`ALTER TABLE scan_sources ADD COLUMN gap TEXT NOT NULL DEFAULT ''`,
			`UPDATE meta SET value = '4' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 5 {
		for _, q := range []string{
			`ALTER TABLE consumers ADD COLUMN evaluated_by TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE consumers ADD COLUMN interval_ms INTEGER`,
			`ALTER TABLE consumers ADD COLUMN datasources TEXT NOT NULL DEFAULT ''`,
			`UPDATE meta SET value = '5' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 6 {
		for _, q := range []string{
			`ALTER TABLE signals ADD COLUMN class TEXT NOT NULL DEFAULT '' CHECK (class IN ('', 'hard_keep', 'keep_high', 'dormant', 'candidate'))`,
			`ALTER TABLE signals ADD COLUMN dormant TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE signals ADD COLUMN owner TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE signals ADD COLUMN tier INTEGER`,
			`UPDATE meta SET value = '6' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 7 {
		for _, q := range []string{
			`ALTER TABLE consumers ADD COLUMN unparsed TEXT NOT NULL DEFAULT ''`,
			`UPDATE meta SET value = '7' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ScanInfo describes one completed scan.
type ScanInfo struct {
	ID          int64
	StartedAt   time.Time
	FinishedAt  time.Time
	WindowDays  int
	LogCoverage float64
}

// LatestScan returns the most recent completed scan, or ErrNoScan.
func (s *Store) LatestScan(ctx context.Context) (ScanInfo, error) {
	scans, err := s.scans(ctx, `ORDER BY id DESC LIMIT 1`)
	if err != nil {
		return ScanInfo{}, err
	}
	if len(scans) == 0 {
		return ScanInfo{}, ErrNoScan
	}
	return scans[0], nil
}

// Scans returns every completed scan, oldest first.
func (s *Store) Scans(ctx context.Context) ([]ScanInfo, error) {
	return s.scans(ctx, `ORDER BY id`)
}

func (s *Store) scans(ctx context.Context, tail string) ([]ScanInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, started_at, finished_at, window_days, log_coverage FROM scans `+tail)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScanInfo
	for rows.Next() {
		var si ScanInfo
		var started, finished int64
		if err := rows.Scan(&si.ID, &started, &finished, &si.WindowDays, &si.LogCoverage); err != nil {
			return nil, err
		}
		si.StartedAt, si.FinishedAt = fromMillis(started), fromMillis(finished)
		out = append(out, si)
	}
	return out, rows.Err()
}

// Signals returns the signals seen in scan scanID, ordered by ID.
func (s *Store) Signals(ctx context.Context, scanID int64) ([]graph.Signal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, name, parent, source, first_seen, class, dormant, owner, tier FROM signals WHERE last_scan = ? ORDER BY id`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.Signal
	for rows.Next() {
		var sg graph.Signal
		var kind string
		var first int64
		var tier sql.NullInt64
		if err := rows.Scan(&sg.ID, &kind, &sg.Name, &sg.Parent, &sg.Source, &first, &sg.Class, &sg.Dormant, &sg.Owner, &tier); err != nil {
			return nil, err
		}
		sg.Kind, sg.FirstSeen = graph.SignalKind(kind), fromMillis(first)
		if tier.Valid {
			t := int(tier.Int64)
			sg.Tier = &t
		}
		out = append(out, sg)
	}
	return out, rows.Err()
}

// Consumers returns the consumers seen in scan scanID, ordered by ID.
func (s *Store) Consumers(ctx context.Context, scanID int64) ([]graph.Consumer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, name, expr, team, last_viewed, evaluated_by, interval_ms, datasources, unparsed FROM consumers WHERE last_scan = ? ORDER BY id`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.Consumer
	for rows.Next() {
		var c graph.Consumer
		var kind string
		var viewed, interval sql.NullInt64
		var dss string
		if err := rows.Scan(&c.ID, &kind, &c.Name, &c.Expr, &c.Team, &viewed, &c.EvaluatedBy, &interval, &dss, &c.Unparsed); err != nil {
			return nil, err
		}
		c.Kind, c.LastViewed = graph.ConsumerKind(kind), fromNullMillis(viewed)
		if interval.Valid {
			c.Interval = time.Duration(interval.Int64) * time.Millisecond
		}
		if dss != "" {
			c.Datasources = strings.Split(dss, "\n")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// References returns the references seen in scan scanID, ordered by
// signal, consumer and via.
func (s *Store) References(ctx context.Context, scanID int64) ([]graph.Reference, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT signal_id, consumer_id, via, last_seen, actor, assumed, evidence, labels, all_labels, reason, result_labels FROM refs WHERE last_scan = ?
		 ORDER BY signal_id, consumer_id, via`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.Reference
	for rows.Next() {
		var r graph.Reference
		var seen sql.NullInt64
		var evidence, labels, reasons string
		if err := rows.Scan(&r.SignalID, &r.ConsumerID, &r.Via, &seen, &r.Actor, &r.Assumed,
			&evidence, &labels, &r.AllLabels, &reasons, &r.ResultLabels); err != nil {
			return nil, err
		}
		r.LastSeen = fromNullMillis(seen)
		r.Evidence = graph.EvidenceClass(evidence)
		if r.Labels, err = decodeLabels(labels); err != nil {
			return nil, fmt.Errorf("reference %s -> %s: %w", r.SignalID, r.ConsumerID, err)
		}
		r.Reasons = decodeReasons(reasons)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SignalRead is the most recent time a query log showed a signal being
// read: by which consumer, and by whom when the log recorded it.
type SignalRead struct {
	SignalID   string
	ConsumerID string
	Actor      string
	At         time.Time
}

// LastReads returns, for every signal a query log ever showed being read,
// its most recent read across all scans, ordered by signal. References
// are kept after the scan that saw them, and their last_seen only moves
// forward, so a read that has left every log since (rotated away, the
// window moved on) still shows here: "last read nine months ago" never
// decays into "never read".
func (s *Store) LastReads(ctx context.Context) ([]SignalRead, error) {
	rows, err := s.db.QueryContext(ctx,
		// The latest read per signal is computed once and joined back,
		// not looked up per reference: refs are kept forever, so a
		// correlated max would grow quadratically with a signal's history.
		`SELECT r.signal_id, r.consumer_id, r.actor, r.last_seen FROM refs r
		 JOIN (SELECT signal_id, max(last_seen) AS latest FROM refs
		       WHERE last_seen IS NOT NULL GROUP BY signal_id) l
		   ON l.signal_id = r.signal_id AND r.last_seen = l.latest
		 ORDER BY r.signal_id, r.consumer_id, r.via`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignalRead
	for rows.Next() {
		var r SignalRead
		var at int64
		if err := rows.Scan(&r.SignalID, &r.ConsumerID, &r.Actor, &at); err != nil {
			return nil, err
		}
		r.At = fromMillis(at)
		if n := len(out); n > 0 && out[n-1].SignalID == r.SignalID {
			continue // a tie: keep the first consumer
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Costs returns the costs observed in scan scanID, ordered by signal.
func (s *Store) Costs(ctx context.Context, scanID int64) ([]graph.Cost, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT signal_id, scan_id, observed_at, active_series, distinct_values, bytes_per_day, spans_per_day, monthly_cost
		 FROM costs WHERE scan_id = ? ORDER BY signal_id`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.Cost
	for rows.Next() {
		var c graph.Cost
		var observed int64
		if err := rows.Scan(&c.SignalID, &c.ScanID, &observed, &c.ActiveSeries, &c.DistinctValues, &c.BytesPerDay, &c.SpansPerDay, &c.MonthlyCost); err != nil {
			return nil, err
		}
		c.ObservedAt = fromMillis(observed)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Sources returns the coverage of each evidence source in scan scanID,
// ordered by source.
func (s *Store) Sources(ctx context.Context, scanID int64) ([]graph.SourceCoverage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source, configured, coverage, window_start, gap FROM scan_sources WHERE scan_id = ? ORDER BY source`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.SourceCoverage
	for rows.Next() {
		var src graph.SourceCoverage
		var configured int
		var start int64
		if err := rows.Scan(&src.Source, &configured, &src.Coverage, &start, &src.Gap); err != nil {
			return nil, err
		}
		src.Configured, src.WindowStart = configured == 1, fromMillis(start)
		out = append(out, src)
	}
	return out, rows.Err()
}

// Loads returns the dashboard loads recorded in scan scanID, ordered by
// org, dashboard, login and kind.
func (s *Store) Loads(ctx context.Context, scanID int64) ([]graph.DashboardLoad, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT org_id, dashboard_uid, login, browser, loads, last_at FROM dashboard_loads WHERE scan_id = ?
		 ORDER BY org_id, dashboard_uid, login, browser`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.DashboardLoad
	for rows.Next() {
		var l graph.DashboardLoad
		var browser int
		var last int64
		if err := rows.Scan(&l.OrgID, &l.DashboardUID, &l.Login, &browser, &l.Loads, &last); err != nil {
			return nil, err
		}
		l.Browser, l.LastAt = browser == 1, fromMillis(last)
		out = append(out, l)
	}
	return out, rows.Err()
}

// Scan is an open scan: a transaction that writes one complete graph.
// Call Finish to commit it or Abort to discard it; deferring Abort right
// after BeginScan is safe, since Abort after Finish does nothing.
type Scan struct {
	ID int64

	tx      *sql.Tx
	started time.Time
	done    bool
	signal  *sql.Stmt
	cons    *sql.Stmt
	ref     *sql.Stmt
	cost    *sql.Stmt
	source  *sql.Stmt
	load    *sql.Stmt
}

// BeginScan starts a scan that began at startedAt with an observation
// window of windowDays.
func (s *Store) BeginScan(ctx context.Context, startedAt time.Time, windowDays int) (*Scan, error) {
	if startedAt.IsZero() {
		return nil, errors.New("store: scan start time is zero")
	}
	if windowDays < 1 {
		return nil, fmt.Errorf("store: window of %d days", windowDays)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	sc := &Scan{tx: tx, started: startedAt}
	fail := func(err error) (*Scan, error) {
		tx.Rollback()
		return nil, fmt.Errorf("store: begin scan: %w", err)
	}
	// finished_at stays 0 until Finish; nobody sees the row before commit.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO scans (started_at, finished_at, window_days) VALUES (?, 0, ?)`,
		millis(startedAt), windowDays)
	if err != nil {
		return fail(err)
	}
	if sc.ID, err = res.LastInsertId(); err != nil {
		return fail(err)
	}
	for _, p := range []struct {
		dst **sql.Stmt
		sql string
	}{
		{&sc.signal, `INSERT INTO signals (id, kind, name, parent, source, first_seen, last_scan, class, dormant, owner, tier)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				kind = excluded.kind, name = excluded.name, parent = excluded.parent,
				source = excluded.source, class = excluded.class, dormant = excluded.dormant,
				owner = excluded.owner, tier = excluded.tier,
				first_seen = min(signals.first_seen, excluded.first_seen),
				last_scan = excluded.last_scan`},
		{&sc.cons, `INSERT INTO consumers (id, kind, name, expr, team, last_viewed, last_scan, evaluated_by, interval_ms, datasources, unparsed)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				kind = excluded.kind, name = excluded.name, expr = excluded.expr, team = excluded.team,
				evaluated_by = excluded.evaluated_by, interval_ms = excluded.interval_ms, datasources = excluded.datasources,
				unparsed = excluded.unparsed,
				last_viewed = CASE
					WHEN consumers.last_viewed IS NULL THEN excluded.last_viewed
					WHEN excluded.last_viewed IS NULL THEN consumers.last_viewed
					ELSE max(consumers.last_viewed, excluded.last_viewed) END,
				last_scan = excluded.last_scan`},
		{&sc.ref, `INSERT INTO refs (signal_id, consumer_id, via, last_seen, actor, assumed, evidence, labels, all_labels, reason, result_labels, last_scan)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (signal_id, consumer_id, via) DO UPDATE SET
				assumed = CASE
					WHEN refs.last_scan = excluded.last_scan THEN max(refs.assumed, excluded.assumed)
					ELSE excluded.assumed END,
				all_labels = CASE
					WHEN refs.last_scan = excluded.last_scan THEN max(refs.all_labels, excluded.all_labels)
					ELSE excluded.all_labels END,
				result_labels = CASE
					WHEN refs.last_scan = excluded.last_scan THEN max(refs.result_labels, excluded.result_labels)
					ELSE excluded.result_labels END,
				labels = CASE
					WHEN refs.last_scan <> excluded.last_scan OR refs.labels = '' THEN excluded.labels
					WHEN excluded.labels = '' OR excluded.labels = refs.labels THEN refs.labels
					ELSE refs.labels || char(10) || excluded.labels END,
				reason = CASE
					WHEN refs.last_scan <> excluded.last_scan OR refs.reason = '' THEN excluded.reason
					WHEN excluded.reason = '' OR excluded.reason = refs.reason THEN refs.reason
					ELSE refs.reason || char(10) || excluded.reason END,
				evidence = excluded.evidence,
				actor = CASE
					WHEN refs.last_seen IS NULL OR excluded.last_seen >= refs.last_seen THEN excluded.actor
					ELSE refs.actor END,
				last_seen = CASE
					WHEN refs.last_seen IS NULL THEN excluded.last_seen
					WHEN excluded.last_seen IS NULL THEN refs.last_seen
					ELSE max(refs.last_seen, excluded.last_seen) END,
				last_scan = excluded.last_scan`},
		{&sc.cost, `INSERT INTO costs (signal_id, scan_id, observed_at, active_series, distinct_values, bytes_per_day, spans_per_day, monthly_cost)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (signal_id, scan_id) DO UPDATE SET
				observed_at = excluded.observed_at, active_series = excluded.active_series,
				distinct_values = excluded.distinct_values,
				bytes_per_day = excluded.bytes_per_day, spans_per_day = excluded.spans_per_day,
				monthly_cost = excluded.monthly_cost`},
		{&sc.source, `INSERT INTO scan_sources (scan_id, source, configured, coverage, window_start, gap)
			VALUES (?, ?, ?, ?, ?, ?)`},
		{&sc.load, `INSERT INTO dashboard_loads (scan_id, org_id, dashboard_uid, login, browser, loads, last_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`},
	} {
		if *p.dst, err = tx.PrepareContext(ctx, p.sql); err != nil {
			return fail(err)
		}
	}
	return sc, nil
}

func (sc *Scan) check() error {
	if sc.done {
		return errors.New("store: scan already finished or aborted")
	}
	return nil
}

// PutSignal records a signal seen in this scan. A zero FirstSeen means the
// scan's start time. An existing signal keeps its earliest FirstSeen.
func (sc *Scan) PutSignal(ctx context.Context, s graph.Signal) error {
	if err := sc.check(); err != nil {
		return err
	}
	if s.ID == "" || s.Name == "" || s.Source == "" {
		return fmt.Errorf("store: signal %q: ID, Name and Source are required", s.ID)
	}
	first := s.FirstSeen
	if first.IsZero() {
		first = sc.started
	}
	var tier sql.NullInt64
	if s.Tier != nil {
		tier = sql.NullInt64{Int64: int64(*s.Tier), Valid: true}
	}
	_, err := sc.signal.ExecContext(ctx, s.ID, string(s.Kind), s.Name, s.Parent, s.Source, millis(first), sc.ID,
		s.Class, s.Dormant, s.Owner, tier)
	return wrap("signal", s.ID, err)
}

// PutConsumer records a consumer seen in this scan. LastViewed keeps the
// latest value across scans.
func (sc *Scan) PutConsumer(ctx context.Context, c graph.Consumer) error {
	if err := sc.check(); err != nil {
		return err
	}
	if c.ID == "" || c.Kind == "" {
		return fmt.Errorf("store: consumer %q: ID and Kind are required", c.ID)
	}
	var interval sql.NullInt64
	if c.Interval > 0 {
		interval = sql.NullInt64{Int64: c.Interval.Milliseconds(), Valid: true}
	}
	_, err := sc.cons.ExecContext(ctx, c.ID, string(c.Kind), c.Name, c.Expr, c.Team, nullMillis(c.LastViewed), sc.ID,
		c.EvaluatedBy, interval, strings.Join(c.Datasources, "\n"), c.Unparsed)
	return wrap("consumer", c.ID, err)
}

// PutReference records a reference seen in this scan. Repeats (the same
// query many times in a log) keep the latest LastSeen and its Actor. The
// signal and consumer may be put before or after; both must exist by Finish.
// Within one scan the broader reading wins: a reference put once as
// Assumed, AllLabels or ResultLabels stays so, and its Labels and Reasons are the union
// of every put. A later scan replaces them with its own reading.
func (sc *Scan) PutReference(ctx context.Context, r graph.Reference) error {
	if err := sc.check(); err != nil {
		return err
	}
	if r.SignalID == "" || r.ConsumerID == "" {
		return errors.New("store: reference: SignalID and ConsumerID are required")
	}
	if !r.Evidence.Valid() {
		return fmt.Errorf("store: reference %s -> %s: evidence %q, want declared, incident or observed", r.SignalID, r.ConsumerID, r.Evidence)
	}
	labels, err := encodeLabels(r.Labels)
	if err != nil {
		return fmt.Errorf("store: reference %s -> %s: %w", r.SignalID, r.ConsumerID, err)
	}
	_, err = sc.ref.ExecContext(ctx, r.SignalID, r.ConsumerID, r.Via, nullMillis(r.LastSeen), r.Actor, boolInt(r.Assumed),
		string(r.Evidence), labels, boolInt(r.AllLabels), encodeReasons(r.Reasons), boolInt(r.ResultLabels), sc.ID)
	return wrap("reference", r.SignalID+" -> "+r.ConsumerID, err)
}

// PutCost records a signal's cost in this scan. A zero ObservedAt means the
// scan's start time. c.ScanID is ignored.
func (sc *Scan) PutCost(ctx context.Context, c graph.Cost) error {
	if err := sc.check(); err != nil {
		return err
	}
	if c.SignalID == "" {
		return errors.New("store: cost: SignalID is required")
	}
	observed := c.ObservedAt
	if observed.IsZero() {
		observed = sc.started
	}
	_, err := sc.cost.ExecContext(ctx, c.SignalID, sc.ID, millis(observed),
		c.ActiveSeries, c.DistinctValues, c.BytesPerDay, c.SpansPerDay, c.MonthlyCost)
	return wrap("cost", c.SignalID, err)
}

// PutSource records one usage-evidence source's coverage. Each source is
// put once per scan.
func (sc *Scan) PutSource(ctx context.Context, src graph.SourceCoverage) error {
	if err := sc.check(); err != nil {
		return err
	}
	if src.Source == "" || src.WindowStart.IsZero() {
		return errors.New("store: source: Source and WindowStart are required")
	}
	if src.Coverage < 0 || src.Coverage > 1 {
		return fmt.Errorf("store: source %q: coverage %v outside 0..1", src.Source, src.Coverage)
	}
	_, err := sc.source.ExecContext(ctx, sc.ID, src.Source, boolInt(src.Configured), src.Coverage, millis(src.WindowStart), src.Gap)
	return wrap("source", src.Source, err)
}

// PutLoad records aggregated dashboard loads. Each (org, dashboard, login,
// browser) is put once per scan; the caller aggregates.
func (sc *Scan) PutLoad(ctx context.Context, l graph.DashboardLoad) error {
	if err := sc.check(); err != nil {
		return err
	}
	if l.DashboardUID == "" || l.LastAt.IsZero() || l.Loads < 1 {
		return fmt.Errorf("store: load of %q: DashboardUID, LastAt and Loads are required", l.DashboardUID)
	}
	_, err := sc.load.ExecContext(ctx, sc.ID, l.OrgID, l.DashboardUID, l.Login, boolInt(l.Browser), l.Loads, millis(l.LastAt))
	return wrap("load", l.DashboardUID, err)
}

// Finish commits the scan with its log coverage (0..1). A reference or cost
// pointing at a signal or consumer that doesn't exist fails the commit and
// discards the scan.
func (sc *Scan) Finish(ctx context.Context, logCoverage float64) error {
	if err := sc.check(); err != nil {
		return err
	}
	sc.done = true
	if logCoverage < 0 || logCoverage > 1 {
		sc.tx.Rollback()
		return fmt.Errorf("store: log coverage %v outside 0..1", logCoverage)
	}
	if _, err := sc.tx.ExecContext(ctx,
		`UPDATE scans SET finished_at = ?, log_coverage = ? WHERE id = ?`,
		millis(time.Now()), logCoverage, sc.ID); err != nil {
		sc.tx.Rollback()
		return fmt.Errorf("store: finish scan: %w", err)
	}
	if err := sc.tx.Commit(); err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return fmt.Errorf("store: finish scan: a reference or cost points at a missing signal or consumer: %w", err)
		}
		return fmt.Errorf("store: finish scan: %w", err)
	}
	return nil
}

// Abort discards the scan. It does nothing after Finish or a previous Abort.
func (sc *Scan) Abort() error {
	if sc.done {
		return nil
	}
	sc.done = true
	return sc.tx.Rollback()
}

func wrap(what, id string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("store: %s %q: %w", what, id, err)
}

func millis(t time.Time) int64 { return t.UnixMilli() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullMillis(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func fromNullMillis(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return fromMillis(n.Int64)
}

// encodeLabels stores label uses one per line as "use:name", sorted. A name
// that would break the format (a newline, or a leading quote) is quoted.
func encodeLabels(ls []graph.LabelRef) (string, error) {
	lines := make([]string, 0, len(ls))
	for _, l := range ls {
		if l.Name == "" || l.Use == "" || strings.ContainsAny(string(l.Use), ":\n") {
			return "", fmt.Errorf("bad label use %+v", l)
		}
		name := l.Name
		if strings.ContainsAny(name, "\n\r") || strings.HasPrefix(name, `"`) {
			name = strconv.Quote(name)
		}
		lines = append(lines, string(l.Use)+":"+name)
	}
	sort.Strings(lines)
	return strings.Join(slices.Compact(lines), "\n"), nil
}

// decodeLabels reads encodeLabels' format. Within one scan repeats are
// appended (see PutReference), so it deduplicates.
func decodeLabels(s string) ([]graph.LabelRef, error) {
	if s == "" {
		return nil, nil
	}
	var out []graph.LabelRef
	for _, line := range strings.Split(s, "\n") {
		use, name, ok := strings.Cut(line, ":")
		if !ok || use == "" || name == "" {
			return nil, fmt.Errorf("bad label line %q", line)
		}
		if strings.HasPrefix(name, `"`) {
			n, err := strconv.Unquote(name)
			if err != nil {
				return nil, fmt.Errorf("bad label line %q: %w", line, err)
			}
			name = n
		}
		out = append(out, graph.LabelRef{Name: name, Use: graph.LabelUse(use)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Use < out[j].Use
	})
	return slices.Compact(out), nil
}

// encodeReasons stores reasons one per line; a newline inside a reason
// becomes a space.
func encodeReasons(rs []string) string {
	lines := make([]string, 0, len(rs))
	for _, r := range rs {
		if r = strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(r)); r != "" {
			lines = append(lines, r)
		}
	}
	sort.Strings(lines)
	return strings.Join(slices.Compact(lines), "\n")
}

func decodeReasons(s string) []string {
	if s == "" {
		return nil
	}
	out := strings.Split(s, "\n")
	sort.Strings(out)
	return slices.Compact(out)
}
