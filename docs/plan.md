# Telemetry Value Tracker: Development Plan

> Exported from the claude.ai Docs version of the plan, last updated Nov 8, 2026. The Docs version is the live copy; re-export after major changes.

## Summary and assumptions

Build the MVP CLI (`scan` → report → `propose` → `check`) in 8 working weeks, starting Dec 1. Before that, spend 3 weeks in November clearing the unknowns that could wreck the schedule. Only one gate matters for the MVP: a design partner merges a PR that `tvt` generated, and the next scan shows measured savings.

Assumptions:

- One full-time engineer (Jason), working with AI coding help, and no second engineer before Phase 2.
- The holidays cost about a week (Dec 22 – Jan 2), so 8 working weeks take 10 calendar weeks. The MVP gate lands around **Feb 6**, about a week later than the spec's roadmap says.
- The scoring engine is a Go library from day one, so Legacy Bridge can import it.
- `tvt` is the working name until the real name is chosen. Naming does not block any code.

## Decisions to settle before coding

| Decision | Recommendation | Why | By |
| --- | --- | --- | --- |
| Standalone or part of Legacy Bridge | One repo, with the engine as `pkg/` libraries that Legacy Bridge imports | One scoring engine and no fork. The open CLI is the funnel. | Now |
| License boundary | Apache 2.0 on the CLI repo from the first commit. Team-edition code goes in a separate private module. | Changing the license later is painful | Now |
| First partner backend | Use whatever the first signed partner runs. Build against self-run Prometheus and Mimir in the lab, with collectors behind interfaces. | Both have cardinality APIs. Grafana Cloud has more restrictions, so it comes second. | Nov 30 |
| Dashboard view stats on OSS Grafana | Spike in Phase 0 (resolved Nov 8; see below) | Affects confidence, not safety | Nov 20 |
| How `tvt check` gets usage data in CI | `scan` writes a protection manifest (`.tvt/protect.json`) that is committed to the config repo. The check runs offline and fails if the manifest is older than N days. | CI needs no credentials and runs in seconds | Before Sprint 4 |
| SQLite driver | Pure-Go `modernc.org/sqlite` | Keeps a single static binary with no CGO | Sprint 1 |
| Pricing anchor | Defer to Phase 2. Ask partners about willingness to pay in the meantime. | Doesn't block the MVP | Phase 2 |

## Prior art: zeroreads (reviewed Nov 9)

[zeroreads](https://github.com/Bisman-Singh/zeroreads) (Apache 2.0, v0.1.2, Oct 2026) proves, one log-line template at a time, that no query reads a set of Loki or OpenSearch lines. It then writes pipeline config to cut them. It's logs-only, treats every stored dashboard as a reader whether anyone views it or not, and doesn't rank by cost. The full comparison is in [zeroreads vs. tvt](https://claude.ai/artifact/7CHyVw2724T9u1PQSbfSGv).

Decisions:

1. **Lead with what zeroreads doesn't do:** metrics and cardinality, dashboard-view evidence, ranking by cost and value with a PR gated by `check`, and history across scans.
2. **Don't build line-level log removal** (ADR 0006). For findings like the lab's `/healthz` success lines, the report names the stream, its dominant unread pattern (Loki patterns API) and the estimated savings, and points to zeroreads for the proven cut.
3. **Grafana-managed alert and recording rules become hard keeps in Sprint 2.** Without them, a metric used only by a Grafana alert would rank as unused, which breaks the promise behind `check`.
4. **Borrow its evidence discipline:** exact or assumed references, a marker query that proves the query log is live, evidence gaps that block proposals until acknowledged, least-lossy actions first, parser coverage on a public dashboard corpus, and signed releases.
5. **LogQL parser:** at the start of Sprint 2, check whether copying zeroreads' parser (Apache 2.0, about 1,300 lines, no dependencies, attribution in NOTICE) beats writing ADR 0005's extractor from scratch.
6. **Go version:** move from 1.26 to the newest supported release (zeroreads builds with 1.27), per ADR 0004.
7. **Open question:** integrate with zeroreads (emit its rules for line-level candidates) or just link to it.

> The live plan (claude.ai Docs) also has Phase 0 results for Loki, Mimir and spikes 3–4, Sprint 2 additions and an updated checklist that this export predates. Re-export it to bring this file fully up to date.

## Phase 0: pre-work (Nov 9 – Nov 30)

The aim is to start December with no open questions on the critical path. Partners also need query logging switched on now, because the January scan needs 30 days of history.

### Spikes (1–2 days each)

1. **Dashboard view evidence on OSS Grafana.** Grafana Enterprise and Cloud have usage insights. For OSS, test two things. First, whether datasource queries can be tied back to a dashboard and panel through the identifying headers Grafana sends, as they appear in Prometheus, Mimir or Loki query logs. Second, whether Grafana's own request logs can do the same. The result is either a documented view-evidence source for each Grafana edition, or a decision that OSS has no view signal.
2. **How complete the query logs are.** Prometheus `query_log_file` is off by default, and the Mimir and Loki query-frontend logs each record different fields. For each source, write down what it captures (query text, time, source) and what it costs to run. The output is a partner setup guide. **This is on the critical path:** partners have to enable logging in early December.
3. **Parser dependencies.** The Prometheus `promql/parser` package imports cleanly. Loki's LogQL syntax package brings in a large dependency tree. Measure the effect on build size and build time, then decide whether to import it or vendor a subset.
4. **Price card schema.** Draft the YAML format: price per series, per DPM, per GB ingested, per GB-month retained and per span. Vendor presets are marked as approximate. (Draft in `docs/pricecard.md`.)

### Spike results (Nov 8, tvt-lab)

**Spike 1: dashboard evidence on OSS Grafana**

- Grafana's request log records the user and `/api/ds/query`, but not the dashboard UID or panel ID. The `X-Dashboard-Uid` and `X-Panel-Id` headers are neither logged nor forwarded.
- Prometheus sees the panel queries but logs only Grafana's IP as the client.
- **Decision:** dashboard-level view evidence on OSS comes from Grafana's own request log. Each browser view loads the dashboard (`GET /api/dashboards/uid/:uid`), and that log line carries the user and time. Panel-level usage comes from matching the query log against panel expressions after normalizing Grafana variables (`$__rate_interval`, `$route` and so on).
- **Consequence for partners:** they need Grafana router logging (`GF_SERVER_ROUTER_LOGGING=true`) shipped somewhere tvt can read, Loki for example. Enterprise/Cloud usage insights stay the preferred source where they're available.
- Confirmed log line format (logfmt): `uname=admin t=… method=GET path=/api/dashboards/uid/shop-overview status=200 … handler=/api/dashboards/uid/:uid`.

**Spike 2: query-log completeness (Prometheus)**

- The query log is JSON lines with the query text, start, end, step and timing.
- Rule evaluations carry a `ruleGroup` field. API queries carry `httpRequest` (client IP, path).
- **Decision:** exclude entries that have `ruleGroup`, because rules are already counted through the rules API. Treat `httpRequest` entries from Grafana's IP as dashboard or Explore traffic, and everything else as other API clients.
- Still to check: Loki query-frontend stats, Mimir query-frontend logs, and the log's performance overhead at volume.

### Design partners

Recruit 2–3 and have at least one committed by Nov 30. Each partner is asked for:

- read-only credentials,
- query logging and Grafana router logging turned on now,
- one config repo where PRs can land,
- a 30-minute call every two weeks.

Start their security-review paperwork early. The pitch that helps here: the tool runs entirely in their network and data never leaves.

### Lab stack

**Done (Nov 8):** the lab is a separate repo, `tvt-lab`, running on Podman Compose (not kind). It runs Prometheus, Loki, Grafana, Alertmanager and node-exporter, with Mimir optional. A seeded `shop` app emits known waste: unused cache metrics, high-cardinality feature-flag counters, a `user_id` label, and a debug log stream that makes up about 86% of log bytes. It also has known dependencies: alerts, a recording-rule chain feeding an alert, an alert-only metric, a Loki audit alert, and a viewed dashboard alongside one nobody views. A `viewer` service simulates dashboard views. `ground-truth.yaml` is the expected result for golden tests. It has been collecting history on Jason's Mac since Nov 8.

## Architecture and repo layout

The flow is collectors → usage graph → cost → scoring → report and propose. `check` runs separately, against a manifest.

```
tvt/
  cmd/tvt/           CLI (stdlib flag): scan, report, propose, check
  pkg/collect/       interfaces + grafana/, prometheus/, mimir/, loki/
  pkg/parse/         PromQL and LogQL reference extraction
  pkg/graph/         signal/reference model, SQLite store
  pkg/cost/          price card; cardinality and volume -> $/month
  pkg/score/         usage weight, confidence, priority (shared with Legacy Bridge)
  pkg/propose/       templates: Prometheus relabel, otelcol filter/transform
  pkg/check/         config evaluator -> affected signals -> verdict
  pkg/vcs/           GitHub / GitLab PRs
  pkg/report/        embedded HTML template + JSON
  testdata/          recorded lab fixtures; golden graphs and expected recommendations
  action/            GitHub Action + GitLab CI template for tvt check
```

**Core data model**

- **Signal**: a metric, label, log stream or span attribute.
- **Consumer**: an alert rule, recording rule, dashboard panel or query source, with its owning team.
- **Reference**: a link from a signal to a consumer, with its kind, when it was last seen and who used it.
- **Cost**: the series count or bytes for a signal, its $/month, and when that was observed.
- **Recommendation**: the action, target, savings, confidence, evidence chain and blast radius.

**Design rules**

- Read-only everywhere.
- Every collector sits behind an interface, and the lab's API responses are recorded as fixtures, so tests never need a live stack.
- The JSON report is the internal API: the HTML report and `propose` both read from it.
- Every recommendation carries its evidence.

## MVP sprint plan (Dec 2026 – Feb 2027)

| Sprint | Dates | Build | Done when |
| --- | --- | --- | --- |
| S1 | Dec 1–12 | Repo, CI and release pipeline (goreleaser); graph schema; collectors for Prometheus/Mimir cardinality, the rules API (Prometheus/Mimir/Loki) and Grafana dashboard search + JSON | `tvt scan` fills SQLite with signals, rules and dashboards from the lab |
| S2 | Dec 15–19 and Jan 5–9 | PromQL/LogQL reference extraction covering matchers, `by`/`without`, `on`/`ignoring` and recording-rule chains; query-log ingestion; hard-keep classification | Every dependency seeded in the lab resolves to the right signal, and no hard-keep is misclassified |
| S3 | Jan 12–23 | Cost model and price card; Loki bytes per stream; scoring and confidence; HTML + JSON report of the top 50 | **Goal 1:** first report in under 30 minutes on the lab. First read-only scan at the design partner, then walk them through the report. |
| S4 | Jan 26 – Feb 6 | `propose` (Prometheus `metric_relabel_configs`, otelcol `filter`/`transform`); PRs via GitHub/GitLab; `tvt check` with the protection manifest and GitHub Action; savings ledger (before/after per PR) | **MVP gate:** the partner merges a `tvt` PR and the next scan records measured savings |
| Buffer | Feb 9–13 | Fixes from partner feedback, docs, preparing the open-source launch | Public v0.1 release |

If the schedule slips, cut in this order:

1. GitLab support (ship GitHub only).
2. otelcol output (ship Prometheus relabel only).
3. The polished HTML report (ship JSON plus Markdown).

Never cut `tvt check`. It is the safety promise.

## The hard technical problems

Most of the engineering risk is concentrated in these five problems. Budget time for them on purpose.

1. **Blast radius of dropping a label.** Dropping a label is riskier than dropping a metric. A query breaks if it:
   - matches on the label (`{pod="x"}`),
   - groups by it (`by (pod)`),
   - joins on it (`on(pod)`).

   A query using `without(pod)` doesn't break, but its meaning quietly changes. The drop can also merge distinct series into duplicates, which the TSDB rejects or which distort counters. Approach: extract label-level references from the AST. Only recommend a label drop when the remaining labels still identify every series uniquely, checked by comparing cardinality before and after the drop.
2. **Regex and dynamic references.** These include `{__name__=~"http_.*"}`, Grafana template variables (`$job`, `label_values()`) and library panels. Approach: expand regexes against the real list of metric names, and resolve dashboard variables to every possible value. Anything that can't be resolved counts as referenced, with lower confidence.
3. **Absence of evidence.** "Unused" is only as reliable as the logs behind it. Approach: confidence reflects the length of the observation window and how much each source covers, and nothing is recommended from a window shorter than 30 days by default. The report header shows the coverage.
4. **Evaluating the config in `tvt check`.** Approach: build an evaluator that applies relabel regex semantics and OTTL conditions to the signal set in the manifest, instead of guessing what the config intends. If a config can't be evaluated, the check fails closed.
5. **Hidden consumers.** Remote-write targets, federation and external tools never show up in logs. Approach: let users declare external consumers in `tvt.yaml`, which turns them into hard-keeps.

## Testing and validation

- **Golden lab fixture.** The seeded waste and dependencies, with expected results (`tvt-lab/ground-truth.yaml`), run on every PR. Two assertions never change: no hard-keep is ever recommended, and every piece of seeded waste is found.
- **Query corpus.** Use real PromQL and LogQL as parser test cases: kube-prometheus mixins, community dashboards, and partner queries tested on their side. Cover every edge case listed above.
- **Recorded collector fixtures.** Capture API responses from the lab (`scripts/capture-fixtures.sh`) so unit tests run fast and offline.
- **Property tests for `check`.** Generate random relabel configs. Whenever the evaluated drop set overlaps the protected set, `check` must fail.
- **Partner precision review.** The partner marks each of the top 50 candidates as safe or unsafe. Target: at least 90% marked safe before running `propose` with them.
- **Scale test.** Use synthetic cardinality (for example, Prometheus `avalanche`) to confirm scan time and memory stay reasonable at a few million series.

## Phases 2–4

| Phase | Dates | Build | Gate |
| --- | --- | --- | --- |
| 2: Continuous mode | Feb 16 – Mar 2027 | `tvt serve` on Postgres; scheduled scans and trend history; new-waste detection within 24 hours (diffing cardinality snapshots); team attribution from `team`/`service`/`namespace` labels plus an ownership map; Slack/webhook alerts. Start the private Team-edition module (SSO, history). **Pull SLO-file hard-keeps (Sloth/Pyrra/OpenSLO) and agent/MCP query logs forward into this phase:** SLO hard-keeps protect alerting, and agent query logs close a usage blind spot. | 3 partners running it weekly |
| 3: More backends | Apr – May 2027 | Tempo/Jaeger traces and tail-sampling output; Elastic and Splunk logs; Datadog metrics via API; Vector and Cribl output. New collectors use the existing interfaces. | First paid contract. Start pricing talks in Phase 2 so this doesn't stall. |
| 4: Policy and governance | Jun 2027 on | Telemetry budgets per team, `check` on instrumentation PRs, PII detection in span attributes and log fields | |

## Risks to the plan

| Risk | Mitigation |
| --- | --- |
| No partner turns on query logging in time, so the January scan has thin evidence | Ask in November. Ship a "rules + dashboards only" mode that works with low confidence. |
| Partner security review holds up read-only access | Start the paperwork in Phase 0. Lean on the facts that it's a single binary run on their side and that reports stay local. |
| Building alone while job-hunting | Each sprint ends with something that works. Keep the cut list above handy. |
| OSS Grafana exposes no view data | Resolved: use Grafana router logs for dashboard loads. Where those aren't available, treat every dashboard as referenced, which is safer but finds fewer savings, and say so in the report. |
| The Loki parser's dependencies bloat the binary | Resolved by ADR 0005: no Loki code (AGPL). Own extractor, or zeroreads' Apache-2.0 parser with NOTICE attribution |
| Label-drop edge cases cause a bad recommendation | `check` fails closed. Recommend routing to cold storage over deletion. Label drops ship behind a flag until partner precision is proven. |

## Immediate next steps

- [x] Create the `tvt` repo (Apache 2.0, skeleton, CI, scoring and cost models with tests)
- [x] Build the lab (`tvt-lab`, Podman) and seed the known waste
- [x] Spike 1 (Grafana view evidence) and spike 2 for Prometheus
- [ ] Push `tvt` and `tvt-lab` to the Opsmastery-Summit GitHub org
- [ ] Run `scripts/capture-fixtures.sh` and commit the fixtures
- [ ] Spike 2 for Loki and Mimir, spike 3 (LogQL dependencies), spike 4 (price card)
- [ ] Draft the partner ask and the guide to enabling query logging, and send them to 3–5 contacts
