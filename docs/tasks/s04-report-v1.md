# S4: report v1

Read `CLAUDE.md`, `docs/tasks/README.md`, `docs/architecture.md` and ADRs 0006 and 0007 first.

**Done when:** `tvt report` produces the HTML and JSON report of the top 50 on the lab in under 30 minutes from a cold start, no hard keep, keep_high, dormant or record signal is ever recommended, and every row lists its evidence gaps.

## Must have (in this order)

1. **Recommendation records and the JSON report** (`pkg/report`). The JSON report is the internal API that the HTML report, `propose`, `check` and later the MCP server read. Each recommendation (drop, downsample, reroute, label removal, and each finding) carries: `claim` (one testable sentence), `evidence` (refs, query-log lines, gaps), `cheapest_check` (the fastest way to confirm or refute it, from ownership, runbook and dormant data), `check_minutes`, `downstream` (every reader with its evidence class and source), `savings`, `risk_if_wrong` (evidence class, dormant flag, tier), `assumptions` (gaps, assumed refs), `owner`, and `status` (open, confirmed, dismissed with a reason). Rank by savings per minute of checking.
2. **HTML report of the top 50.** From an embedded template, reading only the JSON report. If time runs short, ship JSON plus Markdown and cut the polished HTML.
3. **"Dormant: confirm".** Signals that runbooks or postmortems only mention (names collected by Sprint 2 item 7) join the name and tier rules. Dormant signals show as "dormant: confirm" rows with the question for the owner, never as drop candidates. Decide the 21 seeded drops the name rules hold back (see `CLAUDE.md`, Sprint 2 item 6) with tvt-lab's ground truth before this lands.
4. **Evidence gaps.** The report header lists coverage per source and every gap (unconfigured source, unproven query log, unreachable runbook, unknown data source, Cloud-only blind spots, assumed refs). `tvt.yaml` acknowledges a gap with a reason; an unacknowledged gap shows on every affected row, and `check` fails on it (S8).
5. **Seasonal readers.** `tvt.yaml` gets `seasons:` (name, months, the services involved). For those services, an evidence window shorter than the longest season is a gap, and unread signals default to dormant, not drop candidates.
6. **"Evidence kept" per service.** Next to what tvt would cut, list what stays protected: hard keeps, keep_high, records, incident evidence and dormant signals, with counts and cost.
7. **`tvt why <signal>`.** Every reader with its source (query log, rules API, dashboard JSON, runbook, SLO file, external consumer, assumed), first and last seen, evidence class, and whether it's exact or inferred. The report links each row to the same view. Last-read dates come from `Store.LastReads` and never decay.
8. **`--price-card`** on `tvt report`, from S3 item 4.
