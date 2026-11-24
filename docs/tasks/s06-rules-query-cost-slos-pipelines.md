# S6: rules, query cost, SLOs, pipelines

**Status:** Done Nov 16 (PRs #77–#86; item 6 on the Grafana Cloud test stack, `docs/grafana-cloud.md`).

**Done when:** all rule and pipeline findings match ground truth, and the report runs on the Grafana Cloud test stack. After this sprint tvt is ready for a first read-only scan outside the lab.

Read `docs/tasks/README.md`, `docs/architecture.md`, `docs/query-logs.md` and ADR 0007 first.

## Before you start

- Sprint 2 items 12 (rule-efficiency seeds and the alert-query spike), 14 (pipelines in the lab) and 11 (Grafana Cloud adapters) merged.
- tvt-lab: the overflow series seed from `lab-backlog.md` (S6).

## Must have (in this order)

1. **Cost per rule and rule-efficiency findings** (report only; tvt never edits rules). Cost per rule for Prometheus, Mimir and Loki rulers and Grafana-managed rules: evaluations per day (from Sprint 2 item 16's intervals) × samples (Prometheus query log) or bytes processed (Loki `metrics.go`) per evaluation, joined to the rule as Sprint 2 item 12's spike found. An unknown interval is an unknown cost, never zero. Rank rules by daily cost beside signals; on Grafana Cloud, findings ship without a dollar figure. Findings, each with its fix: Grafana-managed alert on Loki or Prometheus (move it to the datasource ruler or a recording rule); LogQL alert that parses raw logs every interval (Loki recording rule); range window far longer than the interval; broad stream selector or no line filter before a parser; interval shorter than the scrape or ingest interval; duplicate rules; recording rule nothing reads; rule with no series for 30 days (broken, not unused; `absent()` rules excluded: skip any rule whose expression `parse.UsesAbsence` matches); rule group slower than its interval. Golden test against `rule_findings:` in the ground truth.
2. **Measured query cost.** Where a backend reports query cost (Mimir and Loki query stats, Prometheus query-log stats), attach it to the panel or rule that runs the query, split by S3's clients, so rule-efficiency findings carry measured cost and the report shows how much of the query load agents drive.
3. **SLO coverage report.** From the SLOs and rules read in Sprint 2: services with no SLO, paging alerts that aren't burn-rate alerts, and SLOs with no alert. Report only.
4. **Pipeline targets** (ADR 0007). Read `pipelines:` from `tvt.yaml` and match each signal against every pipeline's `handles:` selectors. Exactly one match is its target; none or two is "unmapped", reported with its savings but not proposable, and recorded as a gap. Every report row shows its target pipeline or "unmapped". Golden test against the `pipelines:` section of the lab ground truth.
5. **Overflow series are an evidence gap.** When series with `otel.metric.overflow="true"` exist, real cardinality is higher than the backend shows and those points lost their labels. Flag the metric, mark its cost as a floor, and warn any reader that filters on a capped dimension.
6. **The report on Grafana Cloud.** Done Nov 16 (`docs/grafana-cloud.md`). Run `tvt scan` and `tvt report` against the Cloud test stack in the nightly job from Sprint 2 item 11: costs from the provisioned usage data source's billing metrics and Grafana Cloud list prices (negotiated rates as a `tvt.yaml` override), and the Cloud gaps (Usage Insights missing on the plan, direct Loki API queries) in the header.

## Should have

7. **Label-drop safety.** Only recommend dropping a label when the remaining labels still identify every series uniquely: compare series counts with and without it (Prometheus series API, or Mimir's cardinality API) and refuse the drop if series would merge. Sprint 2 item 13's "always aggregated out" list is the candidate source. Label drops stay behind a flag until a precision review on real data passes.
8. **kube-prometheus-stack as a golden case.** Read the rules, ServiceMonitors and dashboards rendered from the pinned chart (see `lab-backlog.md`) and match a hand-checked list of declared readers. The chart's default kubelet `cAdvisorMetricRelabelings` already drop some series and keep `container_memory_swap` because a recording rule and dashboards use it: tvt must agree on both.
