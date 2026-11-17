# S12: release candidate

**Status:** Planned.

**Done when:** `v0.1.0-rc1` is tagged, and the install guide works on a clean machine.

## Before you start

Read `CLAUDE.md`, `docs/tasks/README.md`, `docs/architecture.md` and ADRs 0003, 0006 and 0007 first. The only writes tvt makes are Git branches and PRs in mapped pipeline repos; every backend stays read-only.

Part of the MVP (S7–S12). The MVP is done when `v0.1.0-rc1` is tagged; on the lab, `tvt propose` opens a draft PR for a seeded drop, `tvt check` passes it and fails a PR that would break any hard keep or record, the PR leaves draft only after a recorded owner confirmation, and the next `tvt scan` records the measured saving in the ledger.

- Sprint 2 items 8–10 (more Grafana evidence, self-observability, `tvt init` and `SECURITY.md`) are due by this sprint.

## Must have (in this order)

1. **Supply chain:** reproducible builds verified in CI, cosign-signed releases, govulncheck, gitleaks, Dependabot, and a release checklist in `docs/`.
2. **`SECURITY.md`** (Sprint 2 item 10, if not done) and an install guide checked against a clean install: a fresh macOS user and a fresh Linux VM, where `tvt version` runs and the cosign check verifies `checksums.txt`. The HTML report is opened in a browser on both, not only checked for content.
3. **Docs for every command.**
4. **The README claim:** "Every signal tvt marks unread had no reader in your evidence window, and every gap is listed. If you find a reader we missed, that's a bug, and the benchmark gets a new case."
5. **Sensitive content note.** In the report, unread attributes whose names mark them as prompts, responses, emails or host paths are flagged beside their saving. tvt reads key names and sizes only, never values.
6. **tvt's own telemetry stays lean.** Signal names and query text stay out of tvt's telemetry by default (the spec's detail level). When opted in, attributes are capped at 512 characters with a truncated marker, and no per-scan or per-signal identifier goes on a metric, so a tvt scan of its own telemetry finds nothing to cut.
