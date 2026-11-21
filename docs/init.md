# tvt init

`tvt init` tests each backend the way `tvt scan` will use it, then writes a starting `tvt.yaml`. Run it once, before the first scan, so a missing permission or a log that's off shows up as one clear line instead of a thin report. It only reads.

```sh
export TVT_GRAFANA_TOKEN=...      # Grafana service account, Viewer role
tvt init --grafana https://grafana.example.com --prom https://prometheus.example.com --loki https://loki.example.com
```

```
ok    grafana     answers: https://grafana.example.com
ok    grafana     token: signed in as sa-1-tvt
ok    grafana     reads dashboards
ok    grafana     reads data sources
warn  grafana     router log: can't check with a Viewer token: make sure GF_SERVER_ROUTER_LOGGING=true and pass the log as --grafana-log
ok    prometheus  answers: https://prometheus.example.com
ok    prometheus  cardinality
ok    prometheus  query log: on: pass /var/log/prometheus/query.log as --prom-query-log
ok    loki        answers: https://loki.example.com
warn  loki        query log: can't check remotely: pass Loki's own log (metrics.go lines) as --loki-query-log
ok    github      token: GITHUB_TOKEN not set: needed only for propose --output pr

wrote tvt.yaml. Next: tvt scan --config tvt.yaml --grafana https://grafana.example.com --prom https://prometheus.example.com --loki https://loki.example.com
```

A URL left out is asked for when stdin is a terminal.

## Flags

| Flag | Default | What |
| --- | --- | --- |
| `--grafana` | (asked) | Grafana base URL |
| `--prom` / `--mimir` | (asked) | The metrics backend |
| `--loki` | | Loki base URL |
| `--grafana-edition` | `oss` | `cloud` for Grafana Cloud (views come from Usage Insights) |
| `--out` | `tvt.yaml` | Where to write the config. An existing file is left alone without `--force` |
| `--force` | off | Overwrite `--out`, and write it even when a check fails |
| `--github-api` | `https://api.github.com` | GitHub API root, to check `GITHUB_TOKEN` (GitHub Enterprise: `https://HOST/api/v3`) |
| `--allow-insecure-token` | off | As for scan: send tokens over plain http to a host that isn't loopback |

Tokens come from the environment, as for scan: `TVT_GRAFANA_TOKEN`, `TVT_PROM_TOKEN`, `TVT_LOKI_TOKEN` (with `TVT_PROM_USER` and `TVT_LOKI_USER` on Grafana Cloud), and `GITHUB_TOKEN`.

## What it checks

| Backend | Check | How |
| --- | --- | --- |
| Grafana | answers | `GET /api/health` |
| | token | `GET /api/user` with `TVT_GRAFANA_TOKEN` |
| | reads dashboards | `GET /api/search` (the Viewer role) |
| | reads data sources | `GET /api/datasources` (`datasources:read`) |
| | router log | `GET /api/admin/settings` when the token can read it; a Viewer token can't, so it's a reminder. A token that can read server settings is also flagged as broader than scan needs |
| Prometheus | answers, cardinality | `GET /api/v1/status/buildinfo`, `GET /api/v1/status/tsdb` |
| | query log | `global.query_log_file` in `GET /api/v1/status/config` |
| Mimir | answers | `GET /prometheus/api/v1/status/buildinfo` |
| | cardinality analysis | `GET .../api/v1/cardinality/label_names`, which fails unless `limits.cardinality_analysis_enabled: true` |
| Loki | answers | `GET /loki/api/v1/labels` |
| GitHub | token reach | `GET /user`. A classic token with the `repo` scope reaches every repo its owner can, so it's a warning: use a fine-grained token limited to the pipeline repos. A fine-grained token's repo list isn't readable with its own permissions, so tvt reminds you to check it |

The Mimir and Loki query logs live on their hosts, so init can't check them; it names the flag to pass.

`ok` and `warn` lines still write the config. Any `fail` stops it (exit 1) unless you pass `--force`. Exit 2 is bad flags, or `--out` already exists.

## What it writes

```yaml
# Written by tvt init on 2026-11-21. Backends still come from scan's flags;
# these record what was tested. Tokens come from the environment, never this file.
# More sections (external_consumers, ownership, pipelines, records): examples/tvt.example.yaml.
grafana:
  url: "https://grafana.example.com"
metrics:
  prometheus: "https://prometheus.example.com"
logs:
  loki: "https://loki.example.com"
window_days: 30
```

Add `external_consumers` for readers tvt can't see, `ownership` so recommendations reach an owner, and `pipelines` so `tvt propose` knows where each change goes. [The example](../examples/tvt.example.yaml) has every section.
