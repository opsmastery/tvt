# 0009. Fluent Bit and exporter pipeline kinds; cut at the source first

Date: 2026-11-16 · Status: accepted (Nov 16) · Amends ADR 0007

## Context

ADR 0007 declared three pipeline kinds: `prometheus`, `alloy` and `otelcol`. Two common places where telemetry is shaped were missing.

- **Fluent Bit.** Many Kubernetes shops ship logs with Fluent Bit, so a Loki or Elastic drop often belongs there, not in Alloy.
- **The source.** The cheapest series are the ones never emitted. A node_exporter collector or a kube-state-metrics metric that nobody reads costs scrape time, network and storage. A downstream drop only saves the storage.

## Decision

1. **`fluentbit` is a pipeline kind for logs.** `propose` writes `grep` `Exclude` filters for streams and lines, `modify` and `record_modifier` to remove fields, and `rewrite_tag` with a second output to reroute a stream to cheaper storage. It handles classic `.conf`, YAML config, the fluent-bit Helm chart's values, and Fluent Operator `ClusterFilter`/`ClusterOutput` resources. Lua and WASM filters are opaque: `check` fails closed when one sits on a stream tvt changes.
2. **`exporter` is a pipeline kind for source settings.** It covers OSS exporter flags (node_exporter `--no-collector.<name>`, kube-state-metrics `--metric-denylist` and `--metric-labels-allowlist`, collector flags on common database and blackbox exporters) and OTel Collector receiver settings (hostmetrics scrapers, `k8s_cluster`, `kubeletstats`, and per-metric `enabled: false` in contrib receivers).
3. **Each supported exporter has a metric catalog:** the metrics every flag or scraper produces, generated from its docs and verified in tvt-lab. `check` treats a disabled collector as a drop of every series in its catalog entry, so the usual hard-keep rules apply.
4. **Source first.** When a source change and a downstream drop both keep every reader working, `propose` prefers the source and says why in the PR: it also saves scrape and network cost. A high-cardinality label created by Collector config (a `spanmetrics` dimension, an `attributes` or `transform` processor) is changed in that component, not dropped later.
5. **Rollout.** Fluent Bit is in the MVP (S7 writer, S8 `check`). Exporter catalogs and receiver settings are Phase 3, starting with node_exporter and kube-state-metrics.

## Consequences

- The partner ask lists Fluent Bit configs and exporter deployment manifests alongside scrape, Alloy and Collector config.
- Every exporter catalog is upkeep: an exporter release can add metrics to a collector. A catalog entry records the exporter version it was checked against, and an unknown version is an evidence gap.
- tvt-lab needs a Fluent Bit container that tails the shop's debug log (`docs/tasks/lab-backlog.md`, S7).
