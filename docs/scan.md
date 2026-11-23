# tvt scan

`tvt scan` builds the usage graph: every signal your backends hold, what each costs, and every reader tvt can find. It only reads. Each run adds one scan to the database (`tvt.db` by default), and every other command works from that database or from the report built from it.

```sh
export TVT_GRAFANA_TOKEN=...   # Grafana service account, Viewer role (plus datasources:read)
tvt scan --grafana https://grafana.example.com \
  --prom https://prometheus.example.com --loki https://loki.example.com \
  --grafana-log '/var/lib/grafana/logs/grafana.log*' \
  --prom-query-log /prometheus/query.log \
  --loki-query-log '/var/log/loki/loki.log*' \
  --config tvt.yaml
```

## Flags

| Flag | Default | What |
| --- | --- | --- |
| `--grafana` | (required) | Grafana base URL: dashboards, library panels, datasources, alert rules |
| `--grafana-edition` | `oss` | `cloud` reads dashboard views from Grafana Cloud's Usage Insights, and sends `TVT_PROM_USER` and `TVT_LOKI_USER` with the tokens as basic auth. See [grafana-cloud.md](grafana-cloud.md) |
| `--prom` / `--mimir` | (one, for metrics) | The metrics backend. `--mimir` reads Mimir's cardinality API (`limits.cardinality_analysis_enabled: true`); `/prometheus` is appended if missing |
| `--loki` | | Loki: its alert and recording rules, stream volume, detected patterns for the largest streams, and per-level bytes for the largest streams with no level label |
| `--grafana-log` | | Grafana's log with `GF_SERVER_ROUTER_LOGGING=true`. Without it, no dashboard counts as viewed (OSS) |
| `--prom-query-log` | | Prometheus query log (`query_log_file`) |
| `--mimir-query-log` | | Mimir query-frontend log with query stats lines |
| `--loki-query-log` | | Loki's own log, with its `metrics.go` query lines |
| `--probe-wait` | `30s` | How long to wait for tvt's probe query to show up in each query log |
| `--label-top` | `50` | Measure per-label cardinality for this many of the largest metrics |
| `--window` | `30` | The evidence window in days. Recommendations wait until a scan's logs cover it |
| `--config` | | `tvt.yaml` ([example](../examples/tvt.example.yaml)): external consumers, records, SLOs, runbooks and postmortems, ownership, pipelines (a Collector pipeline with `telemetry:` has its exporter rates read), and merged tvt PRs for the savings ledger |
| `--db` | `tvt.db` | The usage graph database. Created on the first scan; each run adds a scan |
| `--allow-insecure-token` | off | Send tokens over plain http to a host that isn't loopback. Don't |

Every log flag is repeatable and takes globs, and `.gz` files are read. Credentials come only from the environment: `TVT_GRAFANA_TOKEN`, `TVT_PROM_TOKEN`, `TVT_LOKI_TOKEN` (and `TVT_PROM_USER`, `TVT_LOKI_USER` for Grafana Cloud).

## What it reads

A reader is anything that would break or go blank if a signal went away. tvt finds them in five places:

- **Dashboards.** Every panel and variable query, parsed for the metrics, labels and streams it selects, with how it uses each label (filter, group, legend).
- **Dashboard views.** Grafana's router log (OSS) or Usage Insights (Cloud), so a dashboard nobody opened protects less than one people look at. tvt's own fetches are excluded by its login.
- **Rules.** Prometheus, Mimir, Loki and Grafana alert and recording rules. A signal an alert reads is a hard keep.
- **Query logs.** Ad hoc and API queries from the Prometheus, Mimir and Loki logs. Rule evaluations are left out, and so are tvt's own queries.
- **tvt.yaml.** `external_consumers` for readers tvt can't see (a billing export, a partner's scrape).

What a signal costs comes from the backends: series counts and per-label cardinality, stream bytes, and your price card ([pricecard.md](pricecard.md)).

## The probe

A log that stopped being written looks exactly like a quiet one. So before collecting anything, each scan sends one marker query to every query log it can check (Prometheus `count(tvt_query_log_probe{nonce="..."})`, which selects nothing) and waits up to `--probe-wait` for it to appear. A log where it never shows is recorded as stale, its coverage is zero, and the signals it would have cleared stay held. Details in [query-logs.md](query-logs.md).

Besides the probe, tvt runs two tagged Loki queries per large unlabelled stream (`sum by (detected_level, event_name)` of `bytes_over_time(...[1h])` and of `count_over_time(...[1h])`, `X-Query-Tags: Source=tvt-levels`), one attribute sample per large stream (the plain selector, up to 1,000 lines from the last hour, `Source=tvt-attrs`: only key names, sizes and distinct-value counts by hash are kept), and, on Grafana Cloud, the Usage Insights query. Each is dropped from usage by the readers.

## Coverage and gaps

Every evidence source is stored with its coverage: the share of the window its data spans. A source you didn't configure is stored at zero, so the report can say what it couldn't see. `tvt report` and `tvt why` list these as evidence gaps. A signal is only called unread when the sources that could have shown a read cover the window.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | The scan was written |
| 1 | The scan failed. Nothing was written: each run is one transaction, and collection finishes before the database is opened |
| 2 | Bad flags |

## Next

`tvt report` turns the latest scan into the ranked report ([report.md](report.md)). `tvt why <signal>` shows one signal's readers and evidence ([why.md](why.md)).
