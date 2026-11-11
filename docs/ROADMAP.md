# tvt roadmap

`tvt` finds telemetry that costs money but nobody uses, and opens a reviewed pull request to drop, downsample or reroute it. This roadmap shows what is built, what comes next, and the principles that won't change. Dates are targets, not promises.

Last updated: November 2026.

## Principles

- **Read-only.** `tvt` never writes to your backends. Every change is a pull request a human reviews and merges.
- **Fail closed.** If `tvt` can't prove a signal is unused, it keeps it. Alerts, recording rules, SLOs and viewed dashboards are hard keeps.
- **Runs on your side.** A single binary. Reports and the usage graph stay on your machine; `tvt` talks only to the backends you configure and your Git host.
- **Evidence, not guesses.** Every recommendation shows why: who reads the signal, how often, and what it costs. Gaps in the evidence are listed, not hidden.
- **Open standards.** OpenTelemetry for `tvt`'s own traces, metrics and logs; OTel Collector, Alloy and Prometheus config for proposals.

## Now: MVP (target: Q1 2027)

The MVP covers the Grafana stack, self-hosted (OSS) and Grafana Cloud.

| Command | What it does | Status |
| --- | --- | --- |
| `tvt scan` | Builds a usage graph from Prometheus/Mimir cardinality, Prometheus, Mimir and Loki rules, Grafana dashboards and dashboard views, and query logs | In progress |
| `tvt report` | Ranks signals by monthly cost and confidence; HTML and JSON | Planned |
| `tvt propose` | Writes Prometheus `metric_relabel_configs`, OTel Collector `filter`/`transform` or Alloy config into the pipeline's repo, one PR per repo | Planned |
| `tvt check` | CI gate: fails a config PR that would break an alert, SLO or viewed dashboard, or that lacks an acknowledged evidence gap | Planned |

MVP scope in more detail:

- **Usage evidence:** PromQL and LogQL reference extraction (matchers, `by`/`without`, `on`/`ignoring`, recording-rule chains); Grafana-managed alert and recording rules; annotation queries; query logs.
- **Grafana OSS and Grafana Cloud.** On OSS, dashboard views come from Grafana's router log and query use from Prometheus, Mimir and Loki query logs. On Grafana Cloud, the equivalents are Usage Insights, Adaptive Metrics usage data and the Mimir cardinality API. Where an edition can't provide a signal, `tvt` records the gap and keeps the data.
- **Cost:** a price card you control (per series, per GB, per span), with vendor presets marked approximate.
- **Logs:** stream-level volume and drops. Line-level reduction candidates are handed off to zeroreads.
- **Alerts and rules:** each rule's cost per day, plus findings with a recommended fix: Grafana-managed alerts that bypass the datasource ruler, LogQL alerts that parse raw logs every interval, range windows far longer than the interval, broad selectors, duplicate rules, recording rules nobody reads, rules that silently return nothing, and rule groups slower than their interval. Report only; `tvt` never edits rules.
- **Safety:** a protection manifest per pipeline repo, an error-pattern guard on every generated log rule, and a least-lossy action order (downsample or reroute before drop; never drop warn and above).
- **Supply chain:** signed, reproducible releases; govulncheck and gitleaks in CI.

## Next: continuous mode (target: Q2 2027)

- Scheduled scans with trend history.
- New waste flagged within 24 hours, and release-aware regressions ("release X added `user_id`, +5,000 series").
- Cost by team or service from `team`/`service`/`namespace` labels and an ownership map.
- Slack and webhook alerts.
- `tvt verify`: re-check sources on a schedule and fail on a new reader of dropped data or on config drift.
- SLO files (Sloth, Pyrra, OpenSLO) as hard keeps; AI agent and MCP query logs as usage evidence.
- A read-only MCP server over the usage graph (`why_kept`, `consumers_of`, `savings_if_dropped`).

## Later: more backends (target: mid 2027)

Backends are added where a cut saves users the most: high list price per unit, billed on volume, with usage evidence `tvt` can read.

1. Datadog (custom metrics, indexed logs, indexed spans)
2. AWS CloudWatch (custom metrics, logs)
3. Azure Monitor Logs
4. Splunk Cloud Platform logs

Then, driven by demand: New Relic, Sumo Logic, Google Cloud Logging, Dynatrace, Honeycomb and Elastic. Tempo and Jaeger traces with tail-sampling output, and Vector and Cribl as pipeline targets.

Cheap per-GB OpenTelemetry-native backends are covered through OTel Collector proposals without a dedicated collector.

## Further out: policy and governance

- Telemetry budgets per team.
- `tvt check` on instrumentation PRs, before new telemetry ships.
- PII detection in span attributes and log fields.

## Get involved

`tvt` is Apache 2.0. We're looking for a few teams to run early scans and tell us where the recommendations are wrong, especially SRE, observability and FinOps folks. Open an issue or start a discussion.
