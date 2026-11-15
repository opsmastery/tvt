# S5: benchmark and log findings

Read `CLAUDE.md`, `docs/tasks/README.md`, `docs/architecture.md` and ADR 0006 first.

**Done when:** the benchmark baseline is recorded, and a deliberately broken parser change fails CI.

## Before you start

- tvt-lab: the distractors and the two frozen lab states from `lab-backlog.md` (S5), and the repeated-line burst (S5).

## Must have (in this order)

1. **Benchmark in CI.** Turn `ground-truth.yaml` into a versioned benchmark: frozen fixture snapshots of each lab state, scored on every change to parsers, scoring or collectors. Two scores, reported separately: evidence found (readers and gaps resolved) and recommendations right (precision and recall against ground truth). A drop in either fails the build, so an improvement in one finding class can't silently degrade another. Record the baseline in the repo.
2. **Distractors.** The benchmark includes a metric with a near-identical name to a read one, a service that logs background errors all day, and a dashboard that reads a metric through a variable. None may change a recommendation.
3. **Repeated-line finding.** From the Loki patterns API (`/loki/api/v1/patterns`), flag streams where one pattern with identical text (no variable fields) makes up a large share of bytes, with the repeat rate and estimated savings. The lab's `/healthz` lines are the first case.
4. **Line-level hand-off to zeroreads** (ADR 0006). For the top log streams, the dominant unread pattern with its share of bytes and estimated saving, pointing to zeroreads for the proven line-level cut. tvt still ranks and acts on whole streams.
5. **Loki query-log liveness probe.** Like Sprint 2's proven Prometheus query log: a marker query that selects no streams, sent with `X-Query-Tags: Source=tvt-probe`, must appear in Loki's `metrics.go` lines, otherwise Loki query coverage is 0 and recorded as a gap.
6. **Storage hygiene warnings.** `tvt scan` warns when a Prometheus has no `retention.size`, or one above about 85% of its volume where the volume size is known (PVC, or `node_filesystem_size_bytes`). Listed with the evidence gaps, never as a saving.

## Should have

7. **Scale test.** Synthetic cardinality (Prometheus `avalanche`) at a few million series: record scan time and peak memory in `docs/architecture.md`, and add a benchmark that fails on a large regression.
8. **Selected monitors only.** With kube-prometheus-stack's default `serviceMonitorSelectorNilUsesHelmValues: true`, Prometheus only selects monitors that carry the release label. Resolve which `ServiceMonitor`/`PodMonitor` objects the Prometheus selectors actually match, and never attribute series or savings to one it doesn't. Test on the pinned chart fixtures (see `lab-backlog.md`).
