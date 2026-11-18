# Task lists

From Sprint 3 on, sprints are two working days, each with one theme. They're ordered to keep the riskiest promise safe: the report first, the benchmark in CI before any writer exists, and the `check` safety gate before owners can confirm drops. Each sprint is done when its "done when" holds on the lab.

| Sprint | File | Theme | Done when | Status |
| --- | --- | --- | --- | --- |
| S1 | [s01](s01-collectors-store-scan.md) | Collectors, store, `tvt scan`, test groundwork | Each item merged with CI green; `tvt scan` runs on the live lab | Done Nov 10 (`v0.0.1-rc.1`) |
| S2 | [s02](s02-references-query-logs-hard-keeps.md) | References, query logs, hard keeps, SLOs and runbooks | Every seeded dependency resolves and no hard keep is misclassified | Done Nov 16; open: items 8–10 (more Grafana evidence, self-observability, `tvt init` and `SECURITY.md`, due by S12), 13, and item 11's Adaptive Metrics usage counts |
| S3 | [s03](s03-cost-and-scoring.md) | Cost and scoring core | Every lab signal has a monthly cost on two price cards; the simulated agent sweep keeps nothing on its own | Done Nov 14 |
| S4 | [s04](s04-report-v1.md) | Report v1 | First report in under 30 minutes on the lab | Done Nov 14 |
| S5 | [s05](s05-benchmark-and-log-findings.md) | Benchmark and log findings | Benchmark baseline recorded; a deliberately broken parser change fails CI | Done Nov 15 |
| S6 | [s06](s06-rules-query-cost-slos-pipelines.md) | Rules, query cost, SLOs, pipelines | All rule and pipeline findings match ground truth; the report runs on the Grafana Cloud test stack | Done Nov 16 (item 6 on the Grafana Cloud test stack, `docs/grafana-cloud.md`) |
| S7 | [s07](s07-writers.md) | Writers | `propose` opens a draft PR against the lab pipelines repo for each writer kind | Done (PRs #93–#134): the done-when passed on the live lab Nov 18 (M7) |
| S8 | [s08](s08-check-safety-gate.md) | `check`, the safety gate | Every unsafe case in ground truth is refused, with the reason in the `check` output | Must haves done Nov 18 (PRs #135–#138); should haves open (gated lab suite, SnowOps) |
| S9 | [s09](s09-owner-workflow.md) | Owner workflow | A lab drop goes from draft to ready only after a recorded confirmation, and changing its evidence reopens it | Planned |
| S10 | [s10](s10-reduce-before-drop.md) | Reduce before drop | For each lab finding, `propose` picks the least-lossy action that `check` accepts | Planned |
| S11 | [s11](s11-savings-ledger.md) | Savings ledger | The MVP gate is rehearsed on the lab: merge a tvt PR, and the next scan records measured savings | Planned |
| S12 | [s12](s12-release-candidate.md) | Release candidate | `v0.1.0-rc1` is tagged, and the install guide works on a clean machine | Planned |
| S13 | [s13](s13-alert-noise-1.md) | Alert noise I | Lab alert findings match ground truth | Planned |
| S14 | [s14](s14-alert-noise-2.md) | Alert noise II | Each seeded noisy alert lands in the right class | Planned |
| S15 | [s15](s15-agents-and-history.md) | Agents and history | `--as-of` shows each lab state with no leakage, and MCP and CLI answers match | Planned |
| S16 | [s16](s16-collector-and-conventions.md) | Collector and conventions | Seeded Collector and rename cases are reported correctly | Planned |

## How these files are written

Every task file follows the same shape, so status can be read the same way everywhere.

- **File name:** one file per sprint, `sNN-<theme>.md`, the theme in lowercase words joined by hyphens. Lab seeds go in `lab-backlog.md`, decisions in `docs/adr/`.
- **Top of the file:** `# SNN: theme`, then a `**Status:**` line, then a `**Done when:**` line, before anything else.
- **Status values:** Planned, Next, In progress, or Done with the date and the PRs. Open carry-overs are named on the same line ("Done Nov 16; open: items 8–10").
- **Sections:** `## Before you start` (what to read and what must be merged first), `## Must have (in this order)`, `## Should have`, in that order. Sprint-specific sections (Grafana Cloud, groundwork for the next sprint) go after them.
- **Items:** numbered, one bold sentence first, then a short done note when it applies ("Done Nov 17 for patches; open: PRs"), then the spec. What shipped in detail is recorded in `CLAUDE.md`'s Status section, not repeated here.
- **No schedule.** Done dates are history and stay. Planned dates, partners and pricing stay out of the repo.
- **The table above** is the index. Change a sprint's row in the same PR that changes its file's Status line.

Lab seeds for every sprint are collected in [lab-backlog.md](lab-backlog.md), so tvt-lab and its `ground-truth.yaml` can run a sprint ahead of the code.

**Rules for every item:** one branch and one PR per numbered item, `--base main`. Tests first or alongside. Keep the read-only guarantee against backends (`internal/testutil` fails any non-GET); the only writes are Git branches and PRs in mapped pipeline repos. Update `testdata/lab/ground-truth.yaml` when a lab seed changes. Update the Status section of `CLAUDE.md` in each PR. Nothing from internal planning (pricing, partners, schedules) goes in the repo.

**Never cut `check`.** It is the safety promise. When a sprint runs long, cut from its "should have" items, then push its last must-have to the next sprint.
