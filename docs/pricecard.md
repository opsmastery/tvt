# Price card

The price card turns volume into money. Enter your list or negotiated prices. Any price left out counts as zero. Vendor presets (`preset: true`) are approximate and never billing-accurate.

```yaml
name: acme-2027
currency: USD
metrics:
  per_active_series_month: 0.008    # per active series per month
  per_million_samples: 0            # or per data point (DPM-style pricing)
logs:
  per_gb_ingested: 0.50
  per_gb_month_retained: 0.10
  retention_days: 30
traces:
  per_million_spans: 0
  per_gb_ingested: 0.30
```

## How costs are computed

A month is 30 days.

| Signal | Monthly cost |
| --- | --- |
| Metric or label | `series × per_active_series_month` + `series × (seconds per month ÷ scrape interval) ÷ 1e6 × per_million_samples` |
| Log stream | `GB/day × 30 × per_gb_ingested` + `GB/day × retention_days × per_gb_month_retained` (steady-state stored volume) |
| Traces | `spans/day × 30 ÷ 1e6 × per_million_spans` + `GB/day × 30 × per_gb_ingested` |

The loader for this file isn't built yet; Phase 0 spike 4 settles the format first. The Go types are in `pkg/cost/pricecard.go`.
