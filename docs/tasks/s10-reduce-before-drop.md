# S10: reduce before drop

**Status:** In progress (Nov 19). Item 1, part one: coarse label reads (`parse.Coarse`, `graph.LabelCoarse`) and `bucket` recommendations in the report (`report.bucketRows`). Part two (Nov 20): the OTel Collector writer, `replace_pattern` in the label-removal ordered pipeline so `interval/tvt` sums the merged series; other pipeline kinds skip with the reason. The lab seed is tvt-lab #42. Still open: the replay guard (a reader matching a value exactly blocks a bucket). Item 2, part one (Nov 20): retention tiers on dormant metrics' confirm rows (`report.RetentionTier`, `parse.Lookback`, `tiers.dormant_retention`), report only. Next for item 2: writers (VictoriaMetrics retention filters and downsampling, a short-retention Mimir tenant) and check's lookback guard (VictoriaMetrics or a Mimir tenant: Jason's pick). Item 3 (level tiers) needs per-level volumes for streams whose level isn't a label, which only the lab can measure: deferred to the lab run. Item 5, part one (Nov 20): `intervals` in the report (`report.intervalRows`, `parse.QueryWindows`), report only. Part two (Nov 20): `propose.PrometheusIntervals`/`AlloyIntervals` (library; `stripBlocksIf`). Part three (Nov 20): `tvt propose` writes them, manifest `scrape_limits`, check refuses an increase past them. Item 5 code done; its lab check comes with the done-when run.

**Done when:** for each lab finding, `propose` picks the least-lossy action that `check` accepts.

## Before you start

Read `CLAUDE.md`, `docs/tasks/README.md`, `docs/architecture.md` and ADRs 0003, 0006 and 0007 first. The only writes tvt makes are Git branches and PRs in mapped pipeline repos; every backend stays read-only.

Part of the MVP (S7–S12). The MVP is done when `v0.1.0-rc1` is tagged; on the lab, `tvt propose` opens a draft PR for a seeded drop, `tvt check` passes it and fails a PR that would break any hard keep or record, the PR leaves draft only after a recorded owner confirmation, and the next `tvt scan` records the measured saving in the ledger.

- ADR 0010's least-lossy order, enforced by S8's `check`.
- tvt-lab's S10 seeds (`lab-backlog.md`).

## Must have (in this order)

1. **Label removal and bucketing.** When a label is always aggregated away, keep the metric and remove the label (vmagent stream aggregation `without:`/`drop_input_labels`, Alloy or Collector equivalents, or a recording rule replacement). When every reader uses only a label's coarse form (`status_code=~"5.."`, a route rather than a raw path), bucket the values in the Collector instead; the replay check confirms readers' results don't change. Stream aggregation also replaces slow recording rules.
2. **Retention and downsampling tiers** for dormant signals before archive and drop: Mimir per-tenant retention, VictoriaMetrics retention filters and downsampling. Respects S8's lookback limit.
3. **Level-based log tiers.** For streams where nothing reads info or debug lines, route levels below warn to the cheapest tier (Loki `retention_stream` or a separate tenant, Fluent Bit `rewrite_tag`, Alloy `loki.process`). A reader of lower-level lines blocks it for that stream.
4. **Collapse with a count, then sample.** The OTel Collector `logdedup` processor collapses identical lines over an interval into one with a count (confirm the count attribute name and settings in the contrib docs first); 1-in-N sampling (Alloy `stage.sampling`, Fluent Bit) comes after it. Each rule carries its saving.
5. **Scrape interval increase.** When every reader of a job's metrics uses a step and range windows well above the scrape interval, raise the job's `scrape_interval` in Prometheus or Alloy instead of dropping anything, within S8's limits.
6. **Staged drops with a revert.** Reroute to the archive first, drop after the quiet period (default 30 days) with no new reader. Every drop PR is generated with its revert PR.
7. **Cap before drop, and keep one copy.** Bytes per attribute key on spans, span events and logs; a large key nothing reads gets a cap (truncate to 512 characters plus a marker attribute), ranked ahead of a drop. One large value carried by two signals in the same trace (a tool's parameters in a log record and a span event, matched by hashes of the values, never the values) gets a proposal to keep one copy.
