# tvt: Telemetry Value Tracker

A Go CLI that finds telemetry (metrics, labels, log streams, span attributes) that costs money but nobody uses, and opens PRs that drop, downsample or reroute it. A `tvt check` CI gate blocks any change that would break an alert, SLO or viewed dashboard.

- Product spec: `../Spec Telemetry Value Tracker.md`
- Roadmap: `docs/ROADMAP.md`. Sprint task lists: `docs/tasks/`
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

## Status (Nov 11, 2026)

Done:
- CLI skeleton.
- Scoring and cost models with tests.
- Graph model and SQLite schema.
- Collector and parser interfaces.
- CI (lint, race, build, `goreleaser check`, govulncheck, gitleaks) and a tag-triggered release workflow (goreleaser, cosign keyless signing of `checksums.txt`; `-rc.N` tags are pre-releases). `go.mod` pins `toolchain go1.26.11` for govulncheck.
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
4. Done Nov 9: `tvt scan` wired (`cmd/tvt/scan.go`). `pkg/scan.Build` is the I/O-free mapping from collector output to graph rows and owns the ID scheme (`prom:metric:<name>`, `rule:<source>:<ns>/<group>/<name>`, `panel:<org>:<uid>:<panel>:<n>`, `var:<org>:<uid>:<name>`; collisions get `#2`). Collection runs before the store is opened, so a failed scan never creates or touches the DB. Views come from `--grafana-log` (lab: `make export-logs` in tvt-lab); every evidence source is stored in `scan_sources` with its coverage, unconfigured ones at 0. Mimir 2.16 has no `/api/v1/status/tsdb` (404); see item 6. References still need the PromQL extractor (Sprint 2). Golden test: `cmd/tvt/scan_test.go` against `testdata/lab` (use `grafana/grafana.log`, not `router.log`, which holds a spike-1 Legacy Queue view).
5. Done Nov 10: Loki rules collector in `pkg/collect/loki` (YAML from `/loki/api/v1/rules` via `go.yaml.in/yaml/v3`; 404 `no rule groups found` is zero rules; every request sends `X-Query-Tags: Source=tvt`). `tvt scan --loki` adds them as consumers. Until the LogQL extractor exists, `parse.LogStreamSelectors` (a stopgap) pulls stream selectors out of each rule, and `scan.Build` writes a `loki:stream:<canonical selector>` signal with an **assumed** reference (`refs.assumed`, schema v2: covers every label of the stream). An unreadable selector references `loki:stream:*` (every stream).
6. Done Nov 10: Mimir cardinality in `pkg/collect/mimir` (`/prometheus/api/v1/cardinality/label_values` on `__name__`, `label_names` plus `label_values?limit=0` per metric). Mimir caps `limit` at 500, so metric names are partitioned by prefix (`{__name__=~"p.*"}`) until each partition is complete; an uncompletable one is `ErrTruncated`. Analysis disabled → `ErrDisabled`, naming `limits.cardinality_analysis_enabled`. `tvt scan --mimir` uses it, with rules from `prometheus.Client` (`Source: "mimir"`). The lab's `mimir-rules` service loads rules into Mimir's ruler; fixtures in `testdata/lab/mimir/` (`SECTIONS=mimir ./scripts/capture-fixtures.sh`). `internal/testutil.MimirNames` fakes the partitioned API from the capture.
7. Done Nov 10: testing groundwork (PR #9). `internal/testutil` is the only fixture server: any non-GET request or query/proxy endpoint fails the test, enforcing read-only. `cmd/tvt/groundtruth_test.go` asserts the vendored `testdata/lab/ground-truth.yaml` (metrics, rules per source, dashboard viewed state, hard-keep streams, `mimir:`). Fuzz targets: `internal/logfmt`, the Prometheus query-log and Grafana router-log line and file parsers, `parse.LogStreamSelectors`; garbage must give an error, never an empty "unused" result.

Sprint 1 is closed: PRs #1-#10 merged, CI green, and `v0.0.1-rc.1` released (6 archives, `checksums.txt` verified with `cosign verify-blob`).

Sprint 2 (`docs/tasks/2026-11-sprint2.md`):
1. Done Nov 11: PromQL extractor in `pkg/parse/promql.go` (`parse.PromQL`, upstream parser with experimental functions on). One `Ref` per selector with matchers, `Grouping` (by, on, group_left/right includes, label_replace/label_join sources, sort_by_label, `le` under histogram_quantile, `job`/`instance` for `info()`, which also adds a `target_info` ref or the info metric its selector names), `Without` (without, ignoring), `Created`, `Subquery`, `AllLabels` (vector match without `on()`, variable label names, parse failures). An aggregation that keeps only `by()` labels stops outer needs from reaching below it; `without` and topk/bottomk/limitk pass them through. Grafana variables are substituted first: built-in time variables and ranges/offsets become `5m`; a variable in a metric or label name makes the Ref `Assumed` with a reason; one in a matcher value only sets `Matcher.Variable`. A parse failure (or invalid UTF-8) returns the error plus an assumed `AllLabels` Ref for every name that could be a metric, including keyword-like names and the `__name__` inside a bare `{...}` (no name there → an any-metric wildcard). Variable fragments in metric names become `.*` (they can be empty). Corpus test over kube-prometheus rules and dashboards plus the lab (`pkg/parse/testdata/promql-corpus`, baseline 1.0); `FuzzPromQL`.

Next: item 2, the LogQL extractor.

Run `scripts/capture-fixtures.sh` against the lab first and build against those fixtures.
