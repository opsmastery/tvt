# Query logs: what each backend records

Phase 0 spike 2 (Nov 8, 2026, tvt-lab: Prometheus 3.5.0, Loki 3.5.3, Mimir 2.16.0, Grafana 12.1.0). This is the source material for the partner setup guide. Fixtures for Prometheus and Grafana are in `testdata/lab/`.

## Summary

| Source | Enable with | Format | Query text | Who / what sent it | Rule evaluations | Dashboard + panel |
| --- | --- | --- | --- | --- | --- | --- |
| Prometheus | `query_log_file` in `prometheus.yml` (off by default) | JSON lines in a file | yes | client IP only | logged, tagged `ruleGroup` | no |
| Loki | on by default at `log_level: info` | logfmt in Loki's own log | yes | `source=` only if the client sends `X-Query-Tags` | logged, `component=ruler` | no |
| Mimir query-frontend | `frontend.log_queries_longer_than: -1s` (plus `query_stats_enabled: true`) | logfmt in Mimir's own log | yes | `user_agent`; tenant in `user=` | not logged (ruler queries ingesters directly in the default mode) | **yes**, with `log_query_request_headers` |
| Grafana router log | `GF_SERVER_ROUTER_LOGGING=true` | logfmt in Grafana's own log | no | `uname` | n/a | dashboard loads only (`GET /api/dashboards/uid/:uid`) |

**Correction to spike 1.** Grafana 12 *does* forward `X-Dashboard-Uid`, `X-Panel-Id` and `X-Grafana-Org-Id` to the datasource on `/api/ds/query`. Spike 1 saw no attribution because Prometheus doesn't log request headers. Mimir does when asked to, so on Mimir panel usage needs no expression matching. `X-Grafana-User` was not forwarded with default settings; Grafana's `dataproxy.send_user_header` probably controls it (not yet tested).

## Prometheus

```
query_log_file: /prometheus/query.log
```

One JSON object per line: `params.query`, `params.start`/`end`/`step`, `stats.timings`, `ts`.

- Rule evaluations carry `ruleGroup`. Exclude them: rules are counted through the rules API.
- API queries carry `httpRequest.clientIP` and `httpRequest.path`. In the lab, Grafana's IP (`10.89.0.6`) sent 72 of 75 API queries in the last 500 lines; the rest were the viewer's direct "Explore" queries.
- No user, dashboard or panel. Panel usage comes from matching the query text against panel expressions after normalizing Grafana variables.
- The file isn't rotated by Prometheus. Partners need logrotate (Prometheus reopens the file on `SIGHUP` or `/-/reload`) or a log shipper.

## Loki

Every query writes a `caller=metrics.go` line at info level. One query produces several lines, so filter carefully:

| Line | Meaning | Use as evidence? |
| --- | --- | --- |
| `component=frontend`, `query_type=limited\|filter\|metric` | the user-facing query | **yes** (one per query) |
| `component=querier`, same `traceID` | sub-query of the above | no (duplicate) |
| `component=ruler` | rule evaluation | no (counted via the rules API) |
| `query_type=stats\|volume\|labels\|series` (callers `metrics.go:292/409/484`) | metadata calls: Grafana's query inspector, label browser, tvt's own volume scan | no |

Fields used: `ts`, `query`, `query_type`, `range_type`, `length`, `step`, `org_id`, `traceID`, and `source` when present.

- Loki logs the `X-Query-Tags` header as `source=...` (verified: `X-Query-Tags: Source=tvt-spike` → `source=tvt-spike`). Grafana's panel queries don't set it in 12.1, so Loki can't distinguish Grafana from other clients. tvt must send `X-Query-Tags: Source=tvt` on its own Loki calls, so a scan never counts its own queries as usage.
- The lines are in Loki's own log, not a separate file. Partners have to ship Loki's logs somewhere tvt can read them, usually Loki itself (`{container="loki"} |= "metrics.go"`).

## Mimir query-frontend

```yaml
frontend:
  log_queries_longer_than: -1s     # negative = log every query
  query_stats_enabled: true
  log_query_request_headers: "X-Dashboard-Uid,X-Panel-Id,X-Grafana-User,X-Query-Tags,X-Grafana-Org-Id"
```

One `msg="query stats"` line per query from `component=query-frontend`, with `param_query`, `param_start`/`param_end`/`param_step`, `path`, `user_agent`, `user` (the tenant, not a person) and, with header logging on, `header_x_dashboard_uid=shop-overview header_x_panel_id=1 header_x_grafana_org_id=1`.

- **Best source of panel usage found so far.** It fills `LoggedQuery.DashboardUID` and `PanelID` directly.
- `user_agent=Grafana/12.1.0` separates Grafana traffic from scripts and other API clients.
- Rule evaluations don't pass through the query-frontend in Mimir's default (internal) ruler mode, so nothing needs excluding. With the remote ruler (`ruler.query_frontend.address`) they would appear; check `user_agent` there (not yet tested).

## Not yet measured

- Overhead of query logging at volume (Prometheus file I/O, Loki/Mimir log volume). Expect one line per query; the lab is too small to measure.
- Mimir with the remote ruler; `X-Grafana-User` forwarding via `send_user_header`.
- The Mimir cardinality API against real data (the lab's Mimir only receives data under `make up-mimir`).

## Classification rules for collectors

1. Drop rule evaluations (`ruleGroup`, `component=ruler`).
2. Drop Loki sub-queries and metadata calls (keep `component=frontend` with `query_type` in `limited`, `filter`, `metric`).
3. Drop tvt's own queries (`source=tvt`, `user_agent=tvt/...`).
4. Attribute: Mimir headers → dashboard and panel; Grafana IP or user agent → "Grafana, unattributed" (match against panel expressions); anything else → "other API client" (ad hoc weight).
