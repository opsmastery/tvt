# Task lists

From Sprint 3 on, sprints are two working days, each with one theme. They're ordered to keep the riskiest promise safe: the report first, the benchmark in CI before any writer exists, and the `check` safety gate before owners can confirm drops. Each sprint is done when its "done when" holds on the lab.

| Sprint | File | Theme | Done when | Status |
| --- | --- | --- | --- | --- |
| 1 | [sprint 1](2026-11-sprint1-remainder-and-test-groundwork.md) | Collectors, store, `tvt scan`, test groundwork | Done | Done Nov 10 (`v0.0.1-rc.1`) |
| 2 | [sprint 2](2026-11-sprint2.md) | References, query logs, hard keeps, SLOs and runbooks | Every seeded dependency resolves and no hard keep is misclassified | Done Nov 16; open: items 8–10 (more Grafana evidence, self-observability, `tvt init` and `SECURITY.md`, due by S12), 13, and item 11's Adaptive Metrics usage counts |
| S3 | [s03](s03-cost-and-scoring.md) | Cost and scoring core | Every lab signal has a monthly cost on two price cards; the simulated agent sweep keeps nothing on its own | Done Nov 14 |
| S4 | [s04](s04-report-v1.md) | Report v1 | First report in under 30 minutes on the lab | Done Nov 14 |
| S5 | [s05](s05-benchmark-and-log-findings.md) | Benchmark and log findings | Benchmark baseline recorded; a deliberately broken parser change fails CI | Done Nov 15 |
| S6 | [s06](s06-rules-query-cost-slos-pipelines.md) | Rules, query cost, SLOs, pipelines | All rule and pipeline findings match ground truth; the report runs on the Grafana Cloud test stack | Done Nov 16 (item 6 on the Grafana Cloud test stack, `docs/grafana-cloud.md`) |
| S7–S12 | [s07–s12](s07-s12-propose-check-owners-release.md) | Writers, `check`, owner workflow, reduce before drop, savings ledger, release candidate | `v0.1.0-rc1` tagged; the MVP gate rehearsed on the lab | Next. Before S7: ADRs 0009–0012 and the Alloy parsing ADR, tvt-lab's S7 seeds |
| S13–S16 | [s13–s16](s13-s16-alert-noise-agents-collector.md) | Continuous-mode work pulled forward: alert noise, agents and history, Collector and conventions | Each sprint's seeded cases reported correctly | Planned |

Lab seeds for every sprint are collected in [lab-backlog.md](lab-backlog.md), so tvt-lab and its `ground-truth.yaml` can run a sprint ahead of the code.

**Rules for every item:** one branch and one PR per numbered item, `--base main`. Tests first or alongside. Keep the read-only guarantee against backends (`internal/testutil` fails any non-GET); the only writes are Git branches and PRs in mapped pipeline repos. Update `testdata/lab/ground-truth.yaml` when a lab seed changes. Update the Status section of `CLAUDE.md` in each PR. Nothing from internal planning (pricing, partners, schedules) goes in the repo.

**Never cut `check`.** It is the safety promise. When a sprint runs long, cut from its "should have" items, then push its last must-have to the next sprint.
