# tvt: Telemetry Value Tracker

A Go CLI that finds telemetry (metrics, labels, log streams, span attributes) that costs money but nobody uses, and opens PRs that drop, downsample or reroute it. A `tvt check` CI gate blocks any change that would break an alert, SLO or viewed dashboard.

- Product spec: `../Spec Telemetry Value Tracker.md`
- Development plan (phases, sprints, spike results): `docs/plan.md`
- Architecture: `docs/architecture.md`. Decisions: `docs/adr/`
- Test lab: `../tvt-lab` (Podman Grafana stack; `ground-truth.yaml` there is the expected output)

## Commands

```sh
make           # gofmt check, go vet, tests, build -> bin/tvt
make race      # tests with -race
./scripts/capture-fixtures.sh   # record real lab API responses into testdata/lab/
```

## Conventions

- Go 1.26 (ADR 0004: track a supported Go release). Module: `github.com/Opsmastery-Summit/tvt`. Single static binary (`CGO_ENABLED=0`).
- Standard library first (ADR 0004). Expected dependencies: Prometheus `promql/parser`, Loki LogQL syntax (or a vendored subset), `modernc.org/sqlite`, a YAML library, and GitHub/GitLab clients.
- Read-only against every backend. Credentials come only from env vars (`TVT_GRAFANA_TOKEN`, `TVT_PROM_TOKEN`, `TVT_LOKI_TOKEN`, `GITHUB_TOKEN`), never flags, and are never logged.
- Collectors implement the interfaces in `pkg/collect` and are tested against recorded fixtures in `testdata/`, never a live stack.
- `pkg/score`, `pkg/cost`, `pkg/graph` and `pkg/parse` stay I/O-free. Legacy Bridge will import them.
- `check` fails closed: a config it can't evaluate is a failure.
- Commits are authored as Jason Vance.

## Status (Nov 9, 2026)

Done:
- CLI skeleton.
- Scoring and cost models with tests.
- Graph model and SQLite schema.
- Collector and parser interfaces.
- CI and goreleaser.
- Lab running on Jason's Mac since Nov 8, so 30 days of history arrives around Dec 8.

Phase 0 spike findings:
- **Spike 1:** OSS Grafana's own logs carry no dashboard or panel attribution for `/api/ds/query`. Dashboard views come from Grafana router-log lines for `GET /api/dashboards/uid/:uid` (logfmt: `uname`, `t`, `path`, `handler=/api/dashboards/uid/:uid`, `status`). Grafana *does* forward `X-Dashboard-Uid`/`X-Panel-Id` to datasources; Prometheus just doesn't log them (see spike 2).
- **Spike 2:** details in `docs/query-logs.md`.
  - Prometheus: JSON lines. Exclude `ruleGroup` entries (rule evaluations). `httpRequest.clientIP` equal to Grafana's IP means dashboard or Explore traffic. Panel usage needs expression matching after normalizing Grafana variables.
  - Loki: `caller=metrics.go` lines in Loki's own log. Count only `component=frontend` with `query_type` in limited/filter/metric. `source=` comes from `X-Query-Tags`, and tvt must send `X-Query-Tags: Source=tvt`.
  - Mimir: query-frontend `msg="query stats"` lines. With `log_query_request_headers` they carry `header_x_dashboard_uid`/`header_x_panel_id`, which give direct panel attribution.
- **Spike 3:** Loki's LogQL package is AGPL-3.0 and adds about 20 MB and 330 modules. Write our own extractor (ADR 0005). `promql/parser` is Apache 2.0 and uses `parser.NewParser(parser.Options{}).ParseExpr` (there is no package-level `ParseExpr` in Prometheus v0.316).
- **Spike 4:** price card format v1 in `docs/pricecard.md`: marginal prices, included allotments applied to the total, strict decoding, a required `version`.
- **Nov 9 follow-ups** (in `docs/query-logs.md`): `GF_DATAPROXY_SEND_USER_HEADER=true` makes Grafana forward `X-Grafana-User`, which Mimir logs. Mimir remote-ruler evaluations show up in query-frontend logs with `user_agent=mimir/...`, so drop them. Prometheus query-log cost is disk (~635 B/line, one line per rule evaluation), not latency. The Prometheus query-log timestamp is `time` in 3.x and `ts` in 2.x.
- Still open: overhead under concurrent load at partner scale.

Sprint 1 (Dec 1-12, started early):
1. Done Nov 9: Prometheus collector in `pkg/collect/prometheus`. TSDB status raises `limit` until per-metric counts sum to `headStats.numSeries`. Series are decoded as a stream and a truncated result is an error. Rules reject unknown types. The query log reads globs and `.gz` files, and rule-evaluation lines act as a coverage heartbeat. It never runs PromQL, because Prometheus can't tell tvt's queries apart from real usage. It also works for Mimir via `BaseURL=.../prometheus`, `Source="mimir"`.
2. Done Nov 9: Grafana collector in `pkg/collect/grafana`. Dashboards come from search, dashboard JSON and library panels. Datasources resolve from the legacy name string, `null` (the default), `-- Mixed --` with per-query overrides, datasource variables and built-ins. A dashboard tvt can't read is an error. `RouterLog` implements `collect.ViewSource`; `DashboardView.Browser` (Referer is `/d/<uid>`) separates people from API clients, and `ExcludeUsers` takes tvt's own login (`Client.Login`). Shared helpers: `internal/logfile` (globs, `.gz`) and `internal/logfmt`.
3. Done Nov 9: SQLite store in `pkg/store` (`modernc.org/sqlite`; keep `modernc.org/libc` at the exact version its `go.mod` requires). It's a separate package so `pkg/graph` stays I/O-free. Each scan is one transaction (`BeginScan` → `Put*` → `Finish`), so an interrupted scan leaves nothing. Rows carry `last_scan`, so deleted dashboards stop protecting signals. Times are integer Unix ms. Foreign keys are deferred and checked at commit. `meta.schema_version` refuses newer or non-tvt databases.
4. Done Nov 9: `tvt scan` wired (`cmd/tvt/scan.go`). `pkg/scan.Build` is the I/O-free mapping from collector output to graph rows and owns the ID scheme (`prom:metric:<name>`, `rule:<source>:<ns>/<group>/<name>`, `panel:<org>:<uid>:<panel>:<n>`, `var:<org>:<uid>:<name>`; collisions get `#2`). Collection runs before the store is opened, so a failed scan never creates or touches the DB. Views come from `--grafana-log` (lab: `make export-logs` in tvt-lab); every evidence source is stored in `scan_sources` with its coverage, unconfigured ones at 0. Mimir 2.16 has no `/api/v1/status/tsdb` (404): `--mimir` fails with a hint to use `--prom`. References still need the PromQL extractor (Sprint 2). Golden test: `cmd/tvt/scan_test.go` against `testdata/lab` (use `grafana/grafana.log`, not `router.log`, which holds a spike-1 Legacy Queue view).

Next: Sprint 2, the PromQL extractor and references (panel/rule expressions and the query log to signals).

Run `scripts/capture-fixtures.sh` against the lab first and build against those fixtures.
