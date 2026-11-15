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
| Label | `label_removal` |
| Log stream | `reroute`: to cold storage or shorter retention, never line-level (ADR 0006) |

`downsample` and `finding` are reserved for S5 and S6.

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

Dormant signals are never recommendations. Each becomes a `confirm` row: `id` (`confirm:<signal id>`), `signal`, `reason`, `question` (for the owner: nothing read it in the window, it looks incident-critical because of the reason, will they need it in an incident, and what dropping it would save), `owner`, `tier`, `savings` and `gaps`. Rows come highest saving first, limited by `--top`; a label whose metric is also dormant is left out, since the metric's question covers it. `totals.dormant` counts every dormant signal. An owner's answer arrives with S9's owner workflow.

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
| `owner` | From `tvt.yaml` ownership |
| `status` | `open`, `confirmed` or `dismissed` (with `dismissed_reason`); owner decisions arrive with S9, so every row is `open` for now |
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

`scan` (ID, start, window, log coverage, price card), `sources` (every evidence source's coverage), `gaps` (every gap, acknowledged or not, above), `unmatched_acknowledgements`, `totals` (candidates, recommendations, dormant signals, metric savings, and stream savings apart, since stream selectors overlap), `held`.

## Line-level findings

`line_findings` (version 5) lists line patterns worth cutting inside a stream, from Loki's pattern API (`/loki/api/v1/patterns`, which Loki doesn't log as a query) over its last 3 hours. `tvt scan` asks about the 10 largest streams, one stream's full label set per call. Each finding has the stream, the pattern (`<_>` for each variable field), its line count and average rate over the window, its estimated share of the stream's bytes (lines × pattern length, 8 bytes per field), that share of the stream's measured bytes per day, and an approximate monthly saving at the card's log price. tvt never acts on them: per ADR 0006 it doesn't write line filters, and each finding points to [zeroreads](https://github.com/Bisman-Singh/zeroreads) for the proven cut.

- `repeated_line` (S5 item 3): one pattern with identical text (no variable field) is at least 25% of the stream's estimated bytes. Fix it at the source (log once, rate-limit) or cut the repeats with zeroreads.
- `line_handoff` (S5 item 4, ADR 0006): in a stream something reads, the dominant pattern with variable fields, at least 25% of its estimated bytes, that no reader's line filter selects. Loki often splits one pattern on a field with few values (`probe=kubelet-liveness`, `probe=alb-target-health`), so patterns that differ in exactly one token are merged first (`probe=<_>`). zeroreads proves no alert, dashboard or query filter needs those lines and generates the cut; tvt still ranks and acts on the stream as a whole. A stream nothing reads gets a whole-stream reroute instead, and a stream a hard keep (a Loki alert or rule) reads gets no hand-off: that rule's owner decides. A reader with no line filter (a logs panel showing every line) doesn't count as reading the pattern; one whose `|=`/`|~` filters pick it out does.

A stream the dormant rules hold (an error stream by name, or a tier 0 or 1 owner's) gets no line finding: lines repeated in an error stream are a symptom, never a saving. A Loki without the pattern ingester answers 404, and the scan goes on without line findings and says so.

Readers check `version` (now 5: 2 made gaps structured, 3 added `services`, 4 added `scan.repriced_from`, 5 added `line_findings`) and refuse one they don't know.
