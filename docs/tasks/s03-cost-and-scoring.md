# S3: cost and scoring core

Read `CLAUDE.md`, `docs/tasks/README.md`, `docs/architecture.md`, `docs/pricecard.md` and `docs/query-logs.md` first.

**Done when:** every lab signal has a monthly cost on two price cards (a vendor list price and the self-hosted kind), and the simulated agent sweep keeps nothing on its own.

## Before you start

- Sprint 2 merged, including item 7 (SLOs and runbooks) and items 17–19.
- tvt-lab: the agent clients and the self-hosted storage settings from `lab-backlog.md` (S3).
- The lab has less than 30 days of history until early December, so confidence is capped by the window. Tests must expect that, not paper over it.

## Must have (in this order)

1. **Price card and cost per signal** (`pkg/cost`). Load the price card from `tvt.yaml` (format v1 in `docs/pricecard.md`: strict decoding, required `version`, included allotments applied to the total, vendor presets marked approximate). Metrics: active series × samples per month from the scrape interval. Labels: the share of a metric's series a label multiplies. Histograms: dimensions × (buckets + 2), with the bucket multiplier shown and exemplars flagged when on. Every signal gets $/month and the observation time. On Grafana Cloud, series an Adaptive Metrics rule already aggregates are "already handled" and never counted as savings.
2. **Self-hosted price card kind.** Series become disk: series ÷ scrape interval × bytes per sample × retention × 1.2 index overhead × replicas, plus WAL and head chunks. Bytes per sample is measured (`rate(prometheus_tsdb_compaction_chunk_size_bytes_sum[1d]) / rate(prometheus_tsdb_compaction_chunk_samples_sum[1d])`), with 1.5 as a fallback marked assumed. The scrape interval comes per job from the targets API. The card takes $/GB-month, a fixed monthly floor (replicas, compute) and the node sizes in use: a cut that frees disk but crosses no node step says "frees 120 GiB; node count unchanged", and compute savings count only when a cut crosses a node step. Update `docs/pricecard.md` with the kind (`storage_gb_month`, `replicas`, `overhead`, `bytes_per_sample`, `fixed_monthly`, `node_sizes`) and a worked example where a cut saves disk but no nodes.
3. **Loki bytes per stream.** Bytes per day for each stream from Loki's volume API (`/loki/api/v1/index/volume`, fixture `testdata/lab/loki/volume_by_stream.json`), aggregated to the stream signals Sprint 2 writes, and per-label volume so a label drop on a stream can be priced. Every request sends `X-Query-Tags: Source=tvt`.
4. **What-if pricing.** Reprice the same usage graph at another price card without rescanning (`pkg/cost`, exposed as `tvt report --price-card <file|preset>` in S4), so a migration claim is answered with the user's own usage.
5. **Query-log entries tagged by client.** Every logged query gets a client: dashboard, person in Explore, rule (including Grafana's own evaluations of Grafana-managed rules), agent (by user agent, MCP client headers, or a token named in `tvt.yaml`), or other API. This finishes Sprint 2 item 15: a Grafana query from Grafana's IP that matches a panel target, variable query or Grafana-managed rule query after normalization becomes evidence for that panel or rule; one that matches nothing stays ad hoc. S6 attaches measured query cost per client.
6. **Sweeps aren't reads.** A query that matches every metric name (`{__name__=~".+"}`), or more than a configurable share of series, is recorded as a sweep: weak evidence that never keeps a signal on its own. A signal an agent or person names explicitly still counts as read. tvt's own queries stay excluded through `X-Query-Tags: Source=tvt`.
7. **Scoring and confidence** (`pkg/score`). Usage weight from the evidence classes: declared and incident evidence never decays; observed evidence uses the 15-day half-life, with last-seen dates kept indefinitely; sweeps are weak. Confidence from the observation window (nothing is recommended from less than `window_days`, default 30) and each source's coverage. Only `candidate` signals are scored for recommendation.

## Should have

8. **Validate the self-hosted model.** Compare predicted daily disk growth with `prometheus_tsdb_storage_blocks_bytes` in the lab over a week. Within 15%, or fix the model and say why in `docs/pricecard.md`.
