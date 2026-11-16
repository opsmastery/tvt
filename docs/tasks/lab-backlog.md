# tvt-lab backlog

Seeds the lab needs, grouped by the sprint whose tests use them. Land each one in tvt-lab, with its expected result in `ground-truth.yaml`, a sprint ahead of the code that reads it. Then re-run `scripts/capture-fixtures.sh` and vendor the new ground truth into `testdata/lab/`.

## Sprint 2 (now)

- Done Nov 12: a Sloth SLO over a metric nothing else reads, and an alert whose runbook has a query step (item 7).
- Done Nov 12: an `absent()` alert on a shop metric: hard keep, and never a broken rule (item 18).
- Done Nov 12, in tvt's Grafana fixtures rather than the lab (no Pyroscope there): a panel on a Pyroscope (or other unparsed) data source is an assumed reader and a gap, never an unused dashboard (item 19).
- Decide the 21 seeded drops the dormant name rules hold back (node `*_errs_total`, `shop_cache_*` latency, error, eviction and lag gauges, the latency histograms' `_sum`/`_count`): narrow the rules, or mark them "dormant: confirm" in ground truth. Needed before S4.

## S3

- A simulated agent client that sweeps every metric name: ground truth expects it to keep nothing.
- A simulated agent client that queries one shop metric over a 90-day range: it counts as a read, and in S8 it blocks a 30-day retention proposal on that metric.
- `retention.size` on the lab Prometheus, remote write to Mimir while keeping 15 days locally, and one job scraped at 15s whose metrics only a 1m-step dashboard reads. Record the measured bytes per sample.

## S5

- Distractors: a metric with a near-identical name to a read one, a service that logs background errors all day, and a dashboard that reads a metric through a variable.
- Two frozen lab states, before and after a deploy that adds a reader (also used by S15's `--as-of`).
- The shop logs one identical line in a burst at startup: a repeated-line finding.

## S6

- A service with no SLO, and a paging alert that isn't a burn-rate alert: SLO coverage findings.
- The shop's traces through an OTel Collector with a spanmetrics connector and a low `aggregation_cardinality_limit` that overflows: an overflow gap.

## S7

- A pipeline repo (or directory) for the lab's Alloy config, and a Fluent Bit container that tails the shop's debug log and ships it to Loki. Record which pipeline owns each signal.
- An audit stream, and a span with `gen_ai.operation.name=execute_tool` for a simulated agent rollback that succeeds: both are records.
- A Collector pipeline with a `batch` processor (S7 never writes one; S16 flags it).
- A synthetic coding-agent emitter on the GenAI conventions (interaction, llm_request and tool spans; tool_result, tool_decision and api_request logs; token and cost delta counters carrying `session.id` and `user.email`), every content flag on. Ground truth: content attributes unread (cap), `session.id` on delta counters (aggregate in the ordered pipeline), successful tool spans are records (keep the span), `api_request` logs unread (drop). Synthetic only: no real agent or prompts.

## S8

- A Collector filter that reads an attribute a later processor sets (`check` refuses it), an uncast status-code comparison and a regex statement with no `where` (S16 flags both).
- The expected worst-case detection delay for each lab alert, and a scrape interval change that pushes one past its objective.
- A routing connector that routes on a span attribute, a count connector that turns shop error logs into a metric an alert reads, and a statement using an experimental OTTL function: the routing attribute and the counted stream are protected, and S16 reports the upgrade risk.

## S9

- A `catalog-info.yaml` for the shop with an owner, expected in the recommendation's owner field.

## S10

- vmagent with stream aggregation in front of one shop metric.
- A status-code label read only through `=~"5.."`: a bucketing proposal, not a drop.

## S13–S14

- A flapping alert with no `for`, an alert that fires inside a silence window, two alerts that always co-fire, an alert whose runbook file hasn't changed in over 90 days, and a receiver that fails.
- A wildcard alert and a resource-specific alert on the same shop metric, and an alert whose threshold sits inside the metric's normal range.

## S15

- A lab "release" that renames a shop metric a dashboard reads: a readers-broken finding naming the panel and the release.
- The synthetic coding-agent emitter (S7) also calls a telemetry query tool: an `execute_tool` span and a `tool_result` log whose `tool_parameters` hold a PromQL query on a shop metric. Ground truth: that metric is read by client `agent`, and a reduction rule deleting `tool_parameters` from both fails `check`.

## S16

- Shop spans named with raw product paths, the spanmetrics histogram on default buckets, a volatile resource attribute in the metric identity, and a debug exporter left on.

## kube-prometheus-stack fixtures (from S5)

[kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) is how most teams run Prometheus on Kubernetes. Render it with `helm template` at a pinned chart version (93.3.0 when this was written) so CI can test readers and writers without a cluster. SnowOps Labs below is the live run.

- A `make kps-fixtures` target that renders the pinned chart into `testdata/kube-prometheus-stack/` (rules, ServiceMonitors, PodMonitors, dashboards, the default values) plus a hand-checked golden list of declared readers. Bump the pin only on purpose, with the golden diff reviewed in the PR.
- S5 (done Nov 15): monitor selection (`serviceMonitorSelectorNilUsesHelmValues`), including a monitor without the release label that must not count.
- S6: the chart's 41 rule files and dashboards as declared evidence; its default kubelet drops as ground truth (dropped series have no consumers, `container_memory_swap` protected).
- S7 and S8: an overlay that sets `cAdvisorMetricRelabelings` with only tvt's rules must fail `check`; the writer's overlay must keep the chart's defaults.
- S10: the default kubelet drop of some `le` buckets on `csi_operations` and `storage_operation_duration` as a bucket-reduction case.
- S11: retention (`retention`, `retentionSize`, 10d by default) read from the values for the disk and cost model.

## Optional realism suite: SnowOps Labs (from S7)

[SnowOps Labs](https://github.com/snowops-oss/snowopslabs) (Apache 2.0) builds a real Kubernetes platform on k3d or kind: kube-prometheus-stack, Grafana, Loki with Promtail, Tempo with Alloy for traces, Istio, Kafka, Vault, Argo CD and OpenCost, with 12 reversible faults graded against the live cluster. tvt-lab stays the source of exact ground truth; SnowOps tests precision, safety and scale, so it has no golden output. It needs Docker with at least 2 CPUs and 4 GB per scenario: run it in a Codespace or apart from tvt-lab's Podman stack, and review its `install.sh` first.

- A values overlay for tvt: longer Prometheus retention (it ships 24h, 2 GB), the Prometheus query log and Grafana router logging, plus a `make snowops-scan` that runs `tvt scan` read-only against it.
- S5: precision and scale on a full kube-prometheus-stack plus Istio cluster (no hard keep misclassified; scan time and memory against the series count).
- S7: `ServiceMonitor`/`PodMonitor` `metricRelabelings` and kube-prometheus-stack Helm values as `propose` targets.
- S8: the diagnosability loop (apply cuts, inject each fault, still diagnosable and graded), and the `observability-sre` scenario for OTTL readers and connector chains.
- S11: compare the self-hosted fixed floor and disk estimate with OpenCost's allocation for the monitoring namespace.
- Later: the `victoria-metrics-k8s-stack` provider for the VictoriaMetrics adapter and MetricsQL; Promtail for logs beside Alloy for traces as the double-shipping and agent-migration case.

## Later

- Spike: can the Podman machine VM (kernel 5.10 or newer) run the OTel eBPF profiler and OBI in a privileged container with host PID and `/sys/kernel/tracing`? If yes, an optional `profiling` Compose profile (Pyroscope plus the profiler, off by default), with an `unknown_service` process and a profile fixture.
- A second agent tailing the shop's debug log under a different job label: double shipping.
- Logs with and without a trace ID field: triage readiness.
- A VictoriaMetrics single node with `-storage.trackMetricNamesStats`, for the VictoriaMetrics adapter.
