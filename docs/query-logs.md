# Query logs: what each backend records

Phase 0 spike 2 (Nov 8, 2026, tvt-lab: Prometheus 3.5.0, Loki 3.5.3, Mimir 2.16.0, Grafana 12.1.0). This is the source material for the partner setup guide, now published as a separate setup guide. Keep the two in sync when a setting changes. Fixtures for Prometheus and Grafana are in `testdata/lab/`.

## Summary

| Source | Enable with | Format | Query text | Who / what sent it | Rule evaluations | Dashboard + panel |
| --- | --- | --- | --- | --- | --- | --- |
| Prometheus | `query_log_file` in `prometheus.yml` (off by default) | JSON lines in a file | yes | client IP only | logged, tagged `ruleGroup` | no |
| Loki | on by default at `log_level: info` | logfmt in Loki's own log | yes | `source=` only if the client sends `X-Query-Tags` | logged, `component=ruler` | no |
| Mimir query-frontend | `frontend.log_queries_longer_than: -1s` (plus `query_stats_enabled: true`) | logfmt in Mimir's own log | yes | `user_agent`; tenant in `user=` | not logged (ruler queries ingesters directly in the default mode) | **yes**, with `log_query_request_headers` |
| Grafana router log | `GF_SERVER_ROUTER_LOGGING=true` | logfmt in Grafana's own log | no | `uname` | n/a | dashboard loads only (`GET /api/dashboards/uid/:uid`) |

**Correction to spike 1.** Grafana 12 *does* forward `X-Dashboard-Uid`, `X-Panel-Id` and `X-Grafana-Org-Id` to the datasource on `/api/ds/query`. Spike 1 saw no attribution because Prometheus doesn't log request headers. Mimir does when asked to, so on Mimir panel usage needs no expression matching. `X-Grafana-User` is not forwarded by default. With `[dataproxy] send_user_header = true` (`GF_DATAPROXY_SEND_USER_HEADER=true`) Grafana 12.1 forwards the login on both `/api/ds/query` and the datasource proxy (`/api/datasources/proxy/uid/...`), and Mimir logs it as `header_x_grafana_user=admin` (verified Nov 9).

## Prometheus

```
query_log_file: /prometheus/query.log
```

One JSON object per line: `time` (RFC 3339, nanoseconds; Prometheus 2.x wrote `ts`), `params.query`, `params.start`/`end`/`step`, `stats.timings`, `stats.samples`. Instant queries have `start` equal to `end` and `step: 0`. Grafana resolves `$__rate_interval` before sending, so the logged text has a concrete range (`[40s]`) that changes with the panel's time range and width.

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
- Rule evaluations don't pass through the query-frontend in Mimir's default (internal) ruler mode, so nothing needs excluding.
- With the remote ruler (`ruler.query_frontend.address`), every rule evaluation *does* appear as a `query stats` line: `path=/prometheus/api/v1/query` (instant), `user_agent=mimir/2.16.0`, no Grafana headers, and `param_time` one minute behind wall time (the ruler's evaluation delay). There is no rule-group field. Exclude on the `mimir/` user-agent prefix (verified Nov 9 in a throwaway single-binary Mimir).

## Grafana router log

With `GF_SERVER_ROUTER_LOGGING=true`, every request writes a `msg="Request Completed"` logfmt line: `uname`, `t`, `method`, `path`, `status`, `referer`, `handler`. A dashboard load is `handler=/api/dashboards/uid/:uid`, `method=GET`, `status=200`.

- **tvt's own scan looks like a view.** Fetching dashboard JSON is the same request a browser makes. Two filters separate them: a browser load carries `referer=…/d/<uid>/…` and an API client sends none, and tvt excludes its own login (`GET /api/user` returns the service account's login, e.g. `sa-1-tvt`). Verified Nov 9 against the lab.
- **Retention.** Grafana logs to the console by default, which is lost when the container is recreated. File logging keeps only 7 days by default. Partners need `[log] mode = console file` with `[log.file] max_days` of at least 30, or the log shipped somewhere (Loki). The lab uses `GF_LOG_MODE="console file"`, `GF_PATHS_LOGS=/var/lib/grafana/logs` (on the data volume) and `GF_LOG_FILE_MAX_DAYS=45`.
- No steady heartbeat: a quiet night logs nothing, so coverage is the share of the window after the first line.
- One organization per scan for now. The line has `orgId`, and dashboard UIDs are only unique within an org.

## Overhead (measured Nov 9)

The cost of query logging is disk and log volume, not query latency.

| Source | Bytes per line | Lines per query | Notes |
| --- | --- | --- | --- |
| Prometheus `query_log_file` | ~635 | 1 per API query **and 1 per rule evaluation** | Lab: 86% of lines were rule evaluations. Volume ≈ (rules × evaluations/day + API queries/day) × 635 B. |
| Loki `metrics.go` | ~1,450 (frontend line ~1,530) | 1 frontend + 1 or more querier lines; ruler lines per evaluation | Lab: `metrics.go` lines were about half of Loki's own log bytes. |
| Mimir `query stats` | ~860 with header logging | 1 | Rule evaluations add lines only in remote-ruler mode. |

- **Latency.** A throwaway Prometheus 3.5.0 ran 3,000 sequential instant queries with the log off and on, two rounds each (lab VM, so noisy): p50 1.86 ms off against 1.88 and 2.25 ms on, p99 3.4–3.6 ms off against 3.4 and 4.7 ms on. The cost is at most about 0.5 ms per query and often within noise. Not measured under concurrent load.
- **Sizing example for partners.** 2,000 rules at a 30 s interval produce 5.76 M rule lines a day, about **3.7 GB/day** of Prometheus query log before compression, even with no dashboard traffic. Partners with many rules need rotation (Prometheus reopens the file on `SIGHUP` or `/-/reload`) or should ship the file and drop `ruleGroup` lines at the shipper. tvt counts rules through the rules API, so it doesn't need those lines.

## Not yet measured

- Query-log overhead under concurrent query load and at partner scale.
- The Mimir cardinality API against real data (the lab's Mimir only receives data under `make up-mimir`).

## Classification rules for collectors

1. Drop rule evaluations (`ruleGroup`, `component=ruler`).
2. Drop Loki sub-queries and metadata calls (keep `component=frontend` with `query_type` in `limited`, `filter`, `metric`).
3. Drop tvt's own queries (`source=tvt`, `user_agent=tvt/...`) and Mimir remote-ruler evaluations (`user_agent=mimir/...`).
4. Attribute (S3 item 5): every logged query gets a client, stored on its consumer (`consumers.client`):
   - `dashboard`: Mimir's headers name the panel, or the normalized query matches a collected panel target, variable or annotation query (Grafana variables match any text, `[$__rate_interval]` folds like `[40s]`). Evidence for that panel: it ran (`last_viewed`).
   - `rule`: it matches the query of a rule Grafana evaluates (Grafana-managed). Evidence of evaluations, for S6's cost per rule.
   - `agent`: an agent named in `tvt.yaml` `clients.agents` (user agent regex, address or CIDR, or the actor its token belongs to), or a user agent that says `mcp`. On Prometheus only the address works.
   - `explore`: Grafana sent it (user agent `Grafana/`, an address in `clients.grafana`, or, without that, an address that sent a matching query) and it matches nothing.
   - `api`: anyone else.

   A query matching several collected queries is evidence for each: unknown means used. With `clients.grafana` set, only Grafana's addresses are matched against templates.

   Container addresses are reused: in the lab, after a restart the agent got the address Grafana had before, so a 30-day Prometheus log can hold both under one IP. Matching on text comes first for that reason, and an inferred Grafana address only counts within an hour of its matching queries. Configure `clients.grafana` where addresses are stable.

## Rule evaluations in the query logs (Sprint 2 item 12)

Checked on the lab Nov 15, 2026 (Grafana 12, Loki 3.5, Prometheus 3.5), with the rule-efficiency seeds:

- **Prometheus' own ruler** logs each rule's evaluation with `ruleGroup` (`file`, `name`) and `stats.samples.totalQueryableSamples`. tvt still leaves these out of usage (rules come from the rules API) and uses them as the log's heartbeat; since S6 item 1 they also price each rule (`prometheus.QueryLog.RuleEvaluations`).
- **Loki's ruler** logs `caller=metrics.go component=ruler evaluation_mode=local` lines with the query (a binary expression wrapped in parentheses), `total_bytes` and `total_lines`, but no rule or group name (`loki.QueryLog.RuleEvaluations`).
- **Grafana-managed rules** carry no marker. Their Loki evaluations are ordinary `component=frontend` lines (`query_type=metric`, `range_type=instant`, `length=0s`) with no `source`, user or rule header; their Prometheus evaluations are `POST /api/v1/query` instant queries from Grafana's IP with no user agent. Only the query text, the instant type and the cadence tie them to the rule, which is how S3 item 5's matching and S6 item 1's cost per rule join them. Both carry per-evaluation cost (Loki `total_bytes`, Prometheus `totalQueryableSamples`), which `collect.LoggedQuery.Bytes`/`Samples` keep, with `Instant` (S6 item 2).
- Restarting Grafana can change its container address (the lab's went from 10.89.0.17 to .29), another reason matching is on text first.

## Sweeps (S3 item 6)

An agent's "what's here" (`count by (__name__) ({__name__=~".+"})`) touches every metric without depending on any. A query from an observed consumer (query log, dashboard) whose selector names no metric outright, restricts no label, and matches every metric name or selects more than `sweep_share` of the series (`tvt.yaml`, default 0.5) is a sweep: its references are stored with `refs.sweep = 1` and keep nothing on their own. A metric the same query names explicitly (`... + node_load1`) is still read. Alerts, recording rules, SLOs, runbooks and declared external consumers never sweep. tvt's own queries stay out through `X-Query-Tags: Source=tvt`.

## Proving a query log (Sprint 2 item 4)

A log that stopped being written, or a stale copy, looks exactly like a quiet one. So each `tvt scan` sends one marker query to every query log it can probe, before collecting anything, and re-reads the log for up to `--probe-wait` (default 30s) until the marker shows up:

```
count(tvt_query_log_probe{nonce="<16 hex digits>"})                      Prometheus, Mimir
count_over_time({tvt_query_log_probe="<16 hex digits>"}[1m])             Loki (S5 item 5)
X-Query-Tags: Source=tvt-probe                                           both
```

- It selects no series (verified in the lab: Mimir logs it with `fetched_series_count=0`), and every reader drops it from usage.
- Prometheus logs it as an API query (match on the text; Prometheus doesn't log headers). Mimir logs it with `header_x_query_tags="Source=tvt-probe"` when `X-Query-Tags` is in `log_query_request_headers`.
- Loki (`/loki/api/v1/query`, instant) logs the marker as a `caller=metrics.go component=frontend query_type=metric ... source=tvt-probe` line, plus a querier line and a `query_type=stats` line for the bare selector (checked on the lab's Loki 3.5, Nov 15: in the container log within 3s). Only the frontend line proves the log; the reader drops every marker from usage by its text, so a proxy that strips `X-Query-Tags` doesn't turn it into a read. No stream carries the `tvt_query_log_probe` label, so it touches no chunk.
- If the marker never appears, the probe request fails, or tvt can't send one (a Prometheus log in a `--mimir` scan), the source's coverage is 0 and the scan records the gap (`scan_sources.gap`). The log's queries still count: unknown means used.
- So tvt must read the **live** log, not a copy made before the scan. Partners point `--prom-query-log` at the file Prometheus writes (or a mount of it), and `--mimir-query-log`/`--loki-query-log` at the backends' container logs: on Kubernetes the node's `/var/log/pods/...` files, which tvt reads in the CRI format.
