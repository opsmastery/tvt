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

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// SchemaVersion is the version of graph.Schema this package writes.
const SchemaVersion = 21

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
	if from < 8 {
		for _, q := range []string{
			`ALTER TABLE scans ADD COLUMN pricecard TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE scans ADD COLUMN currency TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE scans ADD COLUMN price_preset INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE costs ADD COLUMN interval_ms INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE costs ADD COLUMN interval_assumed TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE costs ADD COLUMN buckets INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE costs ADD COLUMN exemplars INTEGER NOT NULL DEFAULT 0`,
			`UPDATE meta SET value = '8' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 9 {
		for _, q := range []string{
			`ALTER TABLE scans ADD COLUMN price_kind TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE scans ADD COLUMN bytes_per_sample REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE scans ADD COLUMN bytes_per_sample_from TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE scans ADD COLUMN retention_ms INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE scans ADD COLUMN retention_from TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE scans ADD COLUMN head_bytes_per_series REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE costs ADD COLUMN disk_bytes INTEGER NOT NULL DEFAULT 0`,
			// A card before version 9 was always a vendor card.
			`UPDATE scans SET price_kind = 'vendor' WHERE pricecard <> ''`,
			`UPDATE meta SET value = '9' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 10 {
		// stream_volumes is a new table: migrate's CREATE TABLE IF NOT
		// EXISTS adds it.
		for _, q := range []string{
			`ALTER TABLE scans ADD COLUMN volume_truncated INTEGER NOT NULL DEFAULT 0`,
			`UPDATE meta SET value = '10' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 11 {
		for _, q := range []string{
			`ALTER TABLE scans ADD COLUMN tsdb_measured INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE scans ADD COLUMN tsdb_bytes_per_sample REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE scans ADD COLUMN tsdb_head_series INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE scans ADD COLUMN tsdb_head_bytes INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE scans ADD COLUMN tsdb_blocks_bytes INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE scans ADD COLUMN tsdb_retention_ms INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE scans ADD COLUMN tsdb_retention_bytes INTEGER NOT NULL DEFAULT 0`,
			`UPDATE meta SET value = '11' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 12 {
		for _, q := range []string{
			`ALTER TABLE consumers ADD COLUMN client TEXT NOT NULL DEFAULT ''`,
			`UPDATE meta SET value = '12' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 13 {
		for _, q := range []string{
			`ALTER TABLE refs ADD COLUMN sweep INTEGER NOT NULL DEFAULT 0 CHECK (sweep IN (0, 1))`,
			`UPDATE meta SET value = '13' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 14 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS scores (
				scan_id      INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
				signal_id    TEXT NOT NULL REFERENCES signals (id) DEFERRABLE INITIALLY DEFERRED,
				monthly_cost REAL NOT NULL,
				usage_weight REAL NOT NULL CHECK (usage_weight >= 0),
				strongest    TEXT NOT NULL,
				confidence   REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
				priority     REAL NOT NULL,
				held         TEXT NOT NULL DEFAULT '' CHECK (held IN ('', 'window', 'coverage', 'new')),
				PRIMARY KEY (scan_id, signal_id)
			)`,
			`UPDATE meta SET value = '14' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 15 {
		for _, q := range []string{
			`ALTER TABLE refs ADD COLUMN first_scan INTEGER REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED`,
			`UPDATE refs SET first_scan = last_scan`,
			`UPDATE meta SET value = '15' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 16 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS line_findings (
			    scan_id       INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
			    stream        TEXT NOT NULL,
			    pattern       TEXT NOT NULL,
			    kind          TEXT NOT NULL CHECK (kind IN ('repeated_line', 'line_handoff')),
			    lines         INTEGER NOT NULL CHECK (lines >= 0),
			    window_ms     INTEGER NOT NULL CHECK (window_ms >= 0),
			    share         REAL NOT NULL CHECK (share >= 0 AND share <= 1),
			    bytes_per_day INTEGER NOT NULL CHECK (bytes_per_day >= 0),
			    monthly_cost  REAL NOT NULL,
			    PRIMARY KEY (scan_id, stream, pattern)
			)`,
			`UPDATE meta SET value = '16' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 17 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS rule_costs (
			    scan_id          INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
			    consumer_id      TEXT NOT NULL REFERENCES consumers (id) DEFERRABLE INITIALLY DEFERRED,
			    evals_per_day    REAL NOT NULL CHECK (evals_per_day >= 0),
			    samples_per_eval REAL NOT NULL CHECK (samples_per_eval >= 0),
			    bytes_per_eval   REAL NOT NULL CHECK (bytes_per_eval >= 0),
			    evaluations      INTEGER NOT NULL CHECK (evaluations >= 0),
			    unknown          TEXT NOT NULL DEFAULT '',
			    PRIMARY KEY (scan_id, consumer_id)
			)`,
			`CREATE TABLE IF NOT EXISTS rule_findings (
			    scan_id     INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
			    consumer_id TEXT NOT NULL REFERENCES consumers (id) DEFERRABLE INITIALLY DEFERRED,
			    kind        TEXT NOT NULL CHECK (kind IN ('grafana_managed_alert', 'logql_parses_raw_logs', 'no_line_filter_before_parser',
			                    'long_range_window', 'interval_below_scrape', 'duplicate_rule', 'unread_recording_rule', 'no_series',
			                    'group_slower_than_interval')),
			    detail      TEXT NOT NULL DEFAULT '',
			    fix         TEXT NOT NULL DEFAULT '',
			    PRIMARY KEY (scan_id, consumer_id, kind)
			)`,
			`UPDATE meta SET value = '17' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 18 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS query_costs (
			    scan_id     INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
			    consumer_id TEXT NOT NULL REFERENCES consumers (id) DEFERRABLE INITIALLY DEFERRED,
			    queries     REAL NOT NULL CHECK (queries >= 0),
			    samples     REAL NOT NULL CHECK (samples >= 0),
			    bytes       REAL NOT NULL CHECK (bytes >= 0),
			    PRIMARY KEY (scan_id, consumer_id)
			)`,
			`UPDATE meta SET value = '18' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 19 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS slo_findings (
			    scan_id INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
			    kind    TEXT NOT NULL CHECK (kind IN ('service_without_slo', 'paging_not_burn_rate', 'slo_without_alert')),
			    subject TEXT NOT NULL,
			    name    TEXT NOT NULL,
			    detail  TEXT NOT NULL,
			    fix     TEXT NOT NULL,
			    PRIMARY KEY (scan_id, kind, subject)
			)`,
			`UPDATE meta SET value = '19' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 20 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS pipeline_targets (
			    scan_id  INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
			    subject  TEXT NOT NULL,
			    pipeline TEXT NOT NULL,
			    note     TEXT NOT NULL,
			    PRIMARY KEY (scan_id, subject)
			)`,
			`UPDATE meta SET value = '20' WHERE key = 'schema_version'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if from < 21 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS overflow_findings (
			    scan_id INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
			    signal  TEXT NOT NULL,
			    series  INTEGER NOT NULL,
			    capped  TEXT NOT NULL,
			    readers TEXT NOT NULL,
			    PRIMARY KEY (scan_id, signal)
			)`,
			`UPDATE meta SET value = '21' WHERE key = 'schema_version'`,
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
	// PriceCard, Currency, PricePreset and PriceKind describe the card the
	// scan's costs use; PriceCard is empty when none was configured.
	PriceCard   string
	Currency    string
	PricePreset bool
	PriceKind   string
	// SelfHosted is the self-hosted model's inputs; nil for a vendor card.
	SelfHosted *SelfHosted
	// VolumeTruncated: Loki's volume API hit its series limit, so stream
	// bytes are low.
	VolumeTruncated bool
	// TSDB is what the Prometheus server reported about its storage; nil
	// when it wasn't read (Mimir, or an older scan). cost.Reprice takes it.
	TSDB *collect.Storage
}

// SelfHosted is what a self_hosted price card's costs were computed from,
// and where each input came from.
type SelfHosted struct {
	BytesPerSample     float64
	BytesPerSampleFrom string
	Retention          time.Duration
	RetentionFrom      string
	HeadBytesPerSeries float64
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
		`SELECT id, started_at, finished_at, window_days, log_coverage, pricecard, currency, price_preset, price_kind,
		        bytes_per_sample, bytes_per_sample_from, retention_ms, retention_from, head_bytes_per_series, volume_truncated,
		        tsdb_measured, tsdb_bytes_per_sample, tsdb_head_series, tsdb_head_bytes, tsdb_blocks_bytes, tsdb_retention_ms, tsdb_retention_bytes
		 FROM scans `+tail)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScanInfo
	for rows.Next() {
		var si ScanInfo
		var started, finished, retention, tsdbRetention int64
		var sh SelfHosted
		var tsdb collect.Storage
		var measured bool
		if err := rows.Scan(&si.ID, &started, &finished, &si.WindowDays, &si.LogCoverage, &si.PriceCard, &si.Currency, &si.PricePreset, &si.PriceKind,
			&sh.BytesPerSample, &sh.BytesPerSampleFrom, &retention, &sh.RetentionFrom, &sh.HeadBytesPerSeries, &si.VolumeTruncated,
			&measured, &tsdb.BytesPerSample, &tsdb.HeadSeries, &tsdb.HeadBytes, &tsdb.BlocksBytes, &tsdbRetention, &tsdb.RetentionBytes); err != nil {
			return nil, err
		}
		si.StartedAt, si.FinishedAt = fromMillis(started), fromMillis(finished)
		if measured {
			tsdb.RetentionTime = time.Duration(tsdbRetention) * time.Millisecond
			si.TSDB = &tsdb
		}
		if si.PriceKind == "self_hosted" {
			sh.Retention = time.Duration(retention) * time.Millisecond
			si.SelfHosted = &sh
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// FirstSeen returns every stored signal's first sighting, whichever scan
// last saw it.
func (s *Store) FirstSeen(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, first_seen FROM signals`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var first int64
		if err := rows.Scan(&id, &first); err != nil {
			return nil, err
		}
		out[id] = fromMillis(first)
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
		`SELECT id, kind, name, expr, team, last_viewed, evaluated_by, interval_ms, datasources, unparsed, client FROM consumers WHERE last_scan = ? ORDER BY id`, scanID)
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
		if err := rows.Scan(&c.ID, &kind, &c.Name, &c.Expr, &c.Team, &viewed, &c.EvaluatedBy, &interval, &dss, &c.Unparsed, &c.Client); err != nil {
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
		`SELECT r.signal_id, r.consumer_id, r.via, r.last_seen, r.actor, r.assumed, r.evidence, r.labels, r.all_labels, r.reason, r.result_labels, r.sweep, f.started_at
		 FROM refs r LEFT JOIN scans f ON f.id = coalesce(r.first_scan, r.last_scan) WHERE r.last_scan = ?
		 ORDER BY r.signal_id, r.consumer_id, r.via`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.Reference
	for rows.Next() {
		var r graph.Reference
		var seen, first sql.NullInt64
		var evidence, labels, reasons string
		if err := rows.Scan(&r.SignalID, &r.ConsumerID, &r.Via, &seen, &r.Actor, &r.Assumed,
			&evidence, &labels, &r.AllLabels, &reasons, &r.ResultLabels, &r.Sweep, &first); err != nil {
			return nil, err
		}
		r.LastSeen = fromNullMillis(seen)
		r.FirstSeen = fromNullMillis(first)
		r.Evidence = graph.EvidenceClass(evidence)
		if r.Labels, err = decodeLabels(labels); err != nil {
			return nil, fmt.Errorf("reference %s -> %s: %w", r.SignalID, r.ConsumerID, err)
		}
		r.Reasons = decodeReasons(reasons)
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastViewed returns every consumer's latest recorded view across all
// scans (consumers.last_viewed only moves forward), so a dashboard view
// still counts after Grafana's log line for it has rotated away.
func (s *Store) LastViewed(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, last_viewed FROM consumers WHERE last_viewed IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = fromMillis(at)
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

// LastReads returns, for every signal a query log ever showed being read
// (a sweep over every metric name is not a read),
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
		       WHERE last_seen IS NOT NULL AND sweep = 0 GROUP BY signal_id) l
		   ON l.signal_id = r.signal_id AND r.last_seen = l.latest AND r.sweep = 0
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
		`SELECT signal_id, scan_id, observed_at, active_series, distinct_values, bytes_per_day, spans_per_day, monthly_cost,
		        interval_ms, interval_assumed, buckets, exemplars, disk_bytes
		 FROM costs WHERE scan_id = ? ORDER BY signal_id`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.Cost
	for rows.Next() {
		var c graph.Cost
		var observed, interval int64
		if err := rows.Scan(&c.SignalID, &c.ScanID, &observed, &c.ActiveSeries, &c.DistinctValues, &c.BytesPerDay, &c.SpansPerDay, &c.MonthlyCost,
			&interval, &c.IntervalAssumed, &c.Buckets, &c.Exemplars, &c.DiskBytes); err != nil {
			return nil, err
		}
		c.ObservedAt = fromMillis(observed)
		c.Interval = time.Duration(interval) * time.Millisecond
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

// StreamVolumes returns every Loki stream's ingest in scan scanID, ordered
// by selector.
func (s *Store) StreamVolumes(ctx context.Context, scanID int64) ([]graph.StreamVolume, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT selector, bytes_per_day FROM stream_volumes WHERE scan_id = ? ORDER BY selector`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.StreamVolume
	for rows.Next() {
		var v graph.StreamVolume
		if err := rows.Scan(&v.Selector, &v.BytesPerDay); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// LineFindings returns the line findings of scan scanID, most bytes first.
func (s *Store) LineFindings(ctx context.Context, scanID int64) ([]graph.LineFinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT stream, pattern, kind, lines, window_ms, share, bytes_per_day, monthly_cost
		 FROM line_findings WHERE scan_id = ? ORDER BY bytes_per_day DESC, stream, pattern`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.LineFinding
	for rows.Next() {
		var f graph.LineFinding
		var ms int64
		if err := rows.Scan(&f.Stream, &f.Pattern, &f.Kind, &f.Lines, &ms, &f.Share, &f.BytesPerDay, &f.MonthlyCost); err != nil {
			return nil, err
		}
		f.Window = time.Duration(ms) * time.Millisecond
		out = append(out, f)
	}
	return out, rows.Err()
}

// QueryCosts returns the query costs of scan scanID, by consumer.
func (s *Store) QueryCosts(ctx context.Context, scanID int64) ([]graph.QueryCost, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT consumer_id, queries, samples, bytes FROM query_costs WHERE scan_id = ? ORDER BY consumer_id`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.QueryCost
	for rows.Next() {
		var c graph.QueryCost
		if err := rows.Scan(&c.ConsumerID, &c.Queries, &c.Samples, &c.Bytes); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PipelineTargets returns the pipeline targets of scan scanID, by subject.
func (s *Store) PipelineTargets(ctx context.Context, scanID int64) ([]graph.PipelineTarget, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT subject, pipeline, note FROM pipeline_targets WHERE scan_id = ? ORDER BY subject`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.PipelineTarget
	for rows.Next() {
		var t graph.PipelineTarget
		if err := rows.Scan(&t.Subject, &t.Pipeline, &t.Note); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// OverflowFindings returns the overflow findings of scan scanID, by
// signal.
func (s *Store) OverflowFindings(ctx context.Context, scanID int64) ([]graph.OverflowFinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT signal, series, capped, readers FROM overflow_findings WHERE scan_id = ? ORDER BY signal`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.OverflowFinding
	for rows.Next() {
		var f graph.OverflowFinding
		var capped, readers string
		if err := rows.Scan(&f.Signal, &f.Series, &capped, &readers); err != nil {
			return nil, err
		}
		f.Capped, f.Readers = lines(capped), lines(readers)
		out = append(out, f)
	}
	return out, rows.Err()
}

// SLOFindings returns the SLO coverage findings of scan scanID, by kind
// and subject.
func (s *Store) SLOFindings(ctx context.Context, scanID int64) ([]graph.SLOFinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, subject, name, detail, fix FROM slo_findings WHERE scan_id = ? ORDER BY kind, subject`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.SLOFinding
	for rows.Next() {
		var f graph.SLOFinding
		if err := rows.Scan(&f.Kind, &f.Subject, &f.Name, &f.Detail, &f.Fix); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RuleCosts returns the rule costs of scan scanID, by consumer.
func (s *Store) RuleCosts(ctx context.Context, scanID int64) ([]graph.RuleCost, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT consumer_id, evals_per_day, samples_per_eval, bytes_per_eval, evaluations, unknown
		 FROM rule_costs WHERE scan_id = ? ORDER BY consumer_id`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.RuleCost
	for rows.Next() {
		var c graph.RuleCost
		if err := rows.Scan(&c.ConsumerID, &c.EvalsPerDay, &c.SamplesPerEval, &c.BytesPerEval, &c.Evaluations, &c.Unknown); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RuleFindings returns the rule findings of scan scanID, by consumer and
// kind.
func (s *Store) RuleFindings(ctx context.Context, scanID int64) ([]graph.RuleFinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT consumer_id, kind, detail, fix FROM rule_findings WHERE scan_id = ? ORDER BY consumer_id, kind`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.RuleFinding
	for rows.Next() {
		var f graph.RuleFinding
		if err := rows.Scan(&f.ConsumerID, &f.Kind, &f.Detail, &f.Fix); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Scores returns the candidate ranks of scan scanID, highest priority
// first.
func (s *Store) Scores(ctx context.Context, scanID int64) ([]graph.Score, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT signal_id, monthly_cost, usage_weight, strongest, confidence, priority, held
		 FROM scores WHERE scan_id = ? ORDER BY priority DESC, signal_id`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.Score
	for rows.Next() {
		var r graph.Score
		if err := rows.Scan(&r.SignalID, &r.MonthlyCost, &r.UsageWeight, &r.Strongest, &r.Confidence, &r.Priority, &r.Held); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Scan is an open scan: a transaction that writes one complete graph.
// Call Finish to commit it or Abort to discard it; deferring Abort right
// after BeginScan is safe, since Abort after Finish does nothing.
type Scan struct {
	ID int64

	tx          *sql.Tx
	started     time.Time
	done        bool
	signal      *sql.Stmt
	cons        *sql.Stmt
	ref         *sql.Stmt
	cost        *sql.Stmt
	source      *sql.Stmt
	load        *sql.Stmt
	volume      *sql.Stmt
	score       *sql.Stmt
	line        *sql.Stmt
	ruleCost    *sql.Stmt
	ruleFinding *sql.Stmt
	queryCost   *sql.Stmt
	sloFinding  *sql.Stmt
	pipeline    *sql.Stmt
	overflow    *sql.Stmt
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
		{&sc.cons, `INSERT INTO consumers (id, kind, name, expr, team, last_viewed, last_scan, evaluated_by, interval_ms, datasources, unparsed, client)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				kind = excluded.kind, name = excluded.name, expr = excluded.expr, team = excluded.team,
				evaluated_by = excluded.evaluated_by, interval_ms = excluded.interval_ms, datasources = excluded.datasources,
				unparsed = excluded.unparsed, client = excluded.client,
				last_viewed = CASE
					WHEN consumers.last_viewed IS NULL THEN excluded.last_viewed
					WHEN excluded.last_viewed IS NULL THEN consumers.last_viewed
					ELSE max(consumers.last_viewed, excluded.last_viewed) END,
				last_scan = excluded.last_scan`},
		{&sc.ref, `INSERT INTO refs (signal_id, consumer_id, via, last_seen, actor, assumed, evidence, labels, all_labels, reason, result_labels, last_scan, sweep, first_scan)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
				sweep = CASE
					WHEN refs.last_scan = excluded.last_scan THEN min(refs.sweep, excluded.sweep)
					ELSE excluded.sweep END,
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
		{&sc.cost, `INSERT INTO costs (signal_id, scan_id, observed_at, active_series, distinct_values, bytes_per_day, spans_per_day, monthly_cost,
				interval_ms, interval_assumed, buckets, exemplars, disk_bytes)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (signal_id, scan_id) DO UPDATE SET
				observed_at = excluded.observed_at, active_series = excluded.active_series,
				distinct_values = excluded.distinct_values,
				bytes_per_day = excluded.bytes_per_day, spans_per_day = excluded.spans_per_day,
				monthly_cost = excluded.monthly_cost, interval_ms = excluded.interval_ms,
				interval_assumed = excluded.interval_assumed, buckets = excluded.buckets,
				exemplars = excluded.exemplars, disk_bytes = excluded.disk_bytes`},
		{&sc.source, `INSERT INTO scan_sources (scan_id, source, configured, coverage, window_start, gap)
			VALUES (?, ?, ?, ?, ?, ?)`},
		{&sc.volume, `INSERT INTO stream_volumes (scan_id, selector, bytes_per_day) VALUES (?, ?, ?)`},
		{&sc.line, `INSERT INTO line_findings (scan_id, stream, pattern, kind, lines, window_ms, share, bytes_per_day, monthly_cost)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`},
		{&sc.ruleCost, `INSERT INTO rule_costs (scan_id, consumer_id, evals_per_day, samples_per_eval, bytes_per_eval, evaluations, unknown)
			VALUES (?, ?, ?, ?, ?, ?, ?)`},
		{&sc.ruleFinding, `INSERT INTO rule_findings (scan_id, consumer_id, kind, detail, fix) VALUES (?, ?, ?, ?, ?)`},
		{&sc.queryCost, `INSERT INTO query_costs (scan_id, consumer_id, queries, samples, bytes) VALUES (?, ?, ?, ?, ?)`},
		{&sc.sloFinding, `INSERT INTO slo_findings (scan_id, kind, subject, name, detail, fix) VALUES (?, ?, ?, ?, ?, ?)`},
		{&sc.pipeline, `INSERT INTO pipeline_targets (scan_id, subject, pipeline, note) VALUES (?, ?, ?, ?)`},
		{&sc.overflow, `INSERT INTO overflow_findings (scan_id, signal, series, capped, readers) VALUES (?, ?, ?, ?, ?)`},
		{&sc.score, `INSERT INTO scores (scan_id, signal_id, monthly_cost, usage_weight, strongest, confidence, priority, held)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`},
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
		c.EvaluatedBy, interval, strings.Join(c.Datasources, "\n"), c.Unparsed, c.Client)
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
		string(r.Evidence), labels, boolInt(r.AllLabels), encodeReasons(r.Reasons), boolInt(r.ResultLabels), sc.ID, boolInt(r.Sweep), sc.ID)
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
		c.ActiveSeries, c.DistinctValues, c.BytesPerDay, c.SpansPerDay, c.MonthlyCost,
		c.Interval.Milliseconds(), c.IntervalAssumed, c.Buckets, c.Exemplars, c.DiskBytes)
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

// SetPriceCard records the price card this scan's costs use: its name, its
// currency, whether it is a vendor preset (approximate), and its kind.
func (sc *Scan) SetPriceCard(ctx context.Context, name, currency string, preset bool, kind string) error {
	if err := sc.check(); err != nil {
		return err
	}
	_, err := sc.tx.ExecContext(ctx, `UPDATE scans SET pricecard = ?, currency = ?, price_preset = ?, price_kind = ? WHERE id = ?`,
		name, currency, preset, kind, sc.ID)
	return wrap("price card", name, err)
}

// PutStreamVolume records one Loki stream's ingest; each selector is put
// once per scan.
func (sc *Scan) PutStreamVolume(ctx context.Context, v graph.StreamVolume) error {
	if err := sc.check(); err != nil {
		return err
	}
	if v.Selector == "" || v.BytesPerDay < 0 {
		return fmt.Errorf("store: stream volume %q: Selector is required and bytes can't be negative", v.Selector)
	}
	_, err := sc.volume.ExecContext(ctx, sc.ID, v.Selector, v.BytesPerDay)
	return wrap("stream volume", v.Selector, err)
}

// PutLineFinding records a line pattern worth cutting, once per stream and
// pattern.
func (sc *Scan) PutLineFinding(ctx context.Context, f graph.LineFinding) error {
	if err := sc.check(); err != nil {
		return err
	}
	if f.Stream == "" || f.Pattern == "" {
		return fmt.Errorf("store: line finding %q in %q: Stream and Pattern are required", f.Pattern, f.Stream)
	}
	_, err := sc.line.ExecContext(ctx, sc.ID, f.Stream, f.Pattern, f.Kind, f.Lines, f.Window.Milliseconds(), f.Share, f.BytesPerDay, f.MonthlyCost)
	return wrap("line finding", f.Stream+" "+f.Pattern, err)
}

// PutRuleCost records what a rule's evaluations cost, once per rule.
func (sc *Scan) PutRuleCost(ctx context.Context, c graph.RuleCost) error {
	if err := sc.check(); err != nil {
		return err
	}
	if c.ConsumerID == "" {
		return errors.New("store: rule cost: ConsumerID is required")
	}
	_, err := sc.ruleCost.ExecContext(ctx, sc.ID, c.ConsumerID, c.EvalsPerDay, c.SamplesPerEval, c.BytesPerEval, c.Evaluations, c.Unknown)
	return wrap("rule cost", c.ConsumerID, err)
}

// PutRuleFinding records one finding about a rule, once per rule and kind.
func (sc *Scan) PutRuleFinding(ctx context.Context, f graph.RuleFinding) error {
	if err := sc.check(); err != nil {
		return err
	}
	if f.ConsumerID == "" || f.Kind == "" {
		return errors.New("store: rule finding: ConsumerID and Kind are required")
	}
	_, err := sc.ruleFinding.ExecContext(ctx, sc.ID, f.ConsumerID, f.Kind, f.Detail, f.Fix)
	return wrap("rule finding", f.ConsumerID+" "+f.Kind, err)
}

// PutQueryCost records a query-log consumer's measured query cost, once
// per consumer.
func (sc *Scan) PutQueryCost(ctx context.Context, c graph.QueryCost) error {
	if err := sc.check(); err != nil {
		return err
	}
	if c.ConsumerID == "" {
		return errors.New("store: query cost: ConsumerID is required")
	}
	_, err := sc.queryCost.ExecContext(ctx, sc.ID, c.ConsumerID, c.Queries, c.Samples, c.Bytes)
	return wrap("query cost", c.ConsumerID, err)
}

// PutPipelineTarget records one subject's pipeline target, once per
// subject. Unmapped (empty Pipeline) needs a Note saying why.
func (sc *Scan) PutPipelineTarget(ctx context.Context, t graph.PipelineTarget) error {
	if err := sc.check(); err != nil {
		return err
	}
	if t.Subject == "" || (t.Pipeline == "" && t.Note == "") {
		return errors.New("store: pipeline target: Subject is required, and Note when unmapped")
	}
	_, err := sc.pipeline.ExecContext(ctx, sc.ID, t.Subject, t.Pipeline, t.Note)
	return wrap("pipeline target", t.Subject, err)
}

// PutOverflowFinding records one metric's overflow finding, once per
// signal.
func (sc *Scan) PutOverflowFinding(ctx context.Context, f graph.OverflowFinding) error {
	if err := sc.check(); err != nil {
		return err
	}
	if f.Signal == "" || f.Series <= 0 {
		return errors.New("store: overflow finding: Signal and a positive Series are required")
	}
	_, err := sc.overflow.ExecContext(ctx, sc.ID, f.Signal, f.Series, strings.Join(f.Capped, "\n"), strings.Join(f.Readers, "\n"))
	return wrap("overflow finding", f.Signal, err)
}

// PutSLOFinding records one SLO coverage finding, once per kind and
// subject.
func (sc *Scan) PutSLOFinding(ctx context.Context, f graph.SLOFinding) error {
	if err := sc.check(); err != nil {
		return err
	}
	if f.Kind == "" || f.Subject == "" {
		return errors.New("store: SLO finding: Kind and Subject are required")
	}
	_, err := sc.sloFinding.ExecContext(ctx, sc.ID, f.Kind, f.Subject, f.Name, f.Detail, f.Fix)
	return wrap("SLO finding", f.Kind+" "+f.Subject, err)
}

// PutScore records a candidate signal's rank in this scan, once per signal.
func (sc *Scan) PutScore(ctx context.Context, r graph.Score) error {
	if err := sc.check(); err != nil {
		return err
	}
	if r.SignalID == "" || r.Strongest == "" {
		return errors.New("store: score: SignalID and Strongest are required")
	}
	_, err := sc.score.ExecContext(ctx, sc.ID, r.SignalID, r.MonthlyCost, r.UsageWeight, r.Strongest, r.Confidence, r.Priority, r.Held)
	return wrap("score", r.SignalID, err)
}

// SetVolumeTruncated records that Loki's volume API hit its series limit.
func (sc *Scan) SetVolumeTruncated(ctx context.Context) error {
	if err := sc.check(); err != nil {
		return err
	}
	_, err := sc.tx.ExecContext(ctx, `UPDATE scans SET volume_truncated = 1 WHERE id = ?`, sc.ID)
	return wrap("volume truncated", "", err)
}

// SetTSDB records what the Prometheus server reported about its storage.
func (sc *Scan) SetTSDB(ctx context.Context, st collect.Storage) error {
	if err := sc.check(); err != nil {
		return err
	}
	_, err := sc.tx.ExecContext(ctx, `UPDATE scans SET tsdb_measured = 1, tsdb_bytes_per_sample = ?, tsdb_head_series = ?,
		tsdb_head_bytes = ?, tsdb_blocks_bytes = ?, tsdb_retention_ms = ?, tsdb_retention_bytes = ? WHERE id = ?`,
		st.BytesPerSample, st.HeadSeries, st.HeadBytes, st.BlocksBytes, st.RetentionTime.Milliseconds(), st.RetentionBytes, sc.ID)
	return wrap("tsdb storage", "", err)
}

// SetSelfHosted records a self_hosted card's model inputs.
func (sc *Scan) SetSelfHosted(ctx context.Context, sh SelfHosted) error {
	if err := sc.check(); err != nil {
		return err
	}
	_, err := sc.tx.ExecContext(ctx, `UPDATE scans SET bytes_per_sample = ?, bytes_per_sample_from = ?, retention_ms = ?,
		retention_from = ?, head_bytes_per_series = ? WHERE id = ?`,
		sh.BytesPerSample, sh.BytesPerSampleFrom, sh.Retention.Milliseconds(), sh.RetentionFrom, sh.HeadBytesPerSeries, sc.ID)
	return wrap("self-hosted model", "", err)
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

// lines splits a newline-joined column; "" is none.
func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
