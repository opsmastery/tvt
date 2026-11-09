# tvt: Telemetry Value Tracker

`tvt` finds the metrics, labels, log streams and span attributes that cost money but nobody uses. It then opens a pull request that drops, downsamples or reroutes them. It works across vendors and keeps humans in charge of every change.

- **Measure use.** Reads dashboards, alert and recording rules, and query logs to build a usage graph.
- **Price it.** Attaches ingest and storage cost to every series and stream, using your own price card.
- **Act on it.** Generates OTel Collector or Prometheus relabel config as a PR. `tvt check` blocks any change that would break an alert, SLO or viewed dashboard.

> Status: pre-alpha scaffold. Command flags are wired up, and the scoring and cost models are implemented and tested. The collectors, report, propose and check are not built yet. See [docs/architecture.md](docs/architecture.md).

## Commands

```
tvt scan --grafana <url> --prom <url> --loki <url>   # build the usage graph (read-only)
tvt report                                            # ranked savings report, HTML + JSON
tvt propose                                           # config change + pull request
tvt check                                             # CI gate on config PRs
tvt version
```

Credentials come from the environment (`TVT_GRAFANA_TOKEN`, `TVT_PROM_TOKEN`, `TVT_LOKI_TOKEN`, `GITHUB_TOKEN`) and are never passed as flags.

## Develop

Requires Go 1.24+.

```sh
make            # lint, test, build -> bin/tvt
make race       # tests with the race detector
```

Run it against [tvt-lab](../tvt-lab), a Podman Grafana stack seeded with known waste and known dependencies:

```sh
./bin/tvt scan --grafana http://localhost:3000 --prom http://localhost:9090 --loki http://localhost:3100
```

## Layout

| Path | What |
| --- | --- |
| `cmd/tvt` | CLI entry point |
| `pkg/collect` | Read-only backend interfaces; one subpackage per backend |
| `pkg/parse` | PromQL / LogQL reference extraction |
| `pkg/graph` | Usage graph model and SQLite schema |
| `pkg/cost` | Price card and cost model |
| `pkg/score` | Value scoring, shared with Legacy Bridge |
| `pkg/propose` | Config generation |
| `pkg/check` | Pre-merge safety gate |
| `pkg/report` | JSON and HTML report |
| `pkg/vcs` | GitHub / GitLab PRs |
| `docs/adr` | Architecture decisions |

## License

Apache 2.0
