# S8: check, the safety gate

**Status:** Planned.

**Done when:** every unsafe case in ground truth is refused, with the reason in the `check` output.

## Before you start

Read `CLAUDE.md`, `docs/tasks/README.md`, `docs/architecture.md` and ADRs 0003, 0006 and 0007 first. The only writes tvt makes are Git branches and PRs in mapped pipeline repos; every backend stays read-only.

Part of the MVP (S7–S12). The MVP is done when `v0.1.0-rc1` is tagged; on the lab, `tvt propose` opens a draft PR for a seeded drop, `tvt check` passes it and fails a PR that would break any hard keep or record, the PR leaves draft only after a recorded owner confirmation, and the next `tvt scan` records the measured saving in the ledger.

- ADR 0012: collect the OTTL corpus and prototype the extractor; below 95% of statements recognized, the ADR reopens. OTTL is v1 from opentelemetry-collector-contrib v0.164.0. Decide between importing contrib's `pkg/ottl` parser (Apache 2.0; measure the dependency weight as spike 3 did for Loki) and a narrow extractor for paths and the editors that change them. Lambda functions, experimental `xottl` functions and `xprofile` are outside the guarantee.
- ADR 0011: confirm the records rule (S7 item 2).
- tvt-lab's S8 seeds (`lab-backlog.md`).

## Must have (in this order)

1. **`tvt check`** (`pkg/check`). Evaluates the config in a PR against the manifest, offline, with no credentials: apply Prometheus `metric_relabel_configs` regex semantics (and the same for `ServiceMonitor`/`PodMonitor` `metricRelabelings`), Alloy stages, OTel Collector `filter`/`transform` conditions and Fluent Bit filters to the protected signal set, instead of guessing intent. Fails closed: unknown processors, Lua/WASM and templating it can't render are failures. Fails when the evaluated drop set touches a protected signal or a record, on an unacknowledged evidence gap, on a drop of warn-and-above logs, on a component missing from the target build, on a drop with no recorded owner confirmation, and when the manifest is older than `max_age_days` (default 7). Property tests: random relabel configs; whenever the evaluated drop set overlaps the protected set, `check` must fail. A GitHub Action wraps it.
2. **Error-pattern guard on log rules.** Every generated log drop, sample or collapse rule also keeps lines matching `error|exception|traceback|critical|fatal`, in case a stream's level label is wrong. `check` fails a log rule without the guard.
3. **Least-lossy order enforced.** `check` refuses a drop when a less lossy action from ADR 0010 would keep every reader working.
4. **Replay protected queries before a label removal.** In `propose`, where tvt has backend access: run every protected query that touches the signal twice, as written and with the label removed (`sum without(label)` for counters and histograms; colliding gauges flagged), and compare series counts, label sets, empty results and values beyond a tolerance. The diff goes in the PR body; a query that goes empty or changes its grouping blocks the PR until the owner accepts it.
5. **Lookback and detection limits.** A shorter retention or downsampling tier is refused when any reader (dashboard, rule, agent) has queried further back on that signal. Each alert's worst-case detection delay (scrape interval + range window + rule-group interval + `for` + Alertmanager `group_wait`) is computed and shown in the report; a scrape interval change is refused when any rate or increase window would hold fewer than four samples, when an alert's `for` is shorter than the new interval, or when an alert would pass its severity's detection objective (set in `tvt.yaml`, for example critical 5m).
6. **Collapsed logs keep their counts.** Collapsing repeats changes every `count_over_time`, `rate` or `bytes_over_time` over those lines, so `check` refuses the collapse unless each such query is rewritten to sum the count attribute; `propose` includes the rewrite in the PR.
7. **Attributes must exist where a statement runs.** Refuse a generated filter or transform whose condition reads an attribute no earlier processor or the source provides at that point, and a statement that reads an attribute an earlier statement deleted.
8. **Model existing OTTL instead of failing closed on it** (ADR 0012). Parse the OTTL already in a pipeline so `check` knows what each existing filter drops and each transform deletes, renames or sets. Statements that use lambda or experimental functions, or functions tvt doesn't model, stay fail-closed for the signals they touch.
9. **OTTL conditions in the pipeline are readers.** A routing connector condition, a tail sampling or adaptive tail sampling OTTL policy, a filter condition or a lookup key reads the attributes and streams it names. Each becomes a declared `pipeline_rule` consumer, so dropping or renaming one of those attributes is refused like any hard-keep change.
10. **Connectors chain like recording rules.** The count, sum, signal-to-metrics and spanmetrics connectors turn spans and logs into metrics. A reader of the produced metric also reads the connector's inputs (the stream, and the attributes in its conditions and dimensions), so a log drop that would empty an alerted count metric fails `check`.
11. **Helm overlay safety.** `check` fails a values overlay that loses a drop the chart already had (an overlay list that replaces the chart's default `metricRelabelings` or `cAdvisorMetricRelabelings` without them), and a change to a monitor Prometheus doesn't select.
12. **Records and delta counters.** `check` fails any drop or sample of a record, including a filter on successful tool-call spans, and names the less lossy alternative (cap the content attributes). It allows content caps and identity deletion on records when the outcome fields stay. It refuses a label removal on delta counters when more than one Collector replica receives that data without a load-balancing gateway in front, because only one Collector may see every session.

## Should have

1. **Diagnosability after cuts** (optional suite). On a SnowOps Labs cluster (see `lab-backlog.md`), apply the proposed cuts, inject each of its faults, and confirm each is still diagnosable and graded (`labctl incident status`, `labctl scenario verify`). A fault that becomes undiagnosable becomes a ground-truth case in tvt-lab.
