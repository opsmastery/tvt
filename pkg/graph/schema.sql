-- Usage graph store (SQLite for the CLI, Postgres for continuous mode).
-- Keep this DDL portable between the two.

CREATE TABLE IF NOT EXISTS signals (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('metric', 'label', 'stream', 'span_attribute')),
    name        TEXT NOT NULL,
    parent      TEXT NOT NULL DEFAULT '',
    source      TEXT NOT NULL,
    first_seen  TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS signals_name ON signals (kind, name);

CREATE TABLE IF NOT EXISTS consumers (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    name        TEXT NOT NULL,
    team        TEXT NOT NULL DEFAULT '',
    last_viewed TIMESTAMP
);

CREATE TABLE IF NOT EXISTS refs (
    signal_id   TEXT NOT NULL REFERENCES signals (id),
    consumer_id TEXT NOT NULL REFERENCES consumers (id),
    last_seen   TIMESTAMP,
    actor       TEXT NOT NULL DEFAULT '',
    via         TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (signal_id, consumer_id, via)
);
CREATE INDEX IF NOT EXISTS refs_consumer ON refs (consumer_id);

CREATE TABLE IF NOT EXISTS costs (
    signal_id     TEXT NOT NULL REFERENCES signals (id),
    observed_at   TIMESTAMP NOT NULL,
    active_series INTEGER NOT NULL DEFAULT 0,
    bytes_per_day INTEGER NOT NULL DEFAULT 0,
    spans_per_day INTEGER NOT NULL DEFAULT 0,
    monthly_cost  REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (signal_id, observed_at)
);

-- One row per scan, so savings can be measured between scans.
CREATE TABLE IF NOT EXISTS scans (
    id            INTEGER PRIMARY KEY,
    started_at    TIMESTAMP NOT NULL,
    window_days   INTEGER NOT NULL,
    log_coverage  REAL NOT NULL DEFAULT 0  -- 0..1, share of sources with usable query logs
);
