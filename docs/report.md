# The report

`tvt report` reads the latest scan in the database and writes the JSON report, then renders the HTML report from that JSON. The JSON report is the internal API: the HTML report, `propose`, `check` and later the MCP server read it, never the database.

```sh
tvt scan ... --db tvt.db
tvt report --db tvt.db --config tvt.yaml --json report.json --html report.html --top 50
# --json - writes only the JSON, to stdout; --html "" skips the page; --top 0 keeps every recommendation
tvt report --db tvt.db --price-card pricecard-selfhosted.yaml   # what-if: the same scan on another card
```

`--price-card <file|preset>` makes a what-if report: the stored scan's cost rows are repriced at that card (`cost.Reprice`, with the scan's stored TSDB measurements for a self-hosted card, so it equals what a scan with that card would have recorded), and each score's cost and priority are recomputed with the same usage weight and confidence (`score.Priority`). Evidence, classes, confidence and gaps don't depend on price, so only savings, disk, ranking and totals move. `scan.price_card` and `currency` become the what-if card's, `scan.repriced_from` names the card the scan used, and the HTML page says it's a what-if. A relative card path is relative to the working directory; a name that isn't a file is a preset.

The HTML page is one self-contained file (an embedded `html/template`, inline CSS, no scripts or external assets), so it can be attached or archived as is. `report.RenderHTMLFromJSON` decodes the JSON first and refuses a version it doesn't know, so the page can only show what the JSON holds. Each row shows the claim, the cheapest check, its gaps, savings, check minutes and owner, with evidence, readers and assumptions folded under it; each row's anchor is its `id`.

`tvt report` never creates a database and never talks to a backend.

## What gets recommended

Only `candidate` signals with a score above zero confidence. Hard keeps, keep_high and dormant signals never are. Candidates tvt can't trust yet are counted under `held` by reason: `window` (the scan's window is shorter than `window_days`), `coverage` (no usage log could show the signal read), `new` (it first appeared in this scan). A label whose metric is recommended for a drop is left out, because the drop covers it. So is `loki:stream:*`, the stand-in for selectors tvt couldn't read.

| Signal | Action |
| --- | --- |
| Metric | `drop` |
| Label | `label_removal`; or `bucket` for a read label whose every reader matches it only by class (S10 item 1): `code=~"5.."`, `code!~"2.."`, `[45]..` are coarse reads (`graph.LabelCoarse`), while an exact value, a grouping, a legend, a reader of every label or an assumed reader blocks it. The row keeps the label and buckets values to their leading digit and 00 (503 to 500), which every coarse pattern still matches; `merges` estimates the series that collide, so it needs a writer that sums them (S10) and isn't proposed yet |
| Log stream | `reroute`: to cold storage or shorter retention, never line-level (ADR 0006) |

`downsample` and `finding` are reserved for S5 and S6.

### Label removals (S6 item 7)

Label removals are off unless `tvt report --label-drops` is given; without it every label candidate is held as `label_drops_off`. They stay behind the flag until a precision review on real data passes.

With the flag, tvt checks whether the metric's series stay unique without the label. During the series read for label cardinality, `prometheus.Client.LabelCardinality` counts, per label, the distinct series once that label is removed (`collect.LabelCardinality.SeriesWithout`); merges = series − series without (schema v22 `costs.merges`, -1 when unknown, and `costs.summable`). Each label row then has a `removal`:

| Merges | Metric type | Result |
| --- | --- | --- |
| unknown (no series read: a Mimir scan, whose cardinality API gives counts, not series) | any | held `label_drop_unchecked` |
| 0 | any | `removal: "labeldrop"`: a plain `labeldrop` keeps every series |
| more than 0 | counter or histogram (metadata type, else a `_total`/`_bucket`/`_sum`/`_count` name; a summary only for `_sum`/`_count`) | `removal: "aggregate"` with `merges`: the colliding series must be summed (`sum without(label)` or the pipeline's aggregation), never a plain labeldrop, which would make series collide and lose samples |
| more than 0 | gauge, summary quantile, unknown | held `label_drop_merges`: there's no correct way to merge them |

The HTML row shows the removal next to the action. Golden: `TestBenchmarkLabelDrops` (the lab's `user_id` on `shop_cart_operations_total` is `aggregate`, its merge count worked out from the recorded series); the benchmark runs with the flag on.

## Dormant: confirm

A dormant signal is unread but looks incident-critical: its name says error, latency, saturation or queue depth, its owner's tier is 0 or 1, a runbook or postmortem names it without querying it, or its owner has a season the window missed.

**Seasons.** Some readers only run in some months: a quarter-end close, a holiday peak, tax season. `tvt.yaml` declares them:

```yaml
seasons:
  - name: quarter close
    months: [3, 6, 9, 12]
    services: [finance]     # ownership.owners names
```

A season is seen when the scan window touches any of its months. One the window misses is a `season` gap in the header, and every unread signal its services own is dormant ("seasonal reader: quarter close falls outside the window"), not a drop candidate. To clear it, scan with a window that reaches back into one of its months. Runbook mentions come from the runbooks alerts link to; postmortems from `postmortems.paths` in `tvt.yaml` (files or directories of `.md`, `.markdown`, `.txt`, `.html`). A mention is any metric-shaped word (it has `_` or `:`) that names a collected metric.

Dormant signals are never recommendations. Each becomes a `confirm` row: `id` (`confirm:<signal id>`), `signal`, `reason`, `question` (for the owner: nothing read it in the window, it looks incident-critical because of the reason, will they need it in an incident, and what dropping it would save), `owner` (with `owner_source` and `owner_inferred`, as on a recommendation), `tier`, `savings`, `gaps` and `evidence_hash` (for an owner's keep). A dormant signal no owner source names gets the gap `ownership: no owner source names an owner (missing ownership)`: it stays report only, never routed to a guess. Rows come highest saving first, limited by `--top`; a label whose metric is also dormant is left out, since the metric's question covers it. `totals.dormant` counts every dormant signal. An owner's answer arrives with S9's owner workflow.

**Retention tier** (S10 item 2, ADR 0010: shorter retention comes before archive and drop). On a self-hosted price card, a dormant metric's row also offers a third answer: keep a short history. `retention` has `keep_days` (`tvt.yaml` `tiers.dormant_retention`, default 7d, or the longest window any reader of the metric reads back if that's longer, rounded up to a day), `current_days` (the backend's retention in the scan's self-hosted model, measured or from the card), `lookback` and `reader` (that longest window and who reads it; through a recording rule, the rule's own window counts), `monthly_saving` and `disk_bytes` (the storage freed, an upper bound since the head and WAL share doesn't shrink) and `writer`. A reader's window is `parse.Lookback`: the longest range plus offset over its selectors, a subquery's range and offset added to everything beneath it. A window tvt can't read (a range or offset from a dashboard variable such as `$__range`, an `@` modifier, a query that doesn't parse) means no tier, and `retention_held` says which reader; so does a tier no shorter than the retention now. Dashboard time pickers and range-query spans aren't recorded, so a dormant metric's panels (rare: a viewed panel makes it read) are guarded only by their windows. Vendor cards get no tier: their price doesn't follow retention. Prometheus and Mimir keep one retention per server or tenant, so the tier lives in a Mimir tenant of its own (Jason's pick, Nov 20): with `tvt.yaml` `tiers.dormant_tenant` set, `writer` is `Mimir tenant <id>` and the tier is also a `retention_tier:<signal>` recommendation (with `keep_days`), which the owner confirms on the PR like any change; without it, `writer` says tvt writes nothing. A dormant metric has no reader, so the tier's days only have to cover the dormant floor and any window a recording rule reads back, which `keep_days` already does. The HTML report and the weekly digest show the tier under the question.

**Scrape intervals** (S10 item 5, ADR 0010: raising the interval comes after label removal and before retention and drops). `intervals` lists, per scrape job with readers (jobs from the pipeline targets' `job` values), `job`, `current_seconds` (the job's measured interval; a pushed or unmeasured job is held), `proposed_seconds`, `metrics`, `readers`, `shortest_window` and `shortest_by`, `bound` (the guard that stopped tvt going further) and `monthly_saving`. The proposal is the longest of 30s, 1m, 2m and 5m above the current interval that passes every guard: each range a reader uses over the job's raw samples holds at least 4 samples (`parse.QueryWindows`; through a recording rule, the rule's own query counts, since its readers read its output); a reader with no range keeps 2 samples in Prometheus's 5m lookback, so at most 2m; and no alert on the job's metrics passes its severity's detection objective (`detection.objectives`) at the new interval. A reader whose window tvt can't read holds the job with the reason, including any `$__rate_interval` panel: Grafana derives it from the data source's scrape interval setting, which tvt doesn't change. Jobs nothing reads are left out (their drops cover them). The saving is the metrics' cost × (1 − current/proposed) on a self-hosted card (an upper bound: head and WAL don't shrink), 0 on a vendor card, whose price here follows active series. `tvt propose` writes the proposed ones (`docs/propose.md`), and each repo's manifest carries `scrape_limits` (job, `max_seconds`, `why`: the proposal for a proposed row, the current interval for a held one) for `check`. The HTML report lists them under "Scrape intervals". On the lab's writers-2 capture: grafana, loki and prometheus 10s to 2m; alertmanager held by a `[1m]` panel; node and shop held by `$__rate_interval` panels; coding-agent held (pushed).

**Savings ledger** (S11). `ledger` appears once a tvt PR has merged: `tvt scan` records each recommendation of a merged PR from the pipeline repo checkout's `.tvt/proposal.json` (pull the checkout before scanning; schema 29 `merges`), with the time the file landed on the default branch as the merge time (the merge commit's, for a PR merged with one: `git log --first-parent`). Each row has `repo`, `recommendation`, `action`, `signal`, `merged_at`, `status` (`measured`; `settling` while every scan after the merge came before `settles_at`, the merge plus `report.LedgerSettle`: 3 hours for metrics and labels, since Prometheus and Mimir keep a stopped series in the head block until head compaction, and 24 hours for streams, Loki's volume window; its `after` is shown as provisional and not counted; `pending` until a scan after the merge; `unmeasured` when the database has no scan before it), `settles_at`, `before_scan` and `after_scan` (the first scan past `settles_at`), `before` and `after` (`series`, `bytes_per_day`, `monthly`, `disk_bytes`; a signal the scan didn't see, such as a dropped metric, is zero), `saved_monthly` and `expected_monthly` (what the proposal claimed). Totals: `saved_monthly`, `expected_monthly` (measured rows only), `settling`, `pending`, `growth` (`baseline_scan`, `baseline_monthly`, `now_monthly`, `total_change`, `growth` = the change with the savings added back, `new_signals`, `existing_signals`; metrics and streams only, since a label's cost is a share of its metric's), and on a self-hosted card `disk_freed_bytes` and `retention_days_gained`. `collector` (S11 item 4): for each `otelcol` pipeline with `pipelines[].telemetry` set (the Collector's own metrics endpoint, such as `http://otelcol:8888/metrics`), `tvt scan` reads each exporter's `otelcol_exporter_sent_log_records`, `_spans` and `_metric_points` counters with a GET (schema 30 `collector_sent`), and the ledger gives each exporter's send rate per second over the last two scans before the first merge and the last two after the last one (`before_per_second`, `after_per_second`; `before_known`/`after_known` false where a restart reset the counter or there aren't two scans). Bytes per exporter (`otelcol_exporter_enqueue_size_bytes` needs `service.telemetry.metrics.level: detailed` and a `sending_queue`) aren't read yet. The text summary and the HTML report ("Savings ledger") show it.

**Collapses** (S10 item 4, ADR 0010: the second log action, after a cap). A stream with `repeated_line` findings (Loki's pattern API: identical text, at least a quarter of the stream's bytes) that nothing reads and that isn't a record gets a `collapse` recommendation, `collapse:loki:stream:<selector>`: the OTel Collector's `log_dedup` turns each identical line into one record a minute with a `log_count` attribute. The saving is each pattern's cost but one line a minute (a line repeated r times a second keeps 1 in 60r); a line that repeats less than once a minute saves nothing and gets no row. A read stream gets none, since every count over it would change (check's `collapse_counts`). The saving overlaps a level tier's or reroute's of the same stream.

**Level tiers** (S10 item 3, ADR 0010: after collapse, before sampling). For a stream whose level isn't a label (an OTLP stream whose severity is structured metadata, say), `level_tiers` offers to route its lines below warn to the cheapest tier. The scan measures the 10 largest such streams per level over the last hour (`scan.LevelStreams`, `scan.LevelWindow`): one query each, `sum by (detected_level) (bytes_over_time(<stream>[1h]))`, tagged `Source=tvt-levels`, the one LogQL query besides the probe (invariant 4's exception), and scales the line bytes to the stream's bytes per day (schema 27 `stream_volumes.levels`; a Loki that sets no `detected_level` leaves the stream unmeasured). A stream whose level is a label is a stream per level already, so its debug or info stream is a `reroute` row instead. Each row has `stream`, `bytes_per_day`, `levels`, `below_warn_bytes_per_day` and `share` (trace, debug and info; `unknown` stays hot), `readers` (each consumer of a stream signal that selects the stream, with the lowest level it sees: `parse.LevelFloor` reads level matchers on `level`, `detected_level`, `severity_text`, `severity` and friends in the selector or a label filter, all of which must hold; a query with none, or with a label filter using `or`, sees `all`; a line filter like `|= "error"` doesn't count, since a debug line can hold the word; sweeps aren't readers), `records` (the built-in agent tool-call events and `tvt.yaml` `records.log_events`, which the tier leaves hot whatever their level, ADR 0011), `error_guard` (lines matching it stay hot) and `monthly_saving` (the bytes below warn at the stream's log price: an upper bound, since record and guarded lines at those levels stay). A reader that may see lines below warn holds the row (`held` names it), as does a record stream. A row nothing holds is also a recommendation, `level_tier:loki:stream:<selector>` (action `level_tier`), on the stream's own signal, with its pipeline from the stream's label set: the owner confirms it and `tvt propose` writes it like any other. It takes the place of the stream's whole-stream `reroute`, which is more lossy (ADR 0010). The HTML report lists them under "Level tiers". On the lab after tvt-lab #45 (Nov 21): `{service_name="coding-agent"}`, 99.9% of its 29.8 MB/day below warn, no readers, 0.40 USD a month on the grafana-cloud-pro card. The split between levels follows line bytes, so a level whose lines carry large structured metadata (the agent's records) is undercounted; only the below-warn total counts for the tier.

Recommendations are ranked by **savings per minute of checking**: the score's priority (monthly cost ÷ (1 + usage weight) × confidence) divided by `check_minutes`. A cheap check of a moderate saving can outrank an expensive check of a larger one.

## Evidence kept per service

Next to what tvt would cut, `services` lists what stays protected, one entry per `ownership.owners` name (signals no owner claims are `(unowned)`), largest cut first. `cut` counts every recommendation for the service and its monthly savings, before `--top` trims the rows, so the proportion is honest. The kept tallies (`hard_keep`, `keep_high`, `dormant`, `incident`: signals with incident-class evidence, and `records`, zero until S7's records class) count metrics and streams with their monthly cost; a label's cost is already in its metric's, so labels aren't counted. A signal can be in `incident` and in a class tally at once. The HTML page shows it as the "Evidence kept per service" table.

## tvt why

`tvt why <signal>` shows everything tvt knows about one signal's readers, from the same latest scan:

```sh
tvt why --db tvt.db shop_cart_operations_total          # a metric or stream name
tvt why 'prom:label:shop_cart_operations_total:user_id' # a signal ID
tvt why 'shop_cart_operations_total{user_id}' --json    # a label as metric{label}; flags before or after
```

A name found in several backends (`prom` and `mimir`) is ambiguous and the error lists the IDs. The view gives the signal's class (and why it is dormant), owner and tier, monthly cost, score or why it's held, its last logged read across every scan tvt kept (`Store.LastReads`, never decays), its report row (`drop:…`, `confirm:…`) if any, and every reader: evidence class, exact or inferred, source (`query log`, `rules API`, `dashboard JSON`, `runbook`, `SLO file`, `tvt.yaml` for external consumers, `assumed`), first seen (the start of the first scan that stored the reference, schema v15 `refs.first_scan`; references from before v15 date from their last scan) and last seen (the latest logged query), labels read, and why it is inferred (assumed reasons, a recording rule in between, a sweep). Readers come strongest first: declared, then incident, then observed; exact before inferred; sweeps last. The gaps are the signal's evidence gaps, acknowledged ones marked. `--json` prints `report.Why`. Every report row, recommendation or confirm, shows its `tvt why` command, so the page and the terminal give the same view.

## Each recommendation

| Field | What it holds |
| --- | --- |
| `id` | `<action>:<signal id>`, stable across scans |
| `claim` | One testable sentence: what tvt saw in the window and what the change saves |
| `evidence` | `references` (consumer IDs), `query_log` (logged queries that touched the signal, sweeps flagged), `last_read` (the most recent logged read from any scan; it never decays into "never read"), `gaps` (unacknowledged) and `acknowledged_gaps` (with reasons) |
| `cheapest_check`, `check_minutes` | The fastest way to confirm or refute the claim. In order: ask whoever last queried it (2 min); ask its owner (5); ask the team of an unviewed dashboard that shows it (5); search code and configs (15) |
| `downstream` | Every reader: consumer, kind, evidence class, source (`query log`, `rules API`, `dashboard JSON`, `SLO file`, `runbook`, `tvt.yaml`, `assumed`), client, via, last seen, actor, `exact` (false when inferred), `sweep` |
| `savings` | Monthly, in the price card's currency; `approximate` for a vendor preset or a stream (selectors overlap); `disk_bytes` on a self-hosted card |
| `risk_if_wrong` | The strongest evidence class among its readers (sweeps excluded), the dormant flag and the owner's tier |
| `assumptions` | Every gap in the sources that could have shown a reader, assumed references with their reasons, and the stream caveats |
| `owner`, `owner_source`, `owner_inferred` | Who decides, and where tvt found them (S9 item 1): `tvt.yaml` (ownership.owners), `codeowners`, `label` (an ownership label on the signal's series), `backstage` (a component's `spec.owner`), or the inferred fallbacks `service_name` (ownership.services) and `kubernetes` (a workload's namespace team label or `app.kubernetes.io/part-of`). An inferred owner is a guess to confirm, and the row says so |
| `expected` | What should be true after the merge (S9 item 5), for the owner to read first and `tvt verify` (S11) to check: `statements` (for a drop, no new samples within an hour of the deploy, nothing goes empty, the saving; for a label removal, new series without the label and the active series after; for a reroute, the stream leaving the hot tier, the bytes a day that leave it and their share of all stream bytes, and that error lines stay hot), `within_sec` (3600), `series_after`, `hot_bytes_per_day_fall`, `share_pct`, `empty_readers` and `monthly_saving` |
| `evidence_hash` | A short hash of the row's readers and unacknowledged gaps (S9 item 2), without query times or cost: what an owner's confirmation records. A new reader or a reopened gap changes it |
| `status` | `open`, `confirmed` or `dismissed` (with `dismissed_reason`), from the owner's latest decision in the pipeline repo's `.tvt/decisions.yaml` (S9 item 3; `tvt report --config` reads each pipeline's `checkout`). A dismiss or keep holds the row back (not proposable) until its review date; a seasonal keep (`tvt confirm --seasonal`) until after the next season |
| `review` | Why an owner's earlier decision no longer holds, when it doesn't: the evidence hash changed (a reader came or went, a gap opened), the owner of record changed, or the review date passed. The row is `open` again (S9 item 4) |
| `score` | Usage weight, strongest evidence, confidence, priority |

## Evidence gaps

A gap is a way the evidence falls short: a reader it would have shown can't be ruled out. The header lists every one, each with a `source`, a `kind` and its `text`:

| Kind | When |
| --- | --- |
| `unconfigured` | A usage log isn't set up (a backend's query log, the Grafana router log), so it shows nothing |
| `unproven` | A query log whose probe never appeared: tvt can't show it records every query |
| `partial` | A source that covers only part of the window |
| `unreadable` | A definition tvt couldn't read: a runbook it couldn't fetch, an SLO whose SLI it can't parse, panels on a datasource it doesn't parse (a consumer with `unparsed` set reads something tvt can't see, so its dashboard is never "unused") |
| `assumed` | References tvt inferred rather than read (one header gap per consumer, with how many signals and why) |
| `season` | A `tvt.yaml` season the window misses: its readers couldn't show |
| `storage` | A storage hygiene warning about the Prometheus TSDB (source `prometheus storage`): no `retention.size`, or one above 85% of the volume `tvt.yaml` `storage.volume_size` declares. It hides no reader and is never a saving, so it stays in the header and never reaches a row |
| `monitors` | A ServiceMonitor or PodMonitor in `tvt.yaml` `kubernetes.manifests` that no Prometheus selects (source `kubernetes monitors`), one tvt can't decide, or a scrape pool naming a monitor the manifests don't show as selected. It hides no reader and is never a saving, so it stays in the header |

Grafana Cloud's blind spots (direct Loki API queries Usage Insights doesn't see) join with the Cloud adapters (Sprint 2 item 11).

Every row lists the gaps that could hide one of its readers: its backend's query log, the Grafana router log, the definition sources, and its own assumed readers.

### Storage hygiene (S5 item 6)

A Prometheus scan reads the server's limits from its own `/metrics` (`prometheus_tsdb_retention_limit_bytes`, `_seconds`: a GET, never a query) and prints a `storage` line. Two warnings:

- **No `retention.size`.** Only the time limit bounds the TSDB, so a cardinality spike (the kind tvt exists to find) can fill the disk before old blocks age out.
- **`retention.size` above 85% of the volume**, when `tvt.yaml` `storage.volume_size` gives the volume (the PVC's requested storage, or what `df` shows: `50Gi`, `500GB`, bytes). Compaction writes new blocks before it deletes old ones, so the disk needs room past the limit; Prometheus' storage docs suggest 80-85%.

tvt can't size the volume itself: `node_filesystem_size_bytes` would take a PromQL query, which tvt never runs against a backend that logs queries, and it doesn't read Kubernetes. Without `storage.volume_size` only the first warning applies. Mimir keeps its blocks in object storage, so a `--mimir` scan has no `prometheus storage` source.

### Selected monitors (S5 item 8)

prometheus-operator scrapes only the ServiceMonitors and PodMonitors a `Prometheus` (or `PrometheusAgent`) object selects. kube-prometheus-stack's default (`serviceMonitorSelectorNilUsesHelmValues: true`) selects only monitors labelled with the release, so a team's monitor without `release: <name>` scrapes nothing, and an edit to it changes nothing. tvt reads manifests from disk, never a cluster:

```yaml
kubernetes:
  manifests: [deploy/rendered/]   # files or directories, relative to tvt.yaml: helm template output, a GitOps checkout
  namespace: monitoring           # for objects that name none (helm template's --namespace); default "default"
```

`kube.Resolve` applies the operator's rules: a missing or null monitor selector selects nothing and `{}` everything; a missing namespace selector means the server's own namespace and `{}` every namespace; any other namespace selector is matched against the `Namespace` objects in the manifests (every namespace also carries `kubernetes.io/metadata.name`). A namespace the manifests don't define makes the answer unknown, unless the selector reads only that name label; an invalid selector is unknown too. `matchLabels` and `matchExpressions` (`In`, `NotIn`, `Exists`, `DoesNotExist`) must all hold.

The scan prints a `monitors` line (`15 of 17 monitors selected by 1 server, not selected: batch/legacy-jobs, shop/shop`) and stores source `kubernetes monitors`: coverage is the share of monitors tvt could decide (0 when the manifests hold no Prometheus), and each gap line is a monitor nothing selects, one tvt can't decide, or an operator scrape pool from `/api/v1/targets` (`serviceMonitor/<ns>/<name>/<n>`) naming a monitor the manifests don't show as selected (the manifests are out of date). `kube.Selection.ForPool` is the only way a scrape pool maps to a monitor, and it answers only for a selected one, so no series or saving is ever attributed to a monitor that doesn't scrape. A manifest path tvt can't read fails the scan before it contacts any backend.

**Acknowledging a gap.** A gap you've accepted goes in `tvt.yaml`, with a reason:

```yaml
gaps:
  acknowledge:
    - source: loki query log          # as the report names it
      reason: Loki is only read through Grafana here
    - source: datasources
      match: grafana-pyroscope        # only gaps whose text contains this
      reason: profiles aren't metrics
```

An acknowledged gap stays in the header with its reason and moves to the row's `acknowledged_gaps`. An unacknowledged one shows in every affected row's `gaps`, and `check` will fail on it (S8). An acknowledgement that matches no gap is listed under `unmatched_acknowledgements`, so stale entries surface. `tvt report --config tvt.yaml` reads them.

## The header

`scan` (ID, start, window, log coverage, price card), `sources` (every evidence source's coverage), `gaps` (every gap, acknowledged or not, above), `unmatched_acknowledgements`, `totals` (candidates, recommendations, dormant signals, metric savings, and stream savings apart, since stream selectors overlap, and rule findings), `held`.

## Line-level findings

`line_findings` (version 5) lists line patterns worth cutting inside a stream, from Loki's pattern API (`/loki/api/v1/patterns`, which Loki doesn't log as a query) over its last 3 hours. `tvt scan` asks about the 10 largest streams, one stream's full label set per call. Each finding has the stream, the pattern (`<_>` for each variable field), its line count and average rate over the window, its estimated share of the stream's bytes (lines × pattern length, 8 bytes per field), that share of the stream's measured bytes per day, and an approximate monthly saving at the card's log price. tvt never acts on them: per ADR 0006 it doesn't write line filters, and each finding points to [zeroreads](https://github.com/Bisman-Singh/zeroreads) for the proven cut.

- `repeated_line` (S5 item 3): one pattern with identical text (no variable field) is at least 25% of the stream's estimated bytes. Fix it at the source (log once, rate-limit) or cut the repeats with zeroreads.
- `line_handoff` (S5 item 4, ADR 0006): in a stream something reads, the dominant pattern with variable fields, at least 25% of its estimated bytes, that no reader's line filter selects. Loki often splits one pattern on a field with few values (`probe=kubelet-liveness`, `probe=alb-target-health`), so patterns that differ in exactly one token are merged first (`probe=<_>`). zeroreads proves no alert, dashboard or query filter needs those lines and generates the cut; tvt still ranks and acts on the stream as a whole. A stream nothing reads gets a whole-stream reroute instead, and a stream a hard keep (a Loki alert or rule) reads gets no hand-off: that rule's owner decides. A reader with no line filter (a logs panel showing every line) doesn't count as reading the pattern; one whose `|=`/`|~` filters pick it out does.

A stream the dormant rules hold (an error stream by name, or a tier 0 or 1 owner's) gets no line finding: lines repeated in an error stream are a symptom, never a saving. A Loki without the pattern ingester answers 404, and the scan goes on without line findings and says so.

## Rules (S6 item 1)

`rules` (version 6) lists every rule (Prometheus, Mimir and Loki rulers, Grafana-managed) with what its evaluations cost and what's wrong with it, rules with findings first. Report only: tvt never edits a rule.

**Cost per rule** is evaluations per day (86400 ÷ the rule's interval) times what one evaluation read, measured from the query logs:

| Ruler | Where one evaluation's cost comes from |
| --- | --- |
| Prometheus | the query log's rule entries (`ruleGroup`), matched by group and query: `stats.samples.totalQueryableSamples` |
| Loki | `component=ruler` metrics.go lines, matched by query (Loki logs no rule or group): `total_bytes` |
| Grafana-managed | the instant queries Grafana sent with the rule's query text (in the Prometheus or Loki log; neither marks them as rule evaluations, see `docs/query-logs.md`), summed over the rule's queries |
| Mimir | unknown for now: its ruler's evaluations aren't read with their cost |

Queries compare with whitespace and enclosing parentheses ignored (Loki logs `(a > 20)` for `a > 20`). An unknown interval, an unread log or no logged evaluation leaves the cost unknown with the reason (`cost_unknown`), never zero. Rows give `evals_per_day`, `samples_per_eval`/`bytes_per_eval` and their daily totals, and how many evaluations the means come from. No price card prices query work yet, so rules carry no money figure on any backend (on Grafana Cloud findings ship without one by design).

**Findings**, each with a detail and its fix:

| Kind | When | Fix |
| --- | --- | --- |
| `grafana_managed_alert` | Grafana evaluates an alert on Loki or Prometheus and queries the datasource every interval | move it to the datasource ruler, or alert on a recording rule there |
| `logql_parses_raw_logs` | a LogQL alert runs a parser (`json`, `logfmt`, `regexp`, `pattern`, `unpack`) every evaluation | record the count with a Loki recording rule; alert on its output |
| `no_line_filter_before_parser` | a LogQL rule's parser has no line filter between it and the selector, so it parses every line | add `\|= "…"` before the parser |
| `long_range_window` | a range or subquery window of at least 1h and at least 120 intervals (`[24h]` every 1m) | evaluate less often, or record a shorter window and aggregate it |
| `interval_below_scrape` | evaluated more often than every metric it reads is scraped (target metadata intervals; skipped when any read metric has none, such as a recording rule's output) | evaluate at the scrape interval or slower |
| `duplicate_rule` | another rule of the same ruler and kind has the same expression; both copies are flagged, each naming the other | keep one |
| `unread_recording_rule` | nothing but sweeps reads a recording rule's output | remove the rule |
| `no_series` | a PromQL rule whose every directly read metric has no series in the TSDB, so it can never return anything (broken, not unused); rules `parse.UsesAbsence` matches are skipped | fix the metric name or delete the rule |
| `group_slower_than_interval` | the group's last evaluation (rules API `evaluationTime`) took longer than its interval | split the group or lengthen the interval |

Not yet: a "broad stream selector" finding (no lab case to check it against), and `no_series` over the whole window rather than the scan's TSDB head. The scan prints a `rules` line (`16 of 16 priced from logged evaluations, 10 findings (duplicate_rule 2, …)`). Golden: `TestRuleFindings` on the benchmark's `rule-efficiency` state, against tvt-lab's `rule_findings:`; every lab rule is listed there or under `no_findings`, so any other finding fails.

## Query load (S6 item 2)

`query_load` (version 7) is what the logged queries in the window read, as the backends measure it, split by S3's clients (`dashboard`, `rule`, `explore`, `agent`, `api`):

| Log | Cost per query |
| --- | --- |
| Prometheus | `stats.samples.totalQueryableSamples` |
| Mimir | query-frontend `samples_processed` |
| Loki | metrics.go `total_bytes` |

Each query's cost goes to the consumer it was attributed to (`graph.QueryCost`, schema v18 `query_costs`: queries, samples, bytes per consumer). A query that matches several panels or rules is split evenly among them, so client and consumer totals add up to the log's. `clients` gives each client's queries, samples and bytes with their shares of the total, heaviest first; `top` lists the 10 consumers that cost most to run (ranked by share of samples plus share of bytes, so PromQL and LogQL load compare on one scale). Samples and bytes are measured work, not money: no price card prices query work yet. Agents are named in `tvt.yaml` `clients.agents` (an MCP user agent counts too); without that, an agent's queries are `api`. Rule evaluations by the rulers themselves aren't query-log consumers: their cost is in `rules`. The scan prints a `query load` line (`139 queries; samples: agent 0%, dashboard 100%, …`). Golden: `TestQueryLoadRuleEfficiency` on the benchmark's `rule-efficiency` state (agents 0.4% of samples, dashboards 99.6%).

## SLO coverage (S6 item 3)

`slo_coverage` (version 8) lists, report only, from the SLO definitions (`slos.paths`) and every ruler's rules:

- `services_without_slo`: a service is a `tvt.yaml` `ownership.owners` name. It has an SLO when an SLO definition names it as its service (Sloth `service`, OpenSLO `spec.service`) or an SLO's SLI reads a signal it owns. With no owners configured nothing is listed, and the `slo definitions` source carries the gap "no ownership.owners in tvt.yaml: services without an SLO can't be listed".
- `paging_not_burn_rate`: an alert whose `severity` (or `priority`) label is `critical`, `page`, `paging`, `emergency`, `p0`/`p1` or `sev0`/`sev1`, or with `page="true"`, that isn't a burn-rate alert. The detail names an SLO reading the same signals when there is one (that SLO's burn rate should page instead).
- `slos_without_alert`: no alert in any ruler is tied to the SLO. The detail says when the definition declares an alert (Sloth `alerting.name`, Pyrra's `ErrorBudgetBurn`) that no ruler has loaded.

A rule is part of an SLO's alerting (a burn-rate alert, or an SLO tool's recorded window) when an SLO definition names it, it carries `sloth_slo`/`sloth_id`/`sloth_service`/`slo` labels, it reads an SLO tool's recorded series (`slo:sli_error:…`, `…:burnrate5m`, `error_budget`), or it is a multiwindow alert: the same expression over two or more windows joined with `and` (`on()`/`ignoring()` allowed). An alert is tied to an SLO by the declared name (Pyrra: plus its `slo` label), by `sloth_slo`/`slo` labels, or by being a burn-rate alert that reads the SLO's signals. These rules are exempt from `long_range_window`: a burn-rate alert needs its `[1h]` and `[6h]` windows. Each row has `id` (the alert's or SLO's consumer ID, or `service:<owner>`), `name`, `detail`, `fix`; `totals.slo_findings` counts them. Schema v19 `slo_findings`. The scan prints an `slo coverage` line when SLO paths are configured. Golden: `TestSLOCoverage` on the benchmark's `slo-coverage` state against tvt-lab's `slo_coverage:` (each list exact).

## Pipelines (S6 item 4)

`tvt.yaml` `pipelines:` (ADR 0007) declares where signals are shaped: each pipeline has a `name`, `kind` (`prometheus`, `alloy`, `otelcol`), `repo`, `branch`, `files` or `helm_values` (`file`, plus `path`, the config's key, for a collector; not both, except that a `prometheus-operator` pipeline lists its rendered monitors in `files` beside the values it writes), `handles` (label selectors, required) and `reviewers`. The scan maps every signal to the one pipeline whose handles match it:

- A metric's labels come from `prometheus.Client.LabelMetrics`: for each label the handles name, its values (`/api/v1/label/<l>/values`) and the metric names that carry each value (`/api/v1/label/__name__/values?match[]={l="v"}`), over the series lookback. These are metadata reads, which the query log doesn't record. A handle's matchers are checked one by one, so a metric whose series split across pipelines is claimed by both. A label signal takes its metric's pipeline.
- A stream (each label set the volume API listed, subject `stream:<selector>`) takes the pipeline whose handles match its labels. A stream signal (a selector) takes the pipeline of every stream it selects; streams on different pipelines make it unmapped.
- A recording rule's output belongs to its ruler: note "a recording rule's output", never unmapped.

No match, or more than one, is **unmapped**: reported with its savings, never proposable, with the reason (`no pipeline's handles match`, `claimed by several pipelines: a, b`). Metrics every job exposes (`up`, `go_*`, `process_*`) are claimed by every pipeline whose jobs expose them, so no single change drops them. A failed or truncated label lookup leaves every metric unmapped with the error rather than map from partial values. Source `pipelines` (left out of LogCoverage): coverage is the share of priced metric and stream signals mapped; the gap lists each unmapped one and each unmapped stream. Schema v20 `pipeline_targets`.

**Unread streams.** Every stream the volume API lists that no queried selector selects becomes a stream signal of its own, named by its full label set (`loki:stream:{app="shop", component="api", level="debug"}`), so it's priced, classified and reported like any other: a reroute to cold storage when nothing marks it incident-critical, a confirm question when its labels do (an `error` stream). When any query's selectors couldn't be read (`loki:stream:*`), or a selector can't be evaluated, no stream is shown unread: an unread stream is never a guess. Benchmark: both lab streams nothing queries (the shop's debug stream, `route_cold`, and the reconciler's error stream, `dormant_confirm`) were false negatives in every state before; now recall is 1.0 in four of seven states, and the other three miss only `startup_burst`.

**Rewrites (S7 item 9).** A pipeline with a `checkout` (a local checkout of its repo, relative to `tvt.yaml`) has its name-touching rules read from `files` there: Alloy `prometheus.relabel` rules and Prometheus `metric_relabel_configs` on `__name__`. A literal rename is followed: readers already match the stored name, nothing is reported for the source name, and tvt propose's rules go after the rename. A rule tvt can't follow (a value that isn't a literal, such as `regex = sys.env(...)`, a templated `replacement`, or another action on `__name__`) is an evidence gap on every metric that passes it: those carrying a scrape job upstream of the rule, the span metrics connector's output (its namespace prefix) and `target_info` when an `otelcol.exporter.prometheus` feeds it, or every metric of the pipeline when tvt can't enumerate the upstream. Those metrics, and their labels, are unmapped with the note `passes a rewrite tvt can't follow (...)`, so never proposable, and the `pipelines` gap names the rule and how many metrics it holds. OTel Collector pipelines are read too, for the `transform` and `metricstransform` processors in a metrics pipeline: an exact rename (`set(metric.name, "new") where metric.name == "old"`; `metricstransform` `include: old`, `match_type: strict`, `action: update`, `new_name: new`) is followed, and anything else that touches a metric name (a pattern replace, a computed or templated name, a regexp rename) is a rule tvt can't follow. The config doesn't say which metrics reach a collector rule, so an unfollowable one holds every metric mapped to that pipeline. `metricstransform` `insert` copies leave the old name stored and change nothing. Alloy `loki.process` components are read too: `stage.labels`, `stage.template`, or a computed `stage.tenant` (also inside `stage.match`) make stored stream labels depend on log content, which holds every stream mapped to that pipeline; literal label changes (`stage.static_labels`, `stage.label_drop`, `stage.label_keep`) are followed. Rules that set metric labels are read too: Alloy `prometheus.relabel` `replace` (and `hashmod`, `lowercase`, `uppercase`) with a `target_label` other than `__name__`, and `labelmap`; OTel `attributes` processors (`insert`, `update`, `upsert`, and `extract`), `transform` statements that `set(attributes["k"], ...)` or pattern-replace attribute keys, and `metricstransform` `update_label` and `add_label`. tvt doesn't yet place a label removal relative to such a rule, so the labels it sets on the metrics that pass it are unmapped with the same note (every label of those metrics when tvt can't tell which: `labelmap`, `extract`, a key pattern), and the `pipelines` gap counts them; the metrics themselves stay mapped. Deletes, hashes, `labeldrop` and `labelkeep` set no label and change nothing, nor do tvt's own blocks. Prometheus `metric_relabel_configs` label rules hold nothing: tvt's rules go last in that list, after every rename. Golden: `TestRewrites` on the writers state; label rules in `TestLabelRewrites` and `TestHoldLabelRewrites` (the lab has none yet).

The report (version 9): each recommendation and confirm row has `pipeline` (or `unmapped`), `pipeline_note`, and recommendations have `proposable`; an unmapped row carries a `pipelines` gap (kind `unmapped`, acknowledgeable). `pipelines` lists each pipeline's recommendations and savings (before `--top`), the unmapped ones, and `unmapped_streams` (selector, bytes per day, note). With no pipelines configured the section is empty and rows have no pipeline. The HTML report shows the pipeline in the owner cell and a "Pipelines" section. The scan prints a `pipelines` line. Golden: `TestPipelineTargets` on the benchmark's `pipelines` state against tvt-lab's `pipelines:` (examples and every metric of each pipeline's jobs map to it, metrics of jobs on both pipelines are claimed by several, the reconciler stream is unmapped, rule outputs are notes).

## Sensitive content (S12 item 5)

A recommendation for an unread label or span attribute whose name suggests sensitive content has `sensitive`: `prompt` (model prompts or completions: `gen_ai.prompt`, `llm.input.messages`), `response` (request or response bodies: `http.response.body`), `email` (`user_email`) or `host_path` (`log.file.path`, `filename`). The HTML shows "may hold …" under the row's saving. tvt reads names and sizes only, never values, so this is a prompt for the owner to look, not a finding about the data; it often makes the drop more worth doing. `report.SensitiveName` matches whole name tokens split on `.`, `_`, `-`, `/` and `:`, and a measurement token (`tokens`, `size`, `count`, `id`, `length`, `status`, `code` and the like) clears it, so `gen_ai.usage.prompt_tokens`, `http.request.body.size`, `http_response_status_code` and `url.path` aren't flagged. Metrics and streams aren't flagged: the flag is about a value-carrying key.
## Overflow series (S6 item 5)

When an OpenTelemetry SDK or Collector hits its cardinality limit (the SDK's limit, or a Collector connector's `aggregation_cardinality_limit`), it folds new attribute sets into one series per metric carrying `otel.metric.overflow="true"` and none of the capped attributes. The backend then shows fewer series than the metric has, and those points lost their labels. `prometheus.Client.Overflow` (Prometheus and Mimir) reads every series with `{otel_metric_overflow="true"}` (and, where the backend takes UTF-8 label names, `{"otel.metric.overflow"="true"}`; a 400 from Prometheus 2 means none) and each such metric's label names (`/api/v1/labels`). These are metadata reads, which the query log doesn't record. A label the metric's series carry but its overflow series don't is **capped** (`le` never is).

The scan makes one finding per metric: overflow series count, capped labels, and the readers (not sweeps) that filter (`match`, LogQL `filter`) or group on a capped label, directly or through the label's signal. They miss or misplace the overflowed points. Source `overflow series` (left out of LogCoverage): coverage 1 when looked for, a gap line per metric; a failed read is a gap, never a failed scan. Schema v21 `overflow_findings`. The scan prints an `overflow` line.

The report (version 10): `overflow` lists each metric with `series`, `capped`, `readers` (consumer and the capped labels it uses), `detail` and `fix`; `totals.overflow` counts them. Each recommendation and confirm row on such a metric (or one of its labels) has `cost_floor: true`, a "cost is a floor" assumption and the `overflow` gap (acknowledgeable). The HTML shows "at least" on those savings and an "Overflow series" section. Never a recommendation or a saving. Golden: `TestOverflow` on the benchmark's `overflow` state against tvt-lab's `overflow:` (each metric, its series and capped labels, and the viewer's customer lookup as the one reader filtering on `customer_id`).

Readers check `version` (now 10: 2 made gaps structured, 3 added `services`, 4 added `scan.repriced_from`, 5 added `line_findings`, 6 added `rules` and `totals.rule_findings`, 7 added `query_load`, 8 added `slo_coverage` and `totals.slo_findings`, 9 added `pipelines` and each row's pipeline, 10 added `overflow`, `totals.overflow` and each row's `cost_floor`) and refuse one they don't know.

## Protection manifest

`tvt report --config tvt.yaml --manifest-dir DIR` also writes one protection manifest per pipeline repo (S7 item 1, ADR 0003 and 0007) to `DIR/<repo>/.tvt/protect.json`, where `<repo>` is the repo path from `tvt.yaml` `pipelines:` with every character but letters, digits, `.`, `_` and `-` replaced by `_`. `tvt check` reads it in that repo's CI, offline, with no credentials; `tvt propose` commits it with its PRs.

A manifest lists every hard keep, keep_high and dormant signal that one of the repo's pipelines handles, and every record (S7 item 2: `record` says why), each with its pipeline, owner and protectors: the readers behind its class, declared evidence first, never a sweep. It carries names and kinds only, never query text or values. `generated_at` is the scan's start, and `tvt check` fails a manifest older than its max age. Signals no single pipeline claims are in no manifest, because they're never proposed. Output is sorted and deterministic, so a refresh diffs cleanly.

```json
{
  "version": 1,
  "repo": "github.com/Opsmastery-Summit/tvt-lab",
  "pipelines": ["alloy-apps", "fluentbit-shop-debug", "otelcol-agent", "prometheus-scrape"],
  "generated_at": "2026-11-17T07:06:36Z",
  "scan": 1,
  "signals": [
    {"id": "prom:metric:shop_payment_queue_depth", "kind": "metric", "name": "shop_payment_queue_depth",
     "pipeline": "alloy-apps", "class": "hard_keep", "owner": "shop",
     "protectors": [{"consumer": "...", "kind": "alert_rule", "name": "ShopPaymentQueueBacklog",
                     "evidence": "declared", "source": "rules API", "exact": true}]}
  ],
  "decisions": [],
  "acknowledged": []
}
```

`decisions` holds owner decisions once the owner workflow exists (S9).

Version 2 (S8) adds what `tvt check` needs to judge a PR with nothing else (`docs/check.md`): `pipeline_defs` (each pipeline's kind and files from `tvt.yaml`), `jobs` on each metric, `record_matches` (OTTL conditions for record spans and log events: the built-in agent tool calls and `tvt.yaml` `records.spans`/`records.log_events`), `gaps` (the scan's unacknowledged evidence gaps), `alerts` (each alert's severity, `for`, interval, range windows, recording rules, metrics, jobs and scrape interval) and `detection` (`tvt.yaml` objectives and `group_wait`).

## Records

A record (S7 item 2, ADR 0011) is a signal tvt never drops, samples or collapses, whatever its readers: an audit log, deploy or change events, and agent tool-call spans once tvt reads spans. tvt marks a signal a record when `tvt.yaml` `records:` declares it (`metrics` regexes, `streams` selectors), when its metric name or one of its stream's label values has an audit, deploy-events or change-events token, or, for a stream signal, when it selects any stream that is a record. A record candidate is held as `record` and never recommended; records are in the protection manifest with `record` saying why. Capping a record's content attribute is allowed when its outcome fields stay; that check comes with `tvt check` (S8).

## Owner decisions (S9)

`decisions` lists every decision in the pipeline repos' `.tvt/decisions.yaml` files (written by `tvt confirm`), each with a `state`: `current` (it stands), `reopened` (its evidence hash changed, or the owner of record moved), `expired` (a dismiss or keep past its `review` date) or `orphaned` (the scan no longer sees the signal: renamed or removed). Anything but `current` comes first, with `why`. Nothing persists or expires silently: a decision is only current while what it rested on still holds, and review dates are the backstop.

## Weekly digest (S9 item 6)

`tvt report --digest DIR` also writes one Markdown file per owner (`DIR/<owner>.md`, unowned rows in `unowned.md`), ready to post as a GitHub issue: their decisions to look at again (reopened, expired, orphaned), their open recommendations with savings, evidence hash and expected end state (at most `queue.per_owner`, highest savings per minute of checking first, default 20, with a count of the rest), and the questions for their dormant signals. Run it weekly (cron, a scheduled CI job) instead of a stream of PRs.
