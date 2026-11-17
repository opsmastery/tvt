# tvt propose

`tvt propose` turns the latest scan's recommendations into config changes for the pipelines in `tvt.yaml` (S7 item 3, ADR 0007). Today it writes whole-metric drops into Prometheus scrape configs and Alloy configs, as one patch per pipeline repo. It reads local checkouts and never pushes, so it needs no Git token.

```
tvt propose --db tvt.db --config tvt.yaml \
  --checkout github.com/acme/observability=../observability \
  --dir proposals
git -C ../observability apply ../tvt/proposals/github.com_acme_observability.patch
```

## Flags

| Flag | Meaning |
|---|---|
| `--db` | The scan database (default `tvt.db`). |
| `--config` | `tvt.yaml`, required: its `pipelines:` say where each drop goes. |
| `--checkout repo=path` | A pipeline repo's local checkout, with the repo named as in `tvt.yaml`. Repeat per repo. A pipeline with no checkout is skipped. |
| `--report` | The reviewed report JSON. It must be of the database's latest scan; without it, tvt builds the report from the database (all recommendations, no `--top` cut). |
| `--output` | `patch`, the only output so far. Pull requests come with S9's approvals. |
| `--dir` | Where patches go (default `proposals`), as `<repo>.patch` with the repo path made filename-safe as in `report.md`'s manifests. |

## What it writes

Only recommendations that are `proposable` (mapped to exactly one pipeline), `drop` actions, and of whole metrics. Each metric's drop goes into the scrape configs of every job it carries: the scan stores the `job` label's values per metric (schema 24). If any of those jobs has no scrape config in the pipeline's `files`, the metric is skipped, because a drop in some jobs would leave the rest of its series and cost in place.

- **Prometheus** (`kind: prometheus`): one `metric_relabel_configs` drop rule per scrape job, naming the metrics that job carries. The block ID is `<pipeline>/<job>`.
- **Alloy** (`kind: alloy`): one `rule` block in each `prometheus.relabel` component the jobs' `prometheus.scrape` components forward to, naming every metric for the pipeline. The block ID is the pipeline's name.

- **OpenTelemetry Collector** (`kind: otelcol`): a `filter/tvt` processor in each file that has a metrics pipeline, listed first in every `metrics` and `metrics/<name>` pipeline, with `error_mode: ignore` (a condition that fails to evaluate keeps the data). The block ID is the pipeline's name. In a flow-style list (`[a, b]`) tvt's entry can't carry marker lines, so a rerun takes `filter/tvt` out of the list before writing again.

  The report has the names Prometheus stores; the collector sees OTel names. Prometheus's OTLP translation splits the OTel name on anything but letters, digits and colons, appends the unit's words (`USD`, `seconds`, `bytes_per_second`, `ratio` for a gauge with unit `1`) and appends `total` to a monotonic counter. So for each stored name the filter matches every OTel name that translates to it: the exact name (a server keeping UTF-8 names), plus `IsMatch` on the tokens with conditions on `metric.unit` and `metric.is_monotonic`. For `coding_agent_cost_usage_USD_total` that matches the monotonic counter `coding_agent.cost.usage` with unit `USD`, and not a gauge or another unit. A name ending `_bucket`, `_sum`, `_count` or `_created` is skipped: it may be one series of an OTel histogram, and the collector can only drop the whole histogram. A case the translation model misses (a unit word already in the OTel name) matches nothing, so its drop is a no-op `tvt check` shows, never a wrong drop.

Each rule sits in a marked block (S7 item 10):

```
# tvt:begin prometheus-scrape/node scan=12 evidence=3f9c0a1b2d4e
# 3 unread metrics, see tvt why; saves 41.20 USD/month
- source_labels: [__name__]
  regex: "node_a|node_b|node_c"
  action: drop
# tvt:end
```

`evidence` is a short hash of the scan and the recommendations' evidence; S9 binds approvals to it. Every existing tvt block in a file is removed before the new ones go in, so a rerun replaces the last proposal instead of stacking on it, and every other byte of the file is kept. The patch starts with the recommendations it carries and their claims; `git apply` skips that text.

## What it skips, and says so

Every other recommendation is printed as skipped with the reason: unmapped or claimed by several pipelines, a recording rule's output, an action or signal kind with no writer yet (label removals, stream reroutes), a pipeline kind with no writer yet (OTel Collector, Fluent Bit, exporters), Helm-rendered pipelines, a missing checkout, or a scan that didn't read the job label.

## Validation

A config that doesn't load never goes into a patch (S7 item 5). For each changed file, tvt copies the pipeline's checkout (without `.git`) to a scratch directory under your cache directory, writes its changes in, and runs the pipeline's own validator there, so references to other files resolve:

| Kind | Validator | Image when it isn't on PATH |
|---|---|---|
| `prometheus` | `promtool check config <file>` | `prom/prometheus:v3.5.0` |
| `alloy` | `alloy validate <file>` | `grafana/alloy:v1.10.0` |
| `otelcol` | `otelcol-contrib validate --config=<file>` | `otel/opentelemetry-collector-contrib:0.165.0` |
| `fluentbit` | `fluent-bit --dry-run -c <file>` | `fluent/fluent-bit:5.2.0` |

The binary on PATH wins; otherwise the image runs through podman or docker (`--container-runtime`), with no network and the copy mounted read-only. Validate with your own distribution and version with `--validator-image otelcol=ghcr.io/acme/otelcol:1.2.3`. The images are the versions the tvt lab runs.

- A file the validator rejects is left out of the patch, and its recommendations are listed as skipped with the validator's first lines.
- With `--validate auto` (the default), a file no validator could check is still written, and the patch header and the summary mark it `NOT VALIDATED` with the reason. `--validate require` skips it instead; `--validate off` skips validation.

The writers also check their own output first: the Prometheus writer re-reads it, the Alloy writer re-parses it, and the collector writer confirms the filter is first in each pipeline.
