# tvt check

`tvt check` is the pre-merge safety gate (S8, ADR 0003). It judges a change to a pipeline repo offline, with nothing but the repo and its protection manifest (`.tvt/protect.json`, written by `tvt report --config tvt.yaml --manifest-dir DIR`). No credentials, no network, a few seconds.

```sh
# In CI, the PR's checkout against its base branch:
tvt check --base origin/main
# A patch, applied to a temporary copy of the pipeline files (the checkout is never touched):
tvt check --patch proposals/github.com_acme_pipes.patch
```

Exit 0: pass. Exit 4: refused, with one line per finding. Exit 1: an error (no manifest, a patch that doesn't apply). `--json` prints the result; `--skip-confirmation` judges safety alone, without the owner-confirmation rule; `--max-age` (default 7 days) is how old the manifest may be.

## How it judges

check reads every pipeline file the manifest names (`pipeline_defs`) twice, before and after the change, and evaluates each into effects: what a step removes that something protected needs, and what tvt can't tell. It refuses the effects the change adds. A problem already merged doesn't block an unrelated PR; a PR that edits a file tvt can't read is refused.

It evaluates configs, not intent:

| Pipeline | What it evaluates |
|---|---|
| Prometheus | each job's `metric_relabel_configs`, in Prometheus' semantics (anchored regex, `;` separator, rule order, renames), against every protected metric and label, scoped to the jobs the manifest records for it |
| prometheus-operator | `ServiceMonitor` and `PodMonitor` `metricRelabelings` |
| Alloy | `prometheus.relabel` rules (as Prometheus), `loki.relabel` drop rules and keeps in front of a `loki.write`, `loki.process` `stage.drop`, `stage.sampling` and `stage.match` with `action = "drop"`, and `otelcol.processor` samplers and filters (any of which may drop record spans) |
| OTel Collector | every pipeline's processors in order: `filter` conditions, `transform` statements, `attributes` actions, `probabilistic_sampler`, `tail_sampling`, `logdedup`, and the `routing`, `count`, `sum` and `signal_to_metrics` connectors |
| Fluent Bit (YAML) | `grep` excludes and `rewrite_tag` against the pipeline's protected streams; `lua` and `wasm` fail closed |

OTTL is read with tvt's own extractor (ADR 0012) and evaluated three-valued over what tvt knows about the item (a record span's `gen_ai.operation.name`, a metric's name in every spelling the condition uses): a condition that could be true is treated as true.

## Findings

| Rule | Refused when the change |
|---|---|
| `protected_signal` | drops, samples or strips a hard keep, keep_high or dormant metric, label or stream |
| `record` | drops or samples a record (audit streams, agent tool-call spans and their log events), or deletes an outcome field (`gen_ai.tool.name`, `gen_ai.tool.call.id`, `success`, `error.type`, `gen_ai.operation.name`). A content cap (`Substring` plus a marker) passes |
| `pipeline_reader` | deletes an attribute a routing table, filter or sampling policy reads, or empties what a counting connector counts when its metric is protected (S8 items 9, 10) |
| `attribute_unset` | adds a condition that reads an attribute a later processor sets, or one an earlier statement deleted (item 7) |
| `fail_closed` | adds a drop after a statement tvt can't model (an experimental function, a lambda, an unknown function), uses include/exclude matching, Lua or WASM, or a selector tvt can't read (item 8) |
| `warn_logs` | drops lines whose selector doesn't rule out warn-and-above |
| `scrape_interval` | changes a scrape interval so a range window holds fewer than four samples, a nonzero `for` is shorter than the interval, or an alert's worst-case detection delay passes its severity's objective (item 5) |
| `collapse_counts` | collapses repeated lines (`logdedup`), which changes every count over them (item 6) |
| `build_component` | uses a processor or connector missing from the pipeline's ocb builder manifest |
| `unreadable_config` | edits a pipeline file tvt can't read |
| `stale_manifest` | (any change) the manifest is older than `--max-age`, or predates version 2 |
| `evidence_gap` | drops anything while the scan has unacknowledged evidence gaps |
| `unconfirmed_drop` | drops anything without an owner confirmation in the manifest's `decisions` (S9 records them) |

## Detection delay (item 5)

An alert's worst-case detection delay is scrape interval + its longest range window (a recording rule's, with that rule's interval, when it reads one) + its group interval + `for` + Alertmanager's `group_wait`. `tvt.yaml` sets the objectives:

```yaml
detection:
  objectives: {critical: 15m, warning: 1h}
  group_wait: 30s   # default
```

The manifest carries every alert's timing (`alerts`), so check recomputes the delay at the changed interval offline. The longest window makes multiwindow burn-rate alerts look slow (tvt-lab's `ShopAvailabilityBurn` is 1h3m10s); since a change doesn't cause that, check doesn't refuse it.

## Ground truth

tvt-lab's `check-fixtures/*.patch` and ground truth `check:` are the test (`TestCheckLabFixtures`, copies in `testdata/check`): every unsafe fixture is refused for its item's rule, and the safe ones pass.
