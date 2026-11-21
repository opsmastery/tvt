# tvt: Telemetry Value Tracker

`tvt` finds the metrics, labels, log streams and span attributes that cost money but nobody uses. It then opens a pull request that drops, downsamples or reroutes them. It works across vendors and keeps humans in charge of every change.

- **Measure use.** Reads dashboards, alert and recording rules, and query logs to build a usage graph.
- **Price it.** Attaches ingest and storage cost to every series and stream, using your own price card.
- **Act on it.** Generates OTel Collector or Prometheus relabel config as a PR. `tvt check` blocks any change that would break an alert, SLO or viewed dashboard.

Every signal tvt marks unread had no reader in your evidence window, and every gap is listed. If you find a reader we missed, that's a bug, and the benchmark gets a new case.

> Status: release candidate (`v0.1.0-rc1`). Works on Prometheus, Mimir, Loki and Grafana (OSS and Cloud), and writes changes for Prometheus, Alloy, the OpenTelemetry Collector and Fluent Bit. See [docs/ROADMAP.md](docs/ROADMAP.md).

## Install

Download a release, verify its cosign signature, and put `tvt` on your PATH: [docs/install.md](docs/install.md). Or, with Go 1.27+, `go install github.com/Opsmastery-Summit/tvt/cmd/tvt@latest`.

## Commands

| Command | What it does | Docs |
| --- | --- | --- |
| `tvt init` | Tests each backend's access and logs, then writes `tvt.yaml`. Read-only | [init.md](docs/init.md) |
| `tvt scan` | Builds the usage graph from your backends and logs. Read-only | [scan.md](docs/scan.md) |
| `tvt report` | The ranked savings report, HTML and JSON, with every gap listed | [report.md](docs/report.md) |
| `tvt why <signal>` | Every reader of one signal, and the evidence | [why.md](docs/why.md) |
| `tvt propose` | Config changes as a patch per pipeline repo, or a draft PR | [propose.md](docs/propose.md) |
| `tvt check` | The CI gate: refuses a change that breaks an alert, SLO, viewed dashboard or record | [check.md](docs/check.md) |
| `tvt confirm` | Records an owner's confirm, dismiss or keep on the PR | [confirm.md](docs/confirm.md) |
| `tvt version` | The version, commit and build date | |

```sh
tvt init --grafana https://grafana.example.com --prom https://prometheus.example.com --loki https://loki.example.com
tvt scan --config tvt.yaml --grafana https://grafana.example.com --prom https://prometheus.example.com --loki https://loki.example.com
tvt report                                              # report.html and report.json
tvt why shop_cart_operations_total
tvt propose --config tvt.yaml --checkout github.com/acme/pipes=../pipes
tvt check --base origin/main                            # in the pipeline repo's CI
```

Credentials come from the environment (`TVT_GRAFANA_TOKEN`, `TVT_PROM_TOKEN`, `TVT_LOKI_TOKEN`, `GITHUB_TOKEN`), never from flags, and only go over https or to a loopback host. tvt never writes to a backend: its only writes are Git branches and draft PRs in the repos you map. See [SECURITY.md](SECURITY.md).

## Develop

Requires Go 1.27+.

```sh
make            # lint, test, build -> bin/tvt
make race       # tests with the race detector
```

Collector tests run against recorded API responses in `testdata/lab/` through `internal/testutil`, which fails any test that sends a write or a query. The golden tests in `cmd/tvt` check scan output against tvt-lab's `ground-truth.yaml`. CI also checks that builds are reproducible, runs `goreleaser check`, govulncheck and gitleaks, and Dependabot proposes updates weekly.

To scan Mimir instead, start the lab with `make up-mimir` and pass `--mimir http://localhost:9009` in place of `--prom`.

### Releases

A `v*` tag builds reproducible archives for Linux, macOS and Windows (amd64 and arm64) with goreleaser and signs `checksums.txt` with cosign (keyless). Tags with a suffix such as `v0.1.0-rc1` publish a pre-release. The checklist is [docs/release.md](docs/release.md).

Run it against [tvt-lab](../tvt-lab), a Podman Grafana stack seeded with known waste and known dependencies:

```sh
(cd ../tvt-lab && make export-logs)   # copies query.log and grafana.log* to ../tvt-lab/lab-logs
./bin/tvt scan --grafana http://localhost:3000 --prom http://localhost:9090 --loki http://localhost:3100 \
  --prom-query-log ../tvt-lab/lab-logs/query.log --grafana-log '../tvt-lab/lab-logs/grafana.log*'
```

Tokens (`TVT_GRAFANA_TOKEN`, `TVT_PROM_TOKEN`, `TVT_LOKI_TOKEN`) are only sent over https, or over http to a loopback host such as the lab. For any other plain-http URL, `tvt scan` exits with a usage error unless you pass `--allow-insecure-token`.

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
