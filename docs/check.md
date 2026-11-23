# tvt check

`tvt check` is the pre-merge safety gate (S8, ADR 0003). It judges a change to a pipeline repo offline, with nothing but the repo and its protection manifest (`.tvt/protect.json`, written by `tvt report --config tvt.yaml --manifest-dir DIR`). No credentials, no network, a few seconds.

```sh
# In CI, the PR's checkout against its base branch:
tvt check --base origin/main
# A patch, applied to a temporary copy of the pipeline files (the checkout is never touched):
tvt check --patch proposals/github.com_acme_pipes.patch
```

Exit 0: pass. Exit 4: refused, with one line per finding. Exit 1: an error (no manifest, a patch that doesn't apply). `--json` prints the result; `--skip-confirmation` judges safety alone, without the owner-confirmation rule; `--max-age` (default 7 days) is how old the manifest may be. `--repo` is the pipeline repo's checkout (default the current directory) and `--manifest` the manifest's path inside it (default `.tvt/protect.json`).

## How it judges

check reads every pipeline file the manifest names (`pipeline_defs`) twice, before and after the change, and evaluates each into effects: what a step removes that something protected needs, and what tvt can't tell. It refuses the effects the change adds. A problem already merged doesn't block an unrelated PR; a PR that edits a file tvt can't read is refused.

The manifest is committed in the repo the change comes from, so with `--base` check also judges the change against the manifest the base commits. A PR that removes a protected signal or a pipeline from `protect.json` while dropping it is refused, with "protected in the manifest before this change" on the finding. `--patch` does the same when the patch changes `protect.json`: the patched manifest is the one the change brings, and the working tree's is the one before it. That second pass is safety only: owner confirmations, evidence gaps and the manifest's age come from the manifest the change brings. A manifest whose pipeline paths leave the repo (absolute, or with `..`) is an error.

A change that deletes or moves a pipeline file the manifest names is refused: nothing in the file would be judged. Update the pipeline's `files` in `tvt.yaml` and rerun `tvt report --manifest-dir` in the same PR; check then reads the file at its old path before the change and its new path after, so only what the move adds is new.

It evaluates configs, not intent:

| Pipeline | What it evaluates |
|---|---|
| Prometheus | each job's `metric_relabel_configs`, in Prometheus' semantics (anchored regex, `;` separator, rule order, renames), against every protected metric and label, scoped to the jobs the manifest records for it |
| prometheus-operator | `ServiceMonitor` and `PodMonitor` `metricRelabelings` |
| Alloy | `prometheus.relabel` rules (as Prometheus), `prometheus.remote_write` endpoints' `write_relabel_config` (an endpoint to the retention tier's tenant may keep dormant metrics the others drop), `loki.relabel` drop rules and keeps in front of a `loki.write`, `loki.process` `stage.drop`, `stage.sampling` and `stage.match` with `action = "drop"`, and `otelcol.processor` samplers and filters (any of which may drop record spans) |
| OTel Collector | every pipeline's processors in order: `filter` conditions, `transform` statements, `attributes` actions, `probabilistic_sampler`, `tail_sampling`, `log_dedup` (or its deprecated name `logdedup`, with or without settings), and the `routing`, `count`, `sum` and `signal_to_metrics` connectors. `batch`, `memory_limiter`, `k8sattributes`, `resourcedetection`, `interval`, `delta_to_cumulative` and `cumulativetodelta` pass through; any other processor type fails closed |
| Fluent Bit (YAML) | `grep` excludes and `rewrite_tag` against the pipeline's protected streams; `lua` and `wasm` fail closed |

OTTL is read with tvt's own extractor (ADR 0012) and evaluated three-valued over what tvt knows about the item (a record span's `gen_ai.operation.name`, a metric's name in every spelling the condition uses): a condition that could be true is treated as true.

## Findings

| Rule | Refused when the change |
|---|---|
| `protected_signal` | drops, samples or strips a hard keep, keep_high or dormant metric, label or stream |
| `record` | drops or samples a record (audit streams, agent tool-call spans and their log events), or deletes an outcome field (`gen_ai.tool.name`, `gen_ai.tool.call.id`, `success`, `error.type`, `gen_ai.operation.name`). A content cap (`Substring` plus a marker) passes |
| `pipeline_reader` | deletes an attribute a routing table, filter or sampling policy reads, or empties what a counting connector counts when its metric is protected (S8 items 9, 10) |
| `attribute_unset` | adds a condition that reads an attribute a later processor sets, or one an earlier statement deleted (item 7) |
| `fail_closed` | adds a drop after a statement tvt can't model (an experimental function, a lambda, an unknown function), uses include/exclude matching, Lua or WASM, a Collector processor type tvt doesn't know, or a selector tvt can't read (item 8) |
| `protected_signal` (remote_write) | check reads a Prometheus pipeline's `remote_write` too (S10 item 2): a `write_relabel_configs` drop takes metrics out of the backend just as a scrape-time drop does. A `remote_write` whose `X-Scope-OrgID` is the manifest's `dormant_tenant` narrows the retention tier's copy, and a dormant metric it keeps may leave the other `remote_write`s; anything else dropped there is judged as dropped |
| `warn_logs` | drops lines whose selector doesn't rule out warn-and-above; for a Collector `filter` on logs, a condition that can be true at severity number 13, 17 or 21 (warn, error, fatal) |
| `scrape_interval` | changes a scrape interval so a range window holds fewer than four samples, a nonzero `for` is shorter than the interval, or an alert's worst-case detection delay passes its severity's objective (item 5); or raises a job's interval past the manifest's `scrape_limits` (S10 item 5: the longest every reader of the job allows, panels and instant readers included, or the current interval when a reader's window is unknown) |
| `error_guard` | adds a log drop, sample or collapse that doesn't keep lines matching `(?i)error\|exception\|traceback\|critical\|fatal`, in case a stream's level label is wrong (item 2) |
| `least_lossy` | drops a metric or stream that something reads ad hoc (a query-log reader other than a dashboard or rule) when a less lossy action from ADR 0010 would keep that reader working: a cold tier, a reroute, a label removal (item 3). A Collector `filter` on logs is judged against the pipeline's read and protected streams (equality matchers as resource attributes): it passes for a stream only when it keeps warn and above and every reader of the stream keeps to warn and above too (a level matcher tvt reads: `warn_plus` in the manifest), the level tier of S10 item 3; a record stream never passes. The same holds for an Alloy `stage.match` drop with a `|~` level line filter and a Fluent Bit `rewrite_tag` rule with a level lookahead: check judges the filter on sample lines (it must match a debug or info line and no warn-and-above or unlevelled one), and readers that keep to warn and above then lose nothing. With `pipelines[].cold` set, the filtered lines must reach it: some logs pipeline with one of the same receivers exports only to the cold exporter. Such a pipeline is the cold copy, and what it filters is never judged, since it drops nothing from the hot tier |
| `collapse_counts` | collapses repeated lines (`log_dedup`/`logdedup`) whose streams something reads, which changes every count over them (item 6). With `conditions`, only logs one of them can match collapse: each read stream (its equality matchers as resource attributes, `service_name` as `service.name`) and each record match is judged against them, three-valued, so a condition check can't decide refuses; with none, any read stream or record event in the pipeline refuses it |
| `helm_overlay` | sets a values list (`metricRelabelings`, `cAdvisorMetricRelabelings`) that replaces the chart's default without the drops the default had, or changes a monitor Prometheus doesn't select (item 11) |
| `delta_replicas` | removes a label from a delta counter in a pipeline the manifest records as more than one replica with no load-balancing gateway (`pipelines[].replicas`, `load_balanced` in `tvt.yaml`) (item 12) |
| `build_component` | uses a processor or connector missing from the pipeline's ocb builder manifest |
| `unreadable_config` | edits a pipeline file tvt can't read |
| `stale_manifest` | (any change) the manifest is older than `--max-age`, or predates version 2 |
| `evidence_gap` | drops anything while the scan has unacknowledged evidence gaps |
| `unconfirmed_drop` | drops anything without an owner confirmation: in a tvt PR, each recommendation `.tvt/proposal.json` lists for the pipeline needs a `confirm` in `.tvt/decisions.yaml` (S9); a drop tvt didn't write needs a `confirm` naming its block or signal |
| `stale_confirmation` | drops something whose owner confirmed an evidence hash the manifest no longer has: a reader appeared or a gap reopened since they looked, or the scan no longer recommends it (S9 item 2). The finding names the current readers and gaps |

## Owner confirmations (S9)

A tvt PR opens as a draft and carries `.tvt/proposal.json`: per pipeline, each recommendation it writes, its owner and its evidence hash (the recommendation's `evidence_hash`: its readers and open gaps, without query times or cost, so a rescan that finds the same readers keeps it). The owner reviews the evidence and records the decision on the PR's branch:

```sh
tvt confirm --report r.json --dir . --owner group:default/shop-team drop:prom:metric:shop_legacy_queue_depth
tvt confirm --report r.json --dir . --owner @jasonvance --all     # everything in proposal.json they own
git add .tvt/decisions.yaml && git commit -m "tvt: confirm" && git push
```

check then requires, for each listed recommendation, a `confirm` whose `evidence` equals the hash in the manifest's `evidence` (refreshed by every `tvt report --manifest-dir`). Until then the drop is `unconfirmed_drop`; once the evidence moves, it's `stale_confirmation` and the owner looks again. `--dismiss REASON` and `--keep REASON` record the other decisions, with a `--review` date (default 90 days).

## Detection delay (item 5)

An alert's worst-case detection delay is scrape interval + its longest range window (a recording rule's, with that rule's interval, when it reads one) + its group interval + `for` + Alertmanager's `group_wait`. `tvt.yaml` sets the objectives:

```yaml
detection:
  objectives: {critical: 15m, warning: 1h}
  group_wait: 30s   # default
```

The manifest carries every alert's timing (`alerts`), so check recomputes the delay at the changed interval offline. The longest window makes multiwindow burn-rate alerts look slow (tvt-lab's `ShopAvailabilityBurn` is 1h3m10s); since a change doesn't cause that, check doesn't refuse it.

## In GitHub Actions

The repo is a composite action. It builds tvt at the action's ref and runs `check --base origin/<base branch>`; the job fails on exit 4 and the findings appear in the log.

```yaml
on: pull_request
jobs:
  tvt-check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 0}
      - uses: opsmastery/tvt@main
```

## Property tests

`TestPropertyRelabel` generates 2,000 random relabel configs against a random protected set and runs each through Prometheus' own relabel engine as the oracle. Whenever the engine drops a protected signal, check must refuse; the test asserts the two counts are equal.

## Ground truth

tvt-lab's `check-fixtures/*.patch` and ground truth `check:` are the test (`TestCheckLabFixtures`, copies in `testdata/check`): every unsafe fixture is refused for its item's rule, and the safe ones pass.
