# S11: savings ledger

**Status:** Planned.

**Done when:** the MVP gate is rehearsed on the lab: merge a tvt PR, and the next scan records measured savings.

## Before you start

Read `CLAUDE.md`, `docs/tasks/README.md`, `docs/architecture.md` and ADRs 0003, 0006 and 0007 first. The only writes tvt makes are Git branches and PRs in mapped pipeline repos; every backend stays read-only.

Part of the MVP (S7–S12). The MVP is done when `v0.1.0-rc1` is tagged; on the lab, `tvt propose` opens a draft PR for a seeded drop, `tvt check` passes it and fails a PR that would break any hard keep or record, the PR leaves draft only after a recorded owner confirmation, and the next `tvt scan` records the measured saving in the ledger.

- S9's owner workflow merged, so a tvt PR can be merged on the lab.

## Must have (in this order)

1. **Before and after per merged PR:** series or bytes and $/month from the scan before the merge and the first scan after it, stored in the graph and shown in the report.
2. **Savings apart from growth.** What merged PRs saved, separate from what the bill did, with growth attributed to its cause (new metrics, replicas, a release).
3. **Self-hosted results:** GiB freed and how many more days of retention now fit on the same volume (from `retention.size` and the volume size).
4. **Measured from the Collector.** Where a Collector is in the path, read its own exporter telemetry before and after the merge (`otelcol_exporter_sent_log_records_total`, sent spans and metric points, `otelcol_exporter_enqueue_size_bytes`). The bytes metric needs `service.telemetry.metrics.level: detailed` and a `sending_queue`, and it is labelled by exporter, not signal.
