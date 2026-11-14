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
    log_coverage REAL NOT NULL DEFAULT 0, -- 0..1, share of sources with usable query logs
    -- The price card monthly_cost uses (name, its currency, 1 for a vendor
    -- preset: approximate, its kind). Empty when no card was configured.
    pricecard    TEXT NOT NULL DEFAULT '',
    currency     TEXT NOT NULL DEFAULT '',
    price_preset INTEGER NOT NULL DEFAULT 0,
    price_kind   TEXT NOT NULL DEFAULT '',
    -- A self_hosted card's model inputs and where each came from (the card,
    -- the server's /metrics, or an assumption).
    bytes_per_sample      REAL NOT NULL DEFAULT 0,
    bytes_per_sample_from TEXT NOT NULL DEFAULT '',
    retention_ms          INTEGER NOT NULL DEFAULT 0,
    retention_from        TEXT NOT NULL DEFAULT '',
    head_bytes_per_series REAL NOT NULL DEFAULT 0
);

-- Coverage of each usage-evidence source for a scan, configured or not.
CREATE TABLE IF NOT EXISTS scan_sources (
    scan_id      INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
    source       TEXT NOT NULL,
    configured   INTEGER NOT NULL CHECK (configured IN (0, 1)),
    coverage     REAL NOT NULL CHECK (coverage >= 0 AND coverage <= 1),
    window_start INTEGER NOT NULL,
    -- Schema version 4: why the source can't be trusted (a query log
    -- whose probe never appeared), '' when it can.
    gap          TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (scan_id, source)
);

CREATE TABLE IF NOT EXISTS signals (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('metric', 'label', 'stream', 'span_attribute')),
    name       TEXT NOT NULL,
    parent     TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL,
    first_seen INTEGER NOT NULL,
    last_scan  INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
    -- Schema version 6. class: hard_keep, keep_high, dormant or candidate
    -- ('' before version 6). dormant: why an unread signal looks
    -- incident-critical. owner and tier: from tvt.yaml ownership.
    class      TEXT NOT NULL DEFAULT '' CHECK (class IN ('', 'hard_keep', 'keep_high', 'dormant', 'candidate')),
    dormant    TEXT NOT NULL DEFAULT '',
    owner      TEXT NOT NULL DEFAULT '',
    tier       INTEGER
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
    last_scan   INTEGER NOT NULL REFERENCES scans (id) DEFERRABLE INITIALLY DEFERRED,
    -- Schema version 5, rules only. evaluated_by: 'grafana' when Grafana
    -- evaluates the rule and queries the datasource every interval, '' when
    -- the datasource's ruler does. interval_ms: evaluation interval (NULL
    -- if unknown). datasources: the UIDs it queries, one per line.
    evaluated_by TEXT NOT NULL DEFAULT '',
    interval_ms  INTEGER,
    datasources  TEXT NOT NULL DEFAULT '',
    -- Schema version 7: why tvt couldn't read what the consumer queries
    -- (a datasource type it doesn't parse), '' when it could. Such a
    -- consumer reads unknown signals: never "reads nothing".
    unparsed     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS consumers_scan ON consumers (last_scan);

CREATE TABLE IF NOT EXISTS refs (
    signal_id   TEXT NOT NULL REFERENCES signals (id) DEFERRABLE INITIALLY DEFERRED,
    consumer_id TEXT NOT NULL REFERENCES consumers (id) DEFERRABLE INITIALLY DEFERRED,
    via         TEXT NOT NULL DEFAULT '',
    last_seen   INTEGER,
    actor       TEXT NOT NULL DEFAULT '',
    -- 1 when the reference is a guess (a variable, an unresolved pattern,
    -- a query that didn't parse); reason says why (schema version 2).
    assumed     INTEGER NOT NULL DEFAULT 0 CHECK (assumed IN (0, 1)),
    -- Schema version 3. evidence: declared, incident or observed ('' for
    -- rows from before version 3). labels: one "use:name" per line, the
    -- labels the consumer reads. all_labels: it may depend on every label.
    -- reason: why it is assumed, one reason per line. result_labels: the
    -- signal's labels survive into the consumer's result (not a read).
    evidence    TEXT NOT NULL DEFAULT '' CHECK (evidence IN ('', 'declared', 'incident', 'observed')),
    labels      TEXT NOT NULL DEFAULT '',
    all_labels  INTEGER NOT NULL DEFAULT 0 CHECK (all_labels IN (0, 1)),
    reason      TEXT NOT NULL DEFAULT '',
    result_labels INTEGER NOT NULL DEFAULT 0 CHECK (result_labels IN (0, 1)),
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
    -- Metrics: the scrape interval samples are counted at, and why it was
    -- assumed when no API gave it (empty when measured).
    interval_ms   INTEGER NOT NULL DEFAULT 0,
    interval_assumed TEXT NOT NULL DEFAULT '',
    -- Histogram _bucket series: the bucket count multiplying the series,
    -- and whether exemplar storage is on.
    buckets       INTEGER NOT NULL DEFAULT 0,
    exemplars     INTEGER NOT NULL DEFAULT 0,
    -- Self-hosted: bytes on disk over retention, replicas included.
    disk_bytes    INTEGER NOT NULL DEFAULT 0,
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
