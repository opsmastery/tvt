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
7. **Self-run stacks** (Prometheus, Mimir, Loki on your own infrastructure) have no vendor price. Enter an internal rate, for example infra cost ÷ active series. The report labels these as estimated.

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

**Included allotments** come off the total (`MetricsBillable`, `LogsBillable`), at the marginal rate, never below zero; each signal keeps its marginal cost.

## Status

Loader, presets and metric and label costs: S3 item 1. Log streams are priced from Loki's volume API in item 3; the self-hosted kind is item 2. `per_million_events_indexed` is read and validated, but no backend reports indexed events yet. Not yet: Grafana Cloud Adaptive Metrics ("already handled" series, never counted as savings), which comes with the Cloud adapters (Sprint 2 item 11).
