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

- **Fluent Bit** (`kind: fluentbit`, YAML config): a stream reroute to cold storage (never a line cut, ADR 0006). For each `reroute` recommendation, tvt finds the Loki output whose static `labels` are exactly the stream's, and adds a `rewrite_tag` filter that retags that output's records (`match: shop.debug`) to `tvt.cold.shop.debug` and keeps no copy. They leave the Loki output and reach the output `pipelines[].cold` names, by `alias`, which must already exist and whose `match` must take `tvt.cold.*`. tvt doesn't invent a storage destination. It refuses rather than guess: a stream selector with anything but label equalities, a Loki output that matches by wildcard or regex or sets labels from record fields (other streams would move), no cold output, or an input that may not give every record the `log` key `rewrite_tag` tests (tvt takes only `tail` inputs without parsers). A file ending `.conf` is read and written in the classic format: the same checks, and a `[FILTER]` section (`Name rewrite_tag`, `Match`, `Rule`, `Emitter_Name`) before the first `[OUTPUT]`; `@INCLUDE` is refused (list the included files instead). Checked with `fluent-bit --dry-run` and a live 5.2.0 run that moved the tailed lines to the cold output. With `helm_values` (the fluent/fluent-bit chart), tvt checks each route against the overlay's `config.inputs`, `config.filters` and `config.outputs` joined as the chart joins them (the chart's defaults for any the overlay doesn't set), and writes the `[FILTER]` into `config.filters`. Helm replaces the string, so when the overlay doesn't set `config.filters`, tvt creates it starting with the chart's default filters from `helm_values.defaults` (`helm show values fluent/fluent-bit`), or the chart's `kubernetes` filter would go. Checked by rendering chart 0.58.3 with `helm template` and dry-running the rendered `fluent-bit.conf` in Fluent Bit 5.2.0. The values file itself is marked NOT VALIDATED, since only the chart renders it. In most Kubernetes deployments the chart's `tail` input uses `multiline.parser` and Loki labels come from record fields, which tvt refuses to reroute by tag; the writer fits static inputs and outputs. Fluent Operator resources aren't written yet. Reroutes come from the scan: an unread stream from the volume API is a stream signal of its own (see `report.md`).

- **Alloy stream reroutes** (`kind: alloy`): Alloy has no per-stream route inside `loki.process`, so tvt splits the stream off where the one `loki.process` that forwards to a `loki.write` sends it. That component's `forward_to` points at two `loki.relabel` components tvt adds: `tvt_hot` drops the stream (a rule on its exact label values) and forwards everything else to the original targets, and `tvt_cold` keeps only the stream and forwards it to the component `pipelines[].cold` names (such as `loki.write.cold`), which must exist in the file. The original `forward_to` line is kept in the block as a `tvt:original` comment, and removing the block puts it back. A rule on label values also matches a stream that carries those values plus other labels, so check `tvt why` for streams with extra labels. tvt refuses rather than guesses when more than one `loki.process` ships to Loki or `forward_to` spans lines. Checked with `alloy validate` and a live Alloy 1.10.0: the debug line reached the cold `loki.echo` and the info line didn't.

- **Prometheus Operator** (`kind: prometheus-operator`, `files` of ServiceMonitor and PodMonitor manifests): a `metricRelabelings` drop rule on every endpoint of each monitor whose targets carry the metric's jobs, appended to an existing list or added as the key. tvt works out each monitor's `job` from the manifests: a PodMonitor's is `<namespace>/<name>`; a ServiceMonitor's is, for each Service it selects in the files, the Service's label named by `spec.jobLabel`, or else the Service's name. A monitor whose job it can't tell (its Service isn't in the files, as with the apiserver's `default/kubernetes`; a selector with expressions; a PodMonitor `jobLabel`; an endpoint relabeling that sets `job`) maps to nothing until `pipelines[].monitor_jobs` names it (`apiserver: ServiceMonitor/monitoring/kps-kube-prometheus-stack-apiserver`), and a metric with such a job is skipped. There's no validator for monitor manifests yet, so these changes are marked NOT VALIDATED. With `helm_values` (`file`: your kube-prometheus-stack overlay, `path`: the chart's key in an umbrella chart's values or empty, `defaults`: the chart's own `values.yaml` at the deployed version, from `helm show values`), the drops go into the overlay instead, and `files` lists the chart's rendered output (`helm template`), read only to resolve jobs to monitors. Each monitor's rule goes into the values keys that render its endpoints' `metricRelabelings` (the kubelet's three: `metricRelabelings`, `cAdvisorMetricRelabelings`, `probesMetricRelabelings`), checked at chart 93.3.0 by rendering with a sentinel rule in every key. Helm replaces a list rather than merging it, so a key your overlay doesn't set yet is created with the chart's default rules first (the apiserver's bucket drops, the kubelet's cAdvisor drops); a key it sets gets tvt's rule appended. Without `defaults`, the pipeline is skipped. `TestHelmValuesRender` (`TVT_TEST_HELM=1`) renders the chart with an edited overlay and checks each monitor's rules, defaults included.

- **Label removals in Prometheus and Alloy**: a `label_removal` whose series stay unique without the label (`removal: labeldrop`) becomes a relabel rule that empties the label on that metric only (`source_labels: [__name__]`, `regex: <metric>`, `target_label: <label>`, `replacement: ""`, `action: replace`), in the same block as the job's drops, so the job's other metrics keep the label. Checked with Prometheus's own relabel engine: the label goes from the named metric and stays on others. A removal that merges series (`removal: aggregate`) is skipped: a relabel rule can't sum the merged series, so it needs a recording rule or a collector's ordered pipeline (see below). `job`, `instance` and `__` labels are never removed.

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

## Label removals on delta counters

A `label_removal` recommendation on an OTel Collector pipeline (S7 item 8) is written as an ordered pipeline, never a plain label drop. Removing a per-session label (`session.id`, `user.email`) from a delta counter merges every session into one series. After `delta_to_cumulative` that breaks the counter; before it, the merged deltas must be summed over a window, because sessions export separately and `delta_to_cumulative` keeps one point per stream and interval. So tvt adds, just ahead of `delta_to_cumulative` in each metrics pipeline:

1. `transform/tvt_aggregate`: `delete_matching_keys(datapoint.attributes, "^session[^A-Za-z0-9:]+id$") where <the metric's conditions>`, one statement per label, matching the OTel attribute names that translate to the Prometheus label.
2. `interval/tvt` with `interval: 60s` (the OTel SDKs' default export interval), which sums each merged stream's deltas over the window.

Checked on a live otelcol-contrib 0.165.0 with two sessions sending 5 and 3 tokens a round for three rounds. With the window, the cumulative series read 8, 16, 24 whether the sessions exported together or apart. Without it, apart, it read 5, 10, 15: one session lost each round. A pipeline with no `delta_to_cumulative` is refused (no delta counter to protect), and so is a Collector before v0.122.0 (the oldest the ordered pipeline was validated on). A custom build gets `transformprocessor` and `intervalprocessor` added to its manifest. Pseudonymising an ID (SHA256 cut to 16 hex characters) instead of deleting it waits for a recommendation that asks for it.

## OTTL by Collector version

`pipelines[].version` in `tvt.yaml` records the Collector version (S7 item 7), and picks how the filter's conditions are written. Each boundary was checked with `otelcol-contrib validate` on the real image, and both forms were run on live Collectors, which dropped exactly the matching metric:

| Version | Conditions |
|---|---|
| v0.122.0 and later | context-qualified paths: `metric.name`, `metric.unit`, `metric.is_monotonic` (v0.121.0 and v0.120.0 reject them) |
| v0.80.0 to v0.121.x | bare paths: `name`, `unit`, `is_monotonic` |
| before v0.80.0 | refused: older images have no `validate` command, so tvt has no template it has checked for them |
| not set | bare paths, which every checked version accepts (v0.80.0 through v0.165.0), and the patch says to set the version |

Every condition follows S7 item 6: stable standard functions only (`IsMatch`), no experimental or lambda functions, `error_mode: ignore`, and one path form per filter; tvt checks this before writing. tvt never writes a `batch` processor. The filter goes first in each pipeline rather than after enrich and clean processors: it matches only names as receivers emit them, so dropping before any other processor spends work on the data is both safe and cheapest. A pipeline that renames metrics in a `transform` before they're stored is a rewrite (S7 item 9), which tvt doesn't read for collectors yet.

## Custom Collector builds

A Collector built with ocb has only the components its builder manifest lists, and a config naming any other doesn't load (S7 item 4). Before tvt writes `filter/tvt`, it reads the pipeline's manifest: `pipelines[].build` in `tvt.yaml` (a path in the repo), or a `builder-config.yaml` next to one of the pipeline's files. If the manifest doesn't list `github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor`, the same patch adds it, in a marked block, at the version the manifest's other contrib components use (or its core `v0` components', or `dist.otelcol_version`). Rebuild the Collector before deploying the config. A manifest with no version to match skips the pipeline: tvt doesn't guess a module version. With no manifest, tvt assumes `otelcol-contrib`, which includes the filter processor, and the patch header says so.

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
