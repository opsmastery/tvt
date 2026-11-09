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

## How costs are computed

A month is 30 days.

| Signal | Monthly cost |
| --- | --- |
| Metric or label | `series × per_active_series_month` + `series × (seconds per month ÷ scrape interval) ÷ 1e6 × per_million_samples` |
| Log stream | `GB/day × 30 × per_gb_ingested` + `GB/day × retention_days × per_gb_month_retained` (steady-state stored volume) + `events/day × 30 ÷ 1e6 × per_million_events_indexed` |
| Traces | `spans/day × 30 ÷ 1e6 × per_million_spans` + `GB/day × 30 × per_gb_ingested` |

## Status

The Go types are in `pkg/cost/pricecard.go`. They cover the flat prices but not yet `version`, `included_*`, `per_million_events_indexed`, `source` or `as_of`. Those land with the YAML loader in Sprint 3.
