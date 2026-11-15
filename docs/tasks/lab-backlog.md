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

## S16

- Shop spans named with raw product paths, the spanmetrics histogram on default buckets, a volatile resource attribute in the metric identity, and a debug exporter left on.

## Later

- Spike: can the Podman machine VM (kernel 5.10 or newer) run the OTel eBPF profiler and OBI in a privileged container with host PID and `/sys/kernel/tracing`? If yes, an optional `profiling` Compose profile (Pyroscope plus the profiler, off by default), with an `unknown_service` process and a profile fixture.
- A second agent tailing the shop's debug log under a different job label: double shipping.
- Logs with and without a trace ID field: triage readiness.
- A VictoriaMetrics single node with `-storage.trackMetricNamesStats`, for the VictoriaMetrics adapter.
