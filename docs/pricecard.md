# Price card

The price card turns volume into money. Enter your list or negotiated prices. Any price left out counts as zero. Vendor presets are approximate and never billing-accurate.

Format version 1 (Phase 0 spike 4, Nov 8, 2026):

```yaml
version: 1
name: acme-2027
currency: USD
metrics:
  per_active_series_month: 0.008    # per active series per month
  per_million_samples: 0            # or per data point (DPM-style pricing)
  included_active_series: 0         # free or committed allotment
logs:
  per_gb_ingested: 0.50
  per_gb_month_retained: 0.10
  retention_days: 30
  per_million_events_indexed: 0     # Datadog/Splunk-style indexing (Phase 3 backends)
  included_gb_month: 0
traces:
  per_million_spans: 0
  per_gb_ingested: 0.30
```

A preset adds where its numbers came from:

```yaml
preset: true
source: https://example.com/pricing   # vendor pricing page
as_of: 2026-11-08                     # when the numbers were copied
```

## Decisions

1. **Enter marginal prices.** Use the price of the *next* unit, not the average. With tiered or volume pricing, that's the rate of the tier you're in now. Savings come off the top tier, so the marginal price is what a drop actually saves. A single number per unit keeps the format simple, and no tier tables are needed.
2. **Included allotments are applied to the total, not per signal.** Each signal is priced at the marginal rate. If total usage falls below `included_*` after a change, the report caps the saving at what was actually billable and says so.
3. **Committed contracts.** Savings under a committed-spend contract only become cash at renewal. The report prints "savings at your entered rates" and never claims an immediate cash reduction.
4. **Unknown keys are an error.** The loader decodes strictly, so a typo like `per_gb_ingest` fails loudly instead of silently counting as zero.
5. **`version` is required.** A breaking change bumps it, and the loader rejects versions it doesn't know.
6. **Presets ship with `source` and `as_of`,** and the report header shows them. tvt ships no preset whose numbers haven't been checked against the vendor's public page. Presets are added in Sprint 3, starting with the first partner's vendor.
7. **Self-run stacks** (Prometheus, Mimir, Loki on your own infrastructure) have no vendor price. Use `kind: self_hosted` (below): tvt models the disk each metric holds and prices it per GB-month. A per-series internal rate on a vendor card still works if you have one.

Out of scope for v1: per-team or per-environment price overrides (Phase 2), and currency conversion.

## Using a card

In `tvt.yaml`, `pricecard:` is a file (relative to `tvt.yaml`) or the name of a shipped preset:

```yaml
pricecard: ./pricecard.yaml   # or: grafana-cloud
```

Shipped presets (`pkg/cost/presets`):

| Preset | Source | As of | Notes |
| --- | --- | --- | --- |
| `grafana-cloud` | grafana.com/pricing | 2026-11-14 | Pro list prices: $6.50 per 1k series (10k included), logs $0.45/GB ingested ($0.05 processing + $0.40 writing; 50 GB included). The page's $0.10/GB "Retaining" has no stated unit, so retention past the included 30 days isn't priced. Volume discounts aren't modeled. |

Each scan records the card it used (name, currency, preset) next to its costs. Without a card, costs are recorded with zero money and their volume.

## Self-hosted cards

A self-hosted card prices storage, not a vendor's meter:

```yaml
version: 1
kind: self_hosted        # default: vendor
name: on-prem
currency: USD
self_hosted:
  storage_gb_month: 0.08   # required: what a GB of disk costs a month
  replicas: 2              # copies of every sample (an HA pair: 2); default 1
  overhead: 1.2            # index, tombstones, compaction headroom; default 1.2
  bytes_per_sample: 0      # override; 0 measures it
  retention_days: 0        # override; 0 measures it (required for Mimir)
  fixed_monthly: 1500      # servers, licences: never attributed to a signal or counted as savings
  node_sizes:              # optional, at most one: when a cut frees a node
    - {name: storage, disk_gb: 500, monthly: 400}
```

**Disk per metric** = `series × (retention ÷ interval × bytes_per_sample × overhead + head share) × replicas`, and its monthly cost is that in GB × `storage_gb_month`. A label costs its share of its metric's disk, as on a vendor card. Each cost stores the bytes (`costs.disk_bytes`).

**Inputs**, each recorded on the scan with where it came from:

| Input | From, in order |
| --- | --- |
| Bytes per sample | the card; Prometheus `/metrics` (`prometheus_tsdb_compaction_chunk_size_bytes_sum ÷ …_chunk_samples_sum`, since the server started); 1.5, marked assumed (before the first compaction, or Mimir) |
| Retention | the card; Prometheus `prometheus_tsdb_retention_limit_seconds`, shortened when `…_retention_limit_bytes` fills first at the modeled bytes a day (with size-only retention, the size limit alone decides). Mimir's retention lives in its limits and object storage tvt doesn't read, so its card must set `retention_days`. |
| Head share | Prometheus (`prometheus_tsdb_wal_storage_size_bytes` + `prometheus_tsdb_head_chunks_storage_size_bytes`) ÷ `prometheus_tsdb_head_series`; 0 for Mimir |

tvt reads `/metrics` with a GET. It never runs a query, so nothing lands in the query log as usage.

**Nodes.** Disk is bought in nodes, so a cut that frees disk saves money only when it frees a whole node. `NodeStep` reports the node count before and after: `ceil(total bytes ÷ disk_gb)`, at least one. Worked example: 2,400 GB on 500 GB nodes is 5 nodes. A cut that frees 120 GiB leaves 2,271 GB, still 5 nodes, so it saves $0 of node cost (to be reported as "frees 120 GiB; node count unchanged"). A cut that frees 600 GB leaves 1,800 GB, 4 nodes: $400/month.

### Checked against the lab (S3 item 8)

`scripts/validate-disk-model.sh tvt.db` compares the model's daily growth (one replica: `Σ series × 86400 ÷ interval × bytes_per_sample × overhead`) with the daily increase of `prometheus_tsdb_storage_blocks_bytes`. It needs a scan with a `self_hosted` card. Unlike tvt, it sends range queries, so point it only at the lab or a consenting partner.

Lab, Nov 14, 2026 (scan of 1,548 metrics, 61,368 series; 0.607 bytes/sample measured):

| | MB/day | Model |
| --- | --- | --- |
| Model: 6,123 samples/s × 0.607 × 1.2 | 385 | |
| Nov 11 (5,377 samples/s appended; 0.85 B/sample stored) | 394 | −2.2% |
| Nov 12 (5,986 samples/s; 0.81 B/sample) | 417 | −7.7% |
| Nov 10 (5,948 samples/s; 1.70 B/sample) | 875 | outlier |
| **Median of the three** | **417** | **−7.7%: within 15%, no model change** |

What the numbers say:

- **The overhead factor earns its place.** Chunks alone (0.607 B/sample) would predict 321 MB/day, 20–23% low. A block also holds its index, and that costs most in small blocks: by `promtool tsdb list`, the lab's 2h blocks store 0.81–0.84 B/sample, its 6h blocks 0.72–0.74. The default 1.2 (0.73 B/sample) matches compacted blocks and runs a little under fresh 2h ones.
- **Bytes per sample follows the data, and the data changes.** Nov 10 appended as many samples as any other day but stored 1.70 B/sample: the lab's first day of data (the Nov 9–10 block holds 1.62 B/sample all-in) compressed far worse than later days. The model measures bytes per sample from compactions since the server last started, so it tracks recent data rather than a server's whole history. The script shows each day's bytes per sample so a day like this stands out, and the median keeps it from deciding.
- **The sample rate is close.** The model's rate (series ÷ scrape interval) was 6,123/s against 5,377–5,986/s appended (2–14% high): series in the head that stopped being scraped still count until head compaction drops them.

Limits: three usable days, not yet a week. Prometheus was down most of Nov 13 and Nov 9 is partial. Re-run the script once the lab has seven complete days. Not checked here: replicas (one server) and the size-cap retention path (the lab's 20 GB cap is far off).

## How costs are computed

A month is 30 days.

| Signal | Monthly cost |
| --- | --- |
| Metric | `series × per_active_series_month` + `series × (seconds per month ÷ interval) ÷ 1e6 × per_million_samples` |
| Label | the metric's cost per series × the series carrying the label × (1 − 1 ÷ distinct values): what dropping it would collapse, assuming its values are independent of the other labels. A label with one value multiplies nothing. |
| Log stream | `GB/day × 30 × per_gb_ingested` + `GB/day × retention_days × per_gb_month_retained` (steady-state stored volume) + `events/day × 30 ÷ 1e6 × per_million_events_indexed` |
| Traces | `spans/day × 30 ÷ 1e6 × per_million_spans` + `GB/day × 30 × per_gb_ingested` |

**The interval** a metric's samples are counted at is, in order: its recording rule's evaluation interval; the shortest scrape interval of the jobs exposing it (Prometheus `/api/v1/targets` per job, `/api/v1/targets/metadata` for which jobs expose which metric family); for a scraped series no metadata names (`up`, `scrape_*`), the shortest job interval, marked assumed; without a targets API (Mimir), 1m, marked assumed. The interval and any assumption are stored with the cost.

**Histograms:** a `_bucket` metric's series are its dimensions × its buckets, so the bucket count (`le` values, when its labels were collected) is stored with its cost, and exemplars are flagged when Prometheus runs with `exemplar-storage`.

**Log streams.** tvt reads every stream's bytes over the last 24 hours from Loki's index volume API (`/loki/api/v1/index/volume`, answered from the index without reading chunks), one request per label name (`{name=~".+"}`, grouped by every label name; Loki's own `__stream_shard__` is left out so a sharded stream counts once). Each stream signal (a selector a query reads) costs the streams its selector matches. Selectors overlap, so their costs aren't additive. Each stream's own bytes are stored too (`stream_volumes`, by full label set), so any selector or label drop can be priced later without rescanning. A response that hits the 10,000-series limit is flagged on the scan (`volume_truncated`): stream bytes are then low.

**Included allotments** come off the total (`MetricsBillable`, `LogsBillable`), at the marginal rate, never below zero; each signal keeps its marginal cost.

## What-if pricing

A stored scan can be priced at any other card without rescanning: "what would this usage cost on vendor X, or on our own disks?" is answered with your own series, intervals and bytes. `cost.Reprice(card, costs, tsdb)` takes a scan's stored cost rows and its stored TSDB measurements (`scans.tsdb_*`, read from Prometheus's `/metrics` on every Prometheus scan whatever the card) and returns the costs that card gives. It's exactly what a scan with that card would have recorded; the golden test checks this signal by signal, there and back. `tvt report --price-card <file|preset>` exposes it in S4.

Limits: Mimir scans and scans from before schema v11 have no TSDB measurements, so a self-hosted card needs `retention_days` to price them (bytes per sample is then assumed, 1.5). A self-hosted card has no log rates, so streams cost zero on it.

## Status

Loader, presets and metric and label costs: S3 item 1. The self-hosted kind for metrics: item 2. Log streams from Loki's volume API: item 3. What-if pricing (`cost.Reprice`): item 4. A self-hosted card has no log rates yet, so streams on it record bytes with zero money. Item 8 checked the disk model against the lab's real block growth (−7.7%, above). `per_million_events_indexed` is read and validated, but no backend reports indexed events yet. Not yet: Grafana Cloud Adaptive Metrics ("already handled" series, never counted as savings), which comes with the Cloud adapters (Sprint 2 item 11).
