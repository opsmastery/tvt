# tvt: Telemetry Value Tracker

`tvt` finds the metrics, labels, log streams and span attributes that cost money but nobody uses. It then opens a pull request that drops, downsamples or reroutes them. It works across vendors and keeps humans in charge of every change.

- **Measure use.** Reads dashboards, alert and recording rules, and query logs to build a usage graph.
- **Price it.** Attaches ingest and storage cost to every series and stream, using your own price card.
- **Act on it.** Generates OTel Collector or Prometheus relabel config as a PR. `tvt check` blocks any change that would break an alert, SLO or viewed dashboard.

> Status: pre-alpha (Sprint 1). `tvt scan` collects Prometheus or Mimir cardinality and rules, Loki rules, Grafana dashboards and dashboard views into a SQLite usage graph. Query matching (Sprint 2), Loki stream volume (Sprint 3), report, propose and check are not built yet. See [docs/architecture.md](docs/architecture.md).

## Commands

```
tvt scan --grafana <url> --prom <url> --loki <url>   # build the usage graph (read-only)
tvt report                                            # ranked savings report, HTML + JSON
tvt propose                                           # config change + pull request
tvt check                                             # CI gate on config PRs
tvt version
```

Credentials come from the environment (`TVT_GRAFANA_TOKEN`, `TVT_PROM_TOKEN`, `TVT_LOKI_TOKEN`, `GITHUB_TOKEN`) and are never passed as flags.

### tvt scan

```sh
export TVT_GRAFANA_TOKEN=glsa_...   # Grafana service account, Viewer role (plus datasources:read)
tvt scan --grafana https://grafana.example.com --prom https://prometheus.example.com \
  --loki https://loki.example.com \
  --grafana-log '/var/lib/grafana/logs/grafana.log*' \
  --prom-query-log /prometheus/query.log
```

| Flag | Default | What |
| --- | --- | --- |
| `--grafana` | (required) | Grafana base URL |
| `--prom` / `--mimir` | (one required) | Metrics backend. `--mimir` reads Mimir's cardinality API, which needs `limits.cardinality_analysis_enabled: true`; `/prometheus` is appended if missing |
| `--loki` | | Loki base URL. Its alert and recording rules become consumers and protect the streams they select. Stream volume comes in Sprint 3 |
| `--grafana-log` | | Grafana log with `GF_SERVER_ROUTER_LOGGING=true`; repeatable, globs and `.gz` allowed. Without it no dashboard counts as viewed |
| `--prom-query-log` | | Prometheus query log; repeatable. Recorded for coverage now, matched against signals from Sprint 2 |
| `--label-top` | 50 | Per-label cardinality for this many of the largest metrics |
| `--window` | 30 | Observation window in days |
| `--db` | `tvt.db` | Usage graph database; each run adds one scan |

Each run is one transaction: a failed or interrupted scan writes nothing. Exit codes: 0 success, 1 scan failed, 2 bad flags. tvt's own dashboard fetches are excluded from views by its login.

## Develop

Requires Go 1.26+.

```sh
make            # lint, test, build -> bin/tvt
make race       # tests with the race detector
```

Collector tests run against recorded API responses in `testdata/lab/` through `internal/testutil`, which fails any test that sends a write or a query. The golden tests in `cmd/tvt` check scan output against tvt-lab's `ground-truth.yaml`. CI also runs `goreleaser check`, govulncheck and gitleaks.

To scan Mimir instead, start the lab with `make up-mimir` and pass `--mimir http://localhost:9009` in place of `--prom`.

### Releases

A `v*` tag builds archives for Linux, macOS and Windows (amd64 and arm64) with goreleaser and signs `checksums.txt` with cosign (keyless). Tags with a suffix such as `v0.0.1-rc.1` publish a pre-release. The verify command is in `.goreleaser.yaml`.

Run it against [tvt-lab](../tvt-lab), a Podman Grafana stack seeded with known waste and known dependencies:

```sh
(cd ../tvt-lab && make export-logs)   # copies query.log and grafana.log* to ../tvt-lab/lab-logs
./bin/tvt scan --grafana http://localhost:3000 --prom http://localhost:9090 --loki http://localhost:3100 \
  --prom-query-log ../tvt-lab/lab-logs/query.log --grafana-log '../tvt-lab/lab-logs/grafana.log*'
```

## Layout

| Path | What |
| --- | --- |
| `cmd/tvt` | CLI entry point |
| `pkg/collect` | Read-only backend interfaces; one subpackage per backend |
| `pkg/parse` | PromQL / LogQL reference extraction |
| `pkg/graph` | Usage graph model and SQLite schema |
| `pkg/scan` | Collector output to graph rows (I/O-free) |
| `pkg/store` | SQLite usage graph store |
| `pkg/cost` | Price card and cost model |
| `pkg/score` | Value scoring, shared with Legacy Bridge |
| `pkg/propose` | Config generation |
| `pkg/check` | Pre-merge safety gate |
| `pkg/report` | JSON and HTML report |
| `pkg/vcs` | GitHub / GitLab PRs |
| `docs/adr` | Architecture decisions |

## License

Apache 2.0
