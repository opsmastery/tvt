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

- **OpenTelemetry Collector** (`kind: otelcol`), library only for now: `propose.Otelcol` adds a `filter/tvt` processor with one exact `metric.name == "<name>"` condition per metric and `error_mode: ignore` (a condition that fails to evaluate keeps the data), and lists it first in each metrics pipeline it drops from. In a flow-style list (`[a, b]`) tvt's entry can't carry marker lines, so a rerun takes `filter/tvt` out of the list before writing again. The command skips otelcol pipelines for now: the filter needs the names the receivers emit, and the report holds the backend's names after Prometheus translation (units, `_total`). Mapping them is S7 item 9.

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

The Prometheus writer re-reads its output and checks each job carries its rule; the Alloy writer re-parses its output. Running `promtool check config` and `alloy validate` on the result is S7 item 5.
