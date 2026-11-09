# tvt: Telemetry Value Tracker

A Go CLI that finds telemetry (metrics, labels, log streams, span attributes) that costs money but nobody uses, and opens PRs that drop, downsample or reroute it. A `tvt check` CI gate blocks any change that would break an alert, SLO or viewed dashboard.

- Product spec: `../Spec Telemetry Value Tracker.md`
- Architecture: `docs/architecture.md`. Decisions: `docs/adr/`
- Test lab: `../tvt-lab` (Podman Grafana stack; `ground-truth.yaml` there is the expected output)

## Commands

```sh
make           # gofmt check, go vet, tests, build -> bin/tvt
make race      # tests with -race
./scripts/capture-fixtures.sh   # record real lab API responses into testdata/lab/
```

## Conventions

- Go 1.24. Module: `github.com/Opsmastery-Summit/tvt`. Single static binary (`CGO_ENABLED=0`).
- Standard library first (ADR 0004). Expected dependencies: Prometheus `promql/parser`, Loki LogQL syntax (or a vendored subset), `modernc.org/sqlite`, a YAML library, and GitHub/GitLab clients.
- Read-only against every backend. Credentials come only from env vars (`TVT_GRAFANA_TOKEN`, `TVT_PROM_TOKEN`, `TVT_LOKI_TOKEN`, `GITHUB_TOKEN`), never flags, and are never logged.
- Collectors implement the interfaces in `pkg/collect` and are tested against recorded fixtures in `testdata/`, never a live stack.
- `pkg/score`, `pkg/cost`, `pkg/graph` and `pkg/parse` stay I/O-free. Legacy Bridge will import them.
- `check` fails closed: a config it can't evaluate is a failure.
- Commits are authored as Jason Vance.

## Status (Nov 8, 2026)

Done:
- CLI skeleton.
- Scoring and cost models with tests.
- Graph model and SQLite schema.
- Collector and parser interfaces.
- CI and goreleaser.
- Lab running on Jason's Mac since Nov 8, so 30 days of history arrives around Dec 8.

Phase 0 spike findings:
- **Spike 1:** OSS Grafana logs no dashboard or panel attribution on `/api/ds/query`. Dashboard views come from Grafana router-log lines for `GET /api/dashboards/uid/:uid` (logfmt: `uname`, `t`, `path`, `handler=/api/dashboards/uid/:uid`, `status`). Panel usage comes from matching query-log expressions against panel exprs after normalizing Grafana variables.
- **Spike 2 (Prometheus):** the query log is JSON lines. Entries with `ruleGroup` are rule evaluations and must be excluded from usage evidence. Entries with `httpRequest.clientIP` are API queries, and the Grafana container's IP means dashboard or Explore traffic.
- Still open: Loki and Mimir query-log formats, the LogQL parser's dependency weight, and the price card format.

Next (Sprint 1, Dec 1-12):
1. Prometheus collector (`/api/v1/status/tsdb`, `/api/v1/rules`, query-log parser).
2. Grafana collector (search, dashboard JSON, router-log views).
3. SQLite store.

Run `scripts/capture-fixtures.sh` against the lab first and build against those fixtures.
