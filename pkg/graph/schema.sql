-- Usage graph store (SQLite for the CLI, Postgres for continuous mode).
-- Keep this DDL portable between the two.
--
-- Times are INTEGER Unix milliseconds (UTC), NULL when unknown. Integers
-- compare and aggregate exactly in both databases; text timestamps don't.
--
-- Each scan rewrites the current graph inside one transaction. Rows keep
-- the id of the last scan that saw them (last_scan), so the graph "as of
-- scan N" is the rows with last_scan = N, and a dashboard deleted since an
-- earlier scan stops protecting signals. first_seen survives across scans.
-- Foreign keys are checked at commit, so a scan can write rows in any order.

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- One row per completed scan. A scan's rows are written in the same
-- transaction as its finished_at, so an interrupted scan leaves nothing.
CREATE TABLE IF NOT EXISTS scans (
    id           INTEGER PRIMARY KEY,
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER NOT NULL,
    window_days  INTEGER NOT NULL,
    log_coverage REAL NOT NULL DEFAULT 0  -- 0..1, share of sources with usable query logs
);

-- Coverage of each usage-evidence source for a scan, configured or not.
CREATE TABLE IF NOT EXISTS scan_sources (
    scan_id      INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
    source       TEXT NOT NULL,
    configured   INTEGER NOT NULL CHECK (configured IN (0, 1)),
    coverage     REAL NOT NULL CHECK (coverage >= 0 AND coverage <= 1),
    window_start INTEGER NOT NULL,
    PRIMARY KEY (scan_id, source)
);

CREATE TABLE IF NOT EXISTS signals (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('metric', 'label', 'stream', 'span_attribute')),
    name       TEXT NOT NULL,
    parent     TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL,
    first_seen INTEGER NOT NULL,
    last_scan  INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX IF NOT EXISTS signals_name ON signals (kind, name);
CREATE INDEX IF NOT EXISTS signals_scan ON signals (last_scan);

CREATE TABLE IF NOT EXISTS consumers (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    name        TEXT NOT NULL,
    expr        TEXT NOT NULL DEFAULT '',  -- the query, for evidence in reports
    team        TEXT NOT NULL DEFAULT '',
    last_viewed INTEGER,
    last_scan   INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX IF NOT EXISTS consumers_scan ON consumers (last_scan);

CREATE TABLE IF NOT EXISTS refs (
    signal_id   TEXT NOT NULL REFERENCES signals (id) DEFERRABLE INITIALLY DEFERRED,
    consumer_id TEXT NOT NULL REFERENCES consumers (id) DEFERRABLE INITIALLY DEFERRED,
    via         TEXT NOT NULL DEFAULT '',
    last_seen   INTEGER,
    actor       TEXT NOT NULL DEFAULT '',
    -- 1 when the reference was inferred without a full parse of the query:
    -- it covers every label of the signal (schema version 2).
    assumed     INTEGER NOT NULL DEFAULT 0 CHECK (assumed IN (0, 1)),
    last_scan   INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
    PRIMARY KEY (signal_id, consumer_id, via)
);
CREATE INDEX IF NOT EXISTS refs_consumer ON refs (consumer_id);
CREATE INDEX IF NOT EXISTS refs_scan ON refs (last_scan);

-- One row per signal per scan: the history savings are measured from.
CREATE TABLE IF NOT EXISTS costs (
    signal_id     TEXT NOT NULL REFERENCES signals (id) DEFERRABLE INITIALLY DEFERRED,
    scan_id       INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
    observed_at   INTEGER NOT NULL,
    active_series INTEGER NOT NULL DEFAULT 0,
    distinct_values INTEGER NOT NULL DEFAULT 0, -- labels only
    bytes_per_day INTEGER NOT NULL DEFAULT 0,
    spans_per_day INTEGER NOT NULL DEFAULT 0,
    monthly_cost  REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (signal_id, scan_id)
);

-- Dashboard loads in a scan's window, per login and kind. browser = 1 is a
-- view from the dashboard page; 0 is an API client other than tvt.
CREATE TABLE IF NOT EXISTS dashboard_loads (
    scan_id       INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
    org_id        INTEGER NOT NULL,
    dashboard_uid TEXT NOT NULL,
    login         TEXT NOT NULL,
    browser       INTEGER NOT NULL CHECK (browser IN (0, 1)),
    loads         INTEGER NOT NULL,
    last_at       INTEGER NOT NULL,
    PRIMARY KEY (scan_id, org_id, dashboard_uid, login, browser)
);
