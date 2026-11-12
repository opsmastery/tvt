# Task lists

From Sprint 3 on, sprints are two working days, each with one theme. They're ordered to keep the riskiest promise safe: the report first, the benchmark in CI before any writer exists, and the `check` safety gate before owners can confirm drops. Each sprint is done when its "done when" holds on the lab.

| Sprint | File | Theme | Done when |
| --- | --- | --- | --- |
| 1 | [sprint 1](2026-11-sprint1-remainder-and-test-groundwork.md) | Collectors, store, `tvt scan`, test groundwork | Done |
| 2 | [sprint 2](2026-11-sprint2.md) | References, query logs, hard keeps, SLOs and runbooks | Every seeded dependency resolves and no hard keep is misclassified |
| S3 | [s03](s03-cost-and-scoring.md) | Cost and scoring core | Every lab signal has a monthly cost on two price cards; the simulated agent sweep keeps nothing on its own |
| S4 | [s04](s04-report-v1.md) | Report v1 | First report in under 30 minutes on the lab |
| S5 | [s05](s05-benchmark-and-log-findings.md) | Benchmark and log findings | Benchmark baseline recorded; a deliberately broken parser change fails CI |
| S6 | [s06](s06-rules-query-cost-slos-pipelines.md) | Rules, query cost, SLOs, pipelines | All rule and pipeline findings match ground truth; the report runs on the Grafana Cloud test stack |
| S7–S12 | [s07–s12](s07-s12-propose-check-owners-release.md) | Writers, `check`, owner workflow, reduce before drop, savings ledger, release candidate | `v0.1.0-rc1` tagged; the MVP gate rehearsed on the lab |
| S13–S16 | [s13–s16](s13-s16-alert-noise-agents-collector.md) | Continuous-mode work pulled forward: alert noise, agents and history, Collector and conventions | Each sprint's seeded cases reported correctly |

Lab seeds for every sprint are collected in [lab-backlog.md](lab-backlog.md), so tvt-lab and its `ground-truth.yaml` can run a sprint ahead of the code.

**Rules for every item:** one branch and one PR per numbered item, `--base main`. Tests first or alongside. Keep the read-only guarantee against backends (`internal/testutil` fails any non-GET); the only writes are Git branches and PRs in mapped pipeline repos. Update `testdata/lab/ground-truth.yaml` when a lab seed changes. Update the Status section of `CLAUDE.md` in each PR. Nothing from internal planning (pricing, partners, schedules) goes in the repo.

**Never cut `check`.** It is the safety promise. When a sprint runs long, cut from its "should have" items, then push its last must-have to the next sprint.
