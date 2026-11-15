# tvt roadmap

`tvt` finds telemetry that costs money but nobody uses, and opens a reviewed pull request to drop, downsample or reroute it. This roadmap shows what is built, what comes next, and the principles that won't change. Dates are targets, not promises.

Last updated: November 12, 2026.

## Principles

- **Read-only.** `tvt` never writes to your backends. Every change is a pull request a human reviews and merges.
- **Fail closed.** If `tvt` can't prove a signal is unused, it keeps it. Alerts, recording rules, SLOs and viewed dashboards are hard keeps. A query `tvt` can't parse, or a data source it doesn't read yet, counts as a reader, never as "no reader".
- **Records are never cut.** Audit logs, deploy and change events, Kubernetes audit and agent tool-call spans are records: never sampled, collapsed or dropped.
- **Runs on your side.** A single binary. Reports and the usage graph stay on your machine; `tvt` talks only to the backends you configure and your Git host.
- **Owners decide.** Every drop is a draft PR assigned to the signal's owner and moves forward only after they confirm. The confirmation is bound to the evidence they saw: if the evidence changes, it's asked again. Drops are staged through a cheap archive first, with a revert ready. Rarely used is treated as dormant, not unused: the metric nobody touched all year may be the one you need at 3am.
- **Reduce before drop.** The least-lossy action that keeps every reader working wins: remove a label or bucket its values, aggregate, downsample or shorten retention, scrape less often, collapse repeats with a count, sample, reroute, and only then drop.
- **Evidence ranks.** Declared dependencies (an SLO, alert, recording rule or runbook step evaluates the signal) are strongest, then incident evidence (queried during an incident, promoted from postmortems), then observed usage (views, query logs). Only observed usage decays. A sweep that queries everything (an agent or a person browsing every metric) is weak evidence and keeps nothing on its own.
- **Evidence, not guesses.** Every recommendation shows why: who reads the signal, how often, what it costs, and what stays protected. Gaps in the evidence are listed, not hidden.
- **A claim you can disprove.** Every signal `tvt` marks unread had no reader in your evidence window, and every gap is listed. If you find a reader we missed, that's a bug, and the benchmark gets a new case.
- **Open standards.** OpenTelemetry for `tvt`'s own traces, metrics and logs; OTel Collector, Alloy, Prometheus and Fluent Bit config for proposals.

## Now: MVP (target: Q1 2027)

The MVP covers the Grafana stack, self-hosted (OSS) and Grafana Cloud.

| Command | What it does | Status |
| --- | --- | --- |
| `tvt scan` | Builds a usage graph from Prometheus/Mimir cardinality, Prometheus, Mimir, Loki and Grafana-managed rules, Grafana dashboards and dashboard views, query logs, SLOs and runbooks, and classifies every signal | In progress |
| `tvt report` | Ranks recommendations by savings per minute of checking; HTML and JSON | Planned |
| `tvt why <signal>` | Every reader of a signal, with its source, first and last seen, and whether it's exact or inferred | Planned |
| `tvt init` | Asks for each backend, tests the URL, token scope and log settings, and writes `tvt.yaml` | Planned |
| `tvt propose` | Writes Prometheus `metric_relabel_configs`, Prometheus Operator `ServiceMonitor`/`PodMonitor` `metricRelabelings`, Alloy `prometheus.relabel`/`loki.process`, OTel Collector `filter`/`transform` or Fluent Bit filters into the pipeline's repo, one PR per repo | Planned |
| `tvt check` | CI gate: fails a config PR that would break an alert, SLO, viewed dashboard or record, or that lacks an acknowledged evidence gap | Planned |

MVP scope in more detail:

- **Declared dependencies:** SLO definitions (Sloth, Pyrra, OpenSLO, Grafana SLO) and alert runbook links, including runbook steps that run a query, are hard keeps. So are `absent()` alerts, and `tvt.yaml` declares external consumers (remote write, federation, outside tools).
- **Usage evidence:** PromQL and LogQL reference extraction (matchers, `by`/`without`, `on`/`ignoring`, recording-rule chains), with MetricsQL-only syntax read as an assumed reference; Grafana-managed alert and recording rules; annotation queries; proven query logs, with each query tagged by client (dashboard, person in Explore, rule, agent, other API).
- **Grafana OSS and Grafana Cloud.** On OSS, dashboard views come from Grafana's router log and query use from Prometheus, Mimir and Loki query logs. On Grafana Cloud, the equivalents are Usage Insights, Adaptive Metrics usage data and the Mimir cardinality API. Where an edition can't provide a signal, `tvt` records the gap and keeps the data.
- **Cost:** a price card you control, with vendor presets marked approximate and what-if pricing at another vendor's list price. A self-hosted kind turns series into disk from measured bytes per sample, retention and replicas, and separates the fixed floor from what scales with data, so a cut that frees disk but no nodes says so. Histogram cost shows the bucket multiplier. Rules and queries carry their measured cost too.
- **Recommendations you can check:** each one states a testable claim, the cheapest way to confirm it and how many minutes that takes, every downstream reader, the risk if it's wrong, and the expected end state after the merge. The report is ranked by savings per minute of checking, shows what stays protected per service, and maps each signal to the pipeline that would change it, or shows it as unmapped.
- **Logs:** stream-level volume and drops, a repeated-line finding, and level-based tiers (lines below warn to the cheapest tier when nothing reads them). Line-level reduction candidates are handed off to zeroreads.
- **Alerts and rules:** each rule's cost per day, plus findings with a recommended fix: Grafana-managed alerts that bypass the datasource ruler, LogQL alerts that parse raw logs every interval, range windows far longer than the interval, broad selectors, duplicate rules, recording rules nobody reads, rules that silently return nothing, and rule groups slower than their interval. An SLO coverage report and each alert's worst-case detection delay. Report only; `tvt` never edits rules.
- **Dormant, not unused:** unread signals that look incident-critical (error, latency and saturation signals, tier 0/1 services, runbook mentions) or belong to a seasonal workload (quarter-end, peak season, audits) are labelled dormant and never ranked as drop candidates. Last-read dates are kept indefinitely.
- **Safety:** a protection manifest per pipeline repo; every generated config validated by the target's own tool before the PR; generated OTTL limited to stable v1 syntax with safe defaults and the target Collector's version; the OTTL already in your Collectors is read, so routing, sampling and filter conditions protect the attributes they use and connector-derived metrics protect their inputs; protected queries replayed with and without a label before it's removed; retention cuts refused past the longest lookback any reader uses; scrape interval changes refused past an alert's detection objective; collapsed logs refused until count queries are rewritten; an error-pattern guard on every generated log rule; owner confirmation before a drop PR leaves draft; staged drops (archive, then drop after a quiet period) and a generated revert PR.
- **Owner workflow:** owners from CODEOWNERS, team labels, Backstage `catalog-info.yaml` and an ownership map. Every recommendation is open, confirmed or dismissed with a reason; a dismissal records a keep and a review date in the manifest, and a decision is flagged for re-review when its evidence changes. Each owner gets a capped queue and a weekly digest.
- **Measured savings:** a ledger of before and after for every merged PR, with savings separated from bill growth by cause, and GiB freed and retention days gained for self-hosted backends.
- **A benchmark in CI:** lab snapshots with known answers, scored on every change: evidence found and recommendations right, as separate scores. A drop in either fails the build.
- **Supply chain:** signed, reproducible releases; govulncheck and gitleaks in CI.
- **Security review:** `SECURITY.md` documents what `tvt` reads, where data goes (only your backends and Git host) and what stays on disk.

Some continuous-mode work lands right after the MVP's code is complete, before v0.1: alert-noise findings, the MCP server and point-in-time reports, and OpenTelemetry Collector checks.

## Next: continuous mode (target: Q1–Q2 2027)

- A server mode (`tvt serve`): one binary plus Postgres, with scheduled scans and trend history.
- New waste flagged within 24 hours, and release-aware regressions ("release X added `user_id`, +5,000 series"). Readers broken by a release: a panel or rule whose metric, label or stream disappeared or was renamed between scans.
- Cost by team or service from `team`/`service`/`namespace` labels and an ownership map.
- Slack and webhook alerts.
- `tvt verify`: re-check sources on a schedule, fail on a new reader of dropped data or on config drift, and confirm each merged change reached its expected end state by querying the backend directly.
- AI agent and MCP query logs as usage evidence; agent skill files and alert-triggered workflows that run queries are declared readers.
- Incident evidence: read incident timelines (PagerDuty, incident.io, Grafana IRM, Opsgenie) and postmortems, and promote anything queried during an incident into the protection manifest through a reviewed PR that links the postmortem. Protection doesn't expire. `check` holds a drop while an incident is open on the owning service or during a declared change freeze.
- A read-only MCP server over the usage graph (`why_kept`, `consumers_of`, `savings_if_dropped`), an agent skill that teaches agents to use it, and a contract test that the CLI, report and MCP give the same answers.
- Point-in-time reports (`tvt report --as-of <scan>`): what `tvt` knew when an owner approved a change.
- Alert noise, report only with ready-to-apply snippets: static alert lint (runbook present and fresh, owner, severity, debounce), thresholds against a 14-day baseline, firing-history classes (transient, during maintenance, co-firing duplicates, silent for 90 days), alerts per incident, delivery failures, overlapping wildcard and specific alerts, and findings rolled up by alert template.
- Triage readiness: owner and deploy labels, `service.name`, version and environment coverage, and trace IDs in logs.
- OpenTelemetry Collector checks: deprecated `batch` processor, debug exporter left on, `memory_limiter` placement, agent/gateway version skew, retired OTTL feature gates, risky OTTL statements, OTTL that depends on experimental or lambda functions or removed gates, and the component that created a high-cardinality label. A rename-aware metric catalog for semantic-convention migrations; logs read only by counts become count-connector metrics; spanmetrics span-name normalization and unread dimensions removed.
- `tvt propose --dry-run`: run generated processors over a recorded sample and report records in and out before merge.
- Storage follow-ups: local retention under remote write, series churn by label, double-shipped logs, and replicated bytes in self-hosted Loki.
- Verification metrics in `tvt`'s own telemetry: time from draft PR to owner decision, and how often owners dismiss a recommendation.

## Later: more backends and signals (target: Q2–Q3 2027)

Backends are added where a cut saves users the most: high list price per unit, billed on volume, with usage evidence `tvt` can read.

1. Datadog (custom metrics, indexed logs, indexed spans)
2. AWS CloudWatch (custom metrics, logs)
3. Azure Monitor Logs
4. Splunk Cloud Platform logs

Also planned:

- Traces: Tempo and Jaeger, span volume by route, span pruning before sampling, and tail-sampling or probabilistic-sampling proposals that always keep errors, slow requests and spans that change something (deploys, rollbacks, agent tool calls).
- Profiles: Pyroscope as a signal, filter proposals in a gateway Collector, and telemetry overhead measured from profiles.
- VictoriaMetrics and VictoriaLogs (query-count usage evidence, TSDB status cardinality, vmalert rules).
- Vector and Cribl as pipeline targets, and OBI (zero-code eBPF instrumentation) as a source.
- Source-level changes: exporter flags and allow/deny lists (node_exporter, kube-state-metrics, common database exporters) and OTel Collector receiver settings, with a catalog of each exporter's metrics.
- A Backstage plugin showing each service's telemetry cost, unread signals and open recommendations.
- Time-boxed detail (`tvt propose --temporary 7d`): raise detail for one question and open the revert PR at the same time.

Then, driven by demand: New Relic, Sumo Logic, Google Cloud Logging, Dynatrace, Honeycomb and Elastic. Cheap per-GB OpenTelemetry-native backends are covered through OTel Collector proposals without a dedicated collector.

## Further out: policy and governance

- Telemetry budgets per team.
- `tvt check` on instrumentation PRs, before new telemetry ships, including the series a new spanmetrics dimension would add.
- PII detection in span attributes and log fields.
- LLM and agent observability backends (Langfuse, Phoenix, Laminar): sample low-value traces and truncate large payloads, while agent tool-call records stay intact.

## Get involved

`tvt` is Apache 2.0. We're looking for a few teams to run early scans and tell us where the recommendations are wrong, especially SRE, observability and FinOps folks. Open an issue or start a discussion.
