# The report

`tvt report` reads the latest scan in the database and writes the JSON report, then renders the HTML report from that JSON. The JSON report is the internal API: the HTML report, `propose`, `check` and later the MCP server read it, never the database.

```sh
tvt scan ... --db tvt.db
tvt report --db tvt.db --json report.json --html report.html --top 50
# --json - writes only the JSON, to stdout; --html "" skips the page; --top 0 keeps every recommendation
```

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

A dormant signal is unread but looks incident-critical: its name says error, latency, saturation or queue depth, its owner's tier is 0 or 1, or a runbook or postmortem names it without querying it. Runbook mentions come from the runbooks alerts link to; postmortems from `postmortems.paths` in `tvt.yaml` (files or directories of `.md`, `.markdown`, `.txt`, `.html`). A mention is any metric-shaped word (it has `_` or `:`) that names a collected metric.

Dormant signals are never recommendations. Each becomes a `confirm` row: `id` (`confirm:<signal id>`), `signal`, `reason`, `question` (for the owner: nothing read it in the window, it looks incident-critical because of the reason, will they need it in an incident, and what dropping it would save), `owner`, `tier`, `savings` and `gaps`. Rows come highest saving first, limited by `--top`; a label whose metric is also dormant is left out, since the metric's question covers it. `totals.dormant` counts every dormant signal. An owner's answer arrives with S9's owner workflow.

Recommendations are ranked by **savings per minute of checking**: the score's priority (monthly cost ÷ (1 + usage weight) × confidence) divided by `check_minutes`. A cheap check of a moderate saving can outrank an expensive check of a larger one.

## Each recommendation

| Field | What it holds |
| --- | --- |
| `id` | `<action>:<signal id>`, stable across scans |
| `claim` | One testable sentence: what tvt saw in the window and what the change saves |
| `evidence` | `references` (consumer IDs), `query_log` (logged queries that touched the signal, sweeps flagged), `last_read` (the most recent logged read from any scan; it never decays into "never read"), `gaps` |
| `cheapest_check`, `check_minutes` | The fastest way to confirm or refute the claim. In order: ask whoever last queried it (2 min); ask its owner (5); ask the team of an unviewed dashboard that shows it (5); search code and configs (15) |
| `downstream` | Every reader: consumer, kind, evidence class, source (`query log`, `rules API`, `dashboard JSON`, `SLO file`, `runbook`, `tvt.yaml`, `assumed`), client, via, last seen, actor, `exact` (false when inferred), `sweep` |
| `savings` | Monthly, in the price card's currency; `approximate` for a vendor preset or a stream (selectors overlap); `disk_bytes` on a self-hosted card |
| `risk_if_wrong` | The strongest evidence class among its readers (sweeps excluded), the dormant flag and the owner's tier |
| `assumptions` | Every gap in the sources that could have shown a reader, assumed references with their reasons, and the stream caveats |
| `owner` | From `tvt.yaml` ownership |
| `status` | `open`, `confirmed` or `dismissed` (with `dismissed_reason`); owner decisions arrive with S9, so every row is `open` for now |
| `score` | Usage weight, strongest evidence, confidence, priority |

## The header

`scan` (ID, start, window, log coverage, price card), `sources` (every evidence source's coverage), `gaps` (every source that is unconfigured, partial or unproven), `totals` (candidates, recommendations, dormant signals, metric savings, and stream savings apart, since stream selectors overlap), `held`.

Readers check `version` (now 1) and refuse one they don't know.
