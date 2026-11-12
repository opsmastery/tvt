# tvt roadmap

`tvt` finds telemetry that costs money but nobody uses, and opens a reviewed pull request to drop, downsample or reroute it. This roadmap shows what is built, what comes next, and the principles that won't change. Dates are targets, not promises.

Last updated: November 2026.

## Principles

- **Read-only.** `tvt` never writes to your backends. Every change is a pull request a human reviews and merges.
- **Fail closed.** If `tvt` can't prove a signal is unused, it keeps it. Alerts, recording rules, SLOs and viewed dashboards are hard keeps.
- **Runs on your side.** A single binary. Reports and the usage graph stay on your machine; `tvt` talks only to the backends you configure and your Git host.
- **Owners decide.** Every drop is a draft PR assigned to the signal's owner and moves forward only after they confirm. Drops are staged through a cheap archive first, with a revert ready. Rarely used is treated as dormant, not unused: the metric nobody touched all year may be the one you need at 3am.
- **Evidence ranks.** Declared dependencies (an SLO, alert, recording rule or runbook step evaluates the signal) are strongest, then incident evidence (queried during an incident, promoted from postmortems), then observed usage (views, query logs). Only observed usage decays.
- **Evidence, not guesses.** Every recommendation shows why: who reads the signal, how often, and what it costs. Gaps in the evidence are listed, not hidden.
- **Open standards.** OpenTelemetry for `tvt`'s own traces, metrics and logs; OTel Collector, Alloy and Prometheus config for proposals.

## Now: MVP (target: Q1 2027)

The MVP covers the Grafana stack, self-hosted (OSS) and Grafana Cloud.

| Command | What it does | Status |
| --- | --- | --- |
| `tvt scan` | Builds a usage graph from Prometheus/Mimir cardinality, Prometheus, Mimir and Loki rules, Grafana dashboards and dashboard views, and query logs | In progress |
| `tvt report` | Ranks signals by monthly cost and confidence; HTML and JSON | Planned |
| `tvt init` | Asks for each backend, tests the URL, token scope and log settings, and writes `tvt.yaml` | Planned |
| `tvt propose` | Writes Prometheus `metric_relabel_configs`, OTel Collector `filter`/`transform`, Alloy `prometheus.relabel`/`loki.process` or Fluent Bit filters into the pipeline's repo, one PR per repo | Planned |
| `tvt check` | CI gate: fails a config PR that would break an alert, SLO or viewed dashboard, or that lacks an acknowledged evidence gap | Planned |

MVP scope in more detail:

- **Declared dependencies:** SLO definitions (Sloth, Pyrra, OpenSLO, Grafana SLO) and alert runbook links, including runbook steps that run a query, are hard keeps.
- **Usage evidence:** PromQL and LogQL reference extraction (matchers, `by`/`without`, `on`/`ignoring`, recording-rule chains); Grafana-managed alert and recording rules; annotation queries; query logs.
- **Grafana OSS and Grafana Cloud.** On OSS, dashboard views come from Grafana's router log and query use from Prometheus, Mimir and Loki query logs. On Grafana Cloud, the equivalents are Usage Insights, Adaptive Metrics usage data and the Mimir cardinality API. Where an edition can't provide a signal, `tvt` records the gap and keeps the data.
- **Cost:** a price card you control (per series, per GB, per span), with vendor presets marked approximate.
- **Recommendations you can check:** each one states a testable claim, the cheapest way to confirm it and how many minutes that takes, every downstream reader, and the risk if it's wrong. The report is ranked by savings per minute of checking. Each signal maps to the pipeline that would change it, or shows as unmapped.
- **Logs:** stream-level volume and drops. Line-level reduction candidates are handed off to zeroreads.
- **Alerts and rules:** each rule's cost per day, plus findings with a recommended fix: Grafana-managed alerts that bypass the datasource ruler, LogQL alerts that parse raw logs every interval, range windows far longer than the interval, broad selectors, duplicate rules, recording rules nobody reads, rules that silently return nothing, and rule groups slower than their interval. Report only; `tvt` never edits rules.
- **Dormant, not unused:** unread signals that look incident-critical (error, latency and saturation signals, tier 0/1 services, runbook mentions) are labelled dormant and never ranked as drop candidates. Last-read dates are kept indefinitely.
- **Safety:** a protection manifest per pipeline repo, an error-pattern guard on every generated log rule, a least-lossy action order (downsample or reroute before drop; never drop warn and above), owner confirmation recorded in the protection manifest before a drop PR leaves draft, staged drops (archive, then drop after a quiet period) and a generated revert PR. Logs can be sampled (keep 1 in N lines) before they're dropped, and each generated rule carries its monthly saving.
- **Owner workflow:** every recommendation is open, confirmed or dismissed with a reason; a dismissal records a keep and a review date in the manifest. Each owner gets a capped queue and a weekly digest.
- **Measured savings:** a ledger of before and after for every merged PR.
- **Supply chain:** signed, reproducible releases; govulncheck and gitleaks in CI.
- **Security review:** `SECURITY.md` documents what `tvt` reads, where data goes (only your backends and Git host) and what stays on disk.

## Next: continuous mode (target: Q2 2027)

- A server mode (`tvt serve`) with scheduled scans and trend history.
- New waste flagged within 24 hours, and release-aware regressions ("release X added `user_id`, +5,000 series").
- Cost by team or service from `team`/`service`/`namespace` labels and an ownership map.
- Slack and webhook alerts.
- `tvt verify`: re-check sources on a schedule and fail on a new reader of dropped data or on config drift.
- AI agent and MCP query logs as usage evidence.
- Incident evidence: read incident timelines (PagerDuty, incident.io, Grafana IRM, Opsgenie) and postmortems, and promote anything queried during an incident into the protection manifest through a reviewed PR that links the postmortem. Protection doesn't expire.
- A read-only MCP server over the usage graph (`why_kept`, `consumers_of`, `savings_if_dropped`).
- Verification metrics in `tvt`'s own telemetry: time from draft PR to owner decision, and how often owners dismiss a recommendation.

## Later: more backends (target: mid 2027)

Backends are added where a cut saves users the most: high list price per unit, billed on volume, with usage evidence `tvt` can read.

1. Datadog (custom metrics, indexed logs, indexed spans)
2. AWS CloudWatch (custom metrics, logs)
3. Azure Monitor Logs
4. Splunk Cloud Platform logs

Then, driven by demand: New Relic, Sumo Logic, Google Cloud Logging, Dynatrace, Honeycomb and Elastic. Tempo and Jaeger traces with tail-sampling output, and Vector and Cribl as pipeline targets.

Changes can also land at the source: exporter flags and allow/deny lists (node_exporter, kube-state-metrics, common database exporters) and OTel Collector receiver settings, with a catalog of each exporter's metrics.

Cheap per-GB OpenTelemetry-native backends are covered through OTel Collector proposals without a dedicated collector.

## Further out: policy and governance

- Telemetry budgets per team.
- `tvt check` on instrumentation PRs, before new telemetry ships.
- PII detection in span attributes and log fields.

## Get involved

`tvt` is Apache 2.0. We're looking for a few teams to run early scans and tell us where the recommendations are wrong, especially SRE, observability and FinOps folks. Open an issue or start a discussion.
