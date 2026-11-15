# The report

`tvt report` reads the latest scan in the database and writes the JSON report. The JSON report is the internal API: the HTML report (S4 item 2), `propose`, `check` and later the MCP server read it, never the database.

```sh
tvt scan ... --db tvt.db
tvt report --db tvt.db --json report.json --top 50   # --json - for stdout, --top 0 for all
```

`tvt report` never creates a database and never talks to a backend.

## What gets recommended

Only `candidate` signals with a score above zero confidence. Hard keeps, keep_high and dormant signals never are. Candidates tvt can't trust yet are counted under `held` by reason: `window` (the scan's window is shorter than `window_days`), `coverage` (no usage log could show the signal read), `new` (it first appeared in this scan). A label whose metric is recommended for a drop is left out, because the drop covers it. So is `loki:stream:*`, the stand-in for selectors tvt couldn't read.

| Signal | Action |
| --- | --- |
| Metric | `drop` |
| Label | `label_removal` |
| Log stream | `reroute`: to cold storage or shorter retention, never line-level (ADR 0006) |

`downsample` and `finding` are reserved for S5 and S6.

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

`scan` (ID, start, window, log coverage, price card), `sources` (every evidence source's coverage), `gaps` (every source that is unconfigured, partial or unproven), `totals` (candidates, recommendations, metric savings, and stream savings apart, since stream selectors overlap), `held`.

Readers check `version` (now 1) and refuse one they don't know.
