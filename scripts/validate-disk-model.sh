#!/bin/sh
# Check the self-hosted disk model (docs/pricecard.md) against a Prometheus
# server's real block growth (S3 item 8).
#
#   ./scripts/validate-disk-model.sh tvt.db          # local lab, last 7 days
#   PROM=http://prom:9090 DAYS=14 ./scripts/validate-disk-model.sh tvt.db
#
# tvt.db is a scan of that server with a self_hosted price card. The model's
# daily growth is sum(series x 86400 / interval) x bytes_per_sample x
# overhead (one replica). Measured growth is the daily increase of
# prometheus_tsdb_storage_blocks_bytes on days with an hourly point every
# hour or two (the server was up; short scrape gaps are scaled to 24h) and no
# retention deletions; other days are listed and skipped. Each day also shows
# its bytes per appended sample: a day far from the rest stored different
# data (a backfill, a lab's first day), and the median keeps one such day
# from deciding the result.
#
# Unlike tvt, this sends range queries, so they land in the server's query
# log as ad hoc reads of the prometheus_tsdb_* metrics. Only point it at the
# lab or at a partner environment with their consent.
set -eu

DB=${1:?usage: $0 tvt.db}
PROM=${PROM:-http://localhost:9090}
DAYS=${DAYS:-7}
OVERHEAD=${OVERHEAD:-1.2}

exec python3 -I - "$DB" "$PROM" "$DAYS" "$OVERHEAD" <<'EOF'
import json, sqlite3, sys, time, urllib.parse, urllib.request
from datetime import datetime, timezone

db, prom, days, overhead = sys.argv[1], sys.argv[2].rstrip("/"), int(sys.argv[3]), float(sys.argv[4])

con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
scan, bps, retention = con.execute(
    "SELECT id, bytes_per_sample, retention_ms FROM scans WHERE price_kind = 'self_hosted' ORDER BY id DESC LIMIT 1").fetchone() or (None, 0, 0)
if scan is None:
    sys.exit(f"{db}: no scan with a self_hosted price card")
samples = sum(series * 86400 / (interval / 1000) for series, interval in con.execute(
    """SELECT c.active_series, c.interval_ms FROM costs c JOIN signals s ON s.id = c.signal_id
       WHERE c.scan_id = ? AND s.kind = 'metric' AND c.interval_ms > 0""", (scan,)))
predicted = samples * bps * overhead
print(f"model   scan {scan}: {samples / 86400:,.0f} samples/s x {bps:.3f} B/sample x {overhead} overhead = {predicted / 1e6:,.0f} MB/day")

def query_range(expr, start, end, step):
    q = urllib.parse.urlencode({"query": expr, "start": start, "end": end, "step": step})
    res = json.load(urllib.request.urlopen(f"{prom}/api/v1/query_range?{q}"))["data"]["result"]
    return {int(float(t)): float(v) for t, v in res[0]["values"]} if res else {}

today = int(time.time()) // 86400 * 86400
start = today - days * 86400
blocks = query_range("prometheus_tsdb_storage_blocks_bytes", start, today, 3600)
deleted = query_range("prometheus_tsdb_time_retentions_total + prometheus_tsdb_size_retentions_total", start, today, 3600)

def query(expr, at):
    q = urllib.parse.urlencode({"query": expr, "time": at})
    res = json.load(urllib.request.urlopen(f"{prom}/api/v1/query?{q}"))["data"]["result"]
    return float(res[0]["value"][1]) if res else 0.0

growth = []
for d in range(start, today, 86400):
    day = datetime.fromtimestamp(d, timezone.utc).strftime("%Y-%m-%d")
    have = [d + h * 3600 for h in range(25) if d + h * 3600 in blocks]
    if len(have) < 22 or have[0] > d + 3600 or have[-1] < d + 23 * 3600:
        print(f"skip    {day}: {25 - len(have)} of 25 hourly points missing (server down)")
        continue
    if deleted.get(have[-1], 0) != deleted.get(have[0], 0):
        print(f"skip    {day}: retention deleted blocks")
        continue
    g = (blocks[have[-1]] - blocks[have[0]]) * 86400 / (have[-1] - have[0])
    appended = query("sum(increase(prometheus_tsdb_head_samples_appended_total[1d]))", d + 86400)
    per = f", {g / appended:.2f} B/sample over {appended / 86400:,.0f} samples/s appended" if appended else ""
    growth.append(g)
    print(f"day     {day}: {g / 1e6:,.0f} MB{per} (model {(predicted - g) / g:+.1%})")

if not growth:
    sys.exit("no complete day to compare")
growth.sort()
median = growth[len(growth) // 2]
err = (predicted - median) / median
print(f"median  {median / 1e6:,.0f} MB/day over {len(growth)} day(s): model {err:+.1%}, {'within' if abs(err) <= 0.15 else 'OUTSIDE'} 15%")
sys.exit(0 if abs(err) <= 0.15 else 1)
EOF
