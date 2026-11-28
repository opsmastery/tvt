# S12: release candidate

**Status:** Done (Nov 21) for rc1, except two clean-machine checks that need Jason. `v0.1.0-rc2` followed on Nov 23. `v0.1.0-rc3` comes next from main, because the org rename changed the module path and the signing identity; the two checks run on rc3, and v0.1.0 is tagged on the same commit. Items 1 to 6 are done:

- **Item 1 (#170).** Reproducible builds: `-trimpath`, and the commit's time in `mod_timestamp`, `version.Date` and archive mtimes. CI's `reproducible` job builds every target twice from two checkout paths and diffs the hashes. Dependabot runs weekly for Go modules and actions. The release checklist is `docs/release.md`. Cosign signing, govulncheck and gitleaks were already in place.
- **Item 2 (#170).** `SECURITY.md` and `docs/install.md`.
- **Item 3 (#171).** Every command has a page: `docs/init.md`, scan, report, why, propose, check and confirm.
- **Item 4 (#171).** The README carries the claim.
- **Item 5 (#174).** `report.SensitiveName` sets `sensitive` on label and span-attribute rows (prompt, response, email, host_path), and the HTML shows it under the saving. On the live lab (Nov 21) it flagged `coding_agent_token_usage_total{user_email}`.
- **Item 6 (#175, with Sprint 2 item 9).** `internal/telemetry`, off unless `OTEL_*` asks for an exporter. Its spans and metrics follow `docs/telemetry.md`: `TVT_TELEMETRY_DETAIL=basic` keeps names, paths, queries and error text out; attributes are capped at 512 characters with a marker; no metric carries a per-scan or per-signal attribute.

Sprint 2 item 10's `tvt init` is #176. Item 8 (more Grafana evidence) is still open.

The two Dependabot PRs that waited at rc1 landed before rc2: #181 raised the Go minimum to 1.27 for `prometheus/common` v0.74, and #182 took #172's action bumps, with a release dry run on every PR that touches the release path.

**The rc1 release (Nov 21), checked by docs/install.md and docs/release.md:**

- The release workflow passed and published a pre-release with 6 archives, `checksums.txt`, `.sig` and `.pem`.
- **Fresh Linux** (a new `ubuntu:24.04` container on the Mac mini, arm64): cosign (latest, installed in the container) printed `Verified OK` for `checksums.txt`; `sha256sum --ignore-missing -c` passed; `tvt version` printed `tvt 0.1.0-rc1 (commit befb021, built 2026-11-21T14:31:18Z)`; `tvt report` rendered the live lab's latest scan to `report-linux.html` (201 KB). Cosign 3 calls `--certificate` and `--signature` deprecated in favour of `--bundle`; they still verify.
- **macOS** (the Mac mini's existing user, arm64, and the MacBook, amd64): checksums passed, `tvt version` matched, and `tvt report` wrote the same 201 KB page.
- **Reproducible:** `goreleaser build --clean --single-target` on a clean worktree of the tag gave the release's darwin/amd64 binary byte for byte (`ad9cf810...`).

Still open, for Jason:
- A **fresh macOS user**: creating an account is a system change.
- **Opening the HTML report in a browser** on both machines: the pages were written and are in `~/rc1-verify` on the Mac mini (`report-mac.html`, `report-linux.html`), but nobody has looked at them yet.

**rc2 (Nov 23).** Tagged before the org rename. The release test plan passed on the lab and the Grafana Cloud test stack, except R3 and R4 (the clean-machine installs). The org rename then rewrote main's history, so rc2's commit is not an ancestor of main.

**Since rc2, on main:** the module path is `github.com/opsmastery/tvt` and the cosign identity names `opsmastery/tvt` (#208), docs cleanup (#209), CODEOWNERS (#210), and a bound on the diff's LCS table from code scanning (#211). The repo is public, so `go install` needs no `GOPRIVATE`.

**rc3 checks before the tag (Nov 27, main at 03b5f3a):**

- main is green on every job, and R1's release dry run passed on #208, the last PR to touch the release path.
- `make bench` passes; the baseline hasn't changed since rc2.
- R8: every flag in each command's `-h` is on its page. `version` had no page; `docs/version.md` adds it.
- R4 of the checklist, `lab-pr.sh patch` on the Mac mini with a main build (03b5f3a): the scan and report ran clean, every evidence gap was already acknowledged (32), and nothing new. The patch step wrote nothing at first, and still said it had: tvt-lab's `tvt.yaml` kept the old org in its four `repo:` lines, so the script's `--checkout github.com/opsmastery/tvt-lab` matched no pipeline and all 1,252 recommendations were skipped as "no --checkout", with exit 0. Fixed on both sides: tvt-lab #55 renames the repos, and `tvt propose` now refuses a `--checkout` no pipeline names, listing the repos `tvt.yaml` does name (with a hint when only the case differs). Rerun with the old name mapped: 1,164 drops written, all four configs valid in their own validators (Prometheus v3.5.0, Alloy v1.10.0, Fluent Bit 5.2.0, Collector contrib 0.165.0), and `tvt check --skip-confirmation` passes the patch. Without the flag, check refuses all 1,161 drops, each as `unconfirmed_drop`, as it should with no owner confirms recorded.

**Still open:** tag rc3; R2 (archives, checksums and Sigstore bundle), R6 (rebuild one binary, same SHA-256), R7 (`go install ...@latest` without `GOPRIVATE`); R3, a fresh macOS user, and R4, a fresh Ubuntu VM (Jason); then v0.1.0 on the same commit, with the machines and results recorded here.

**Done when:** `v0.1.0-rc1` is tagged, and the install guide works on a clean machine.

## Before you start

Read `docs/tasks/README.md`, `docs/architecture.md` and ADRs 0003, 0006 and 0007 first. The only writes tvt makes are Git branches and PRs in mapped pipeline repos; every backend stays read-only.

Part of the MVP (S7–S12). The MVP is done when `v0.1.0-rc1` is tagged; on the lab, `tvt propose` opens a draft PR for a seeded drop, `tvt check` passes it and fails a PR that would break any hard keep or record, the PR leaves draft only after a recorded owner confirmation, and the next `tvt scan` records the measured saving in the ledger.

- Sprint 2 items 8–10 (more Grafana evidence, self-observability, `tvt init` and `SECURITY.md`) are due by this sprint.

## Must have (in this order)

1. **Supply chain:** reproducible builds verified in CI, cosign-signed releases, govulncheck, gitleaks, Dependabot, and a release checklist in `docs/`.
2. **`SECURITY.md`** (Sprint 2 item 10, if not done) and an install guide checked against a clean install: a fresh macOS user and a fresh Linux VM, where `tvt version` runs and the cosign check verifies `checksums.txt`. The HTML report is opened in a browser on both, not only checked for content.
3. **Docs for every command.**
4. **The README claim:** "Every signal tvt marks unread had no reader in your evidence window, and every gap is listed. If you find a reader we missed, that's a bug, and the benchmark gets a new case."
5. **Sensitive content note.** In the report, unread attributes whose names mark them as prompts, responses, emails or host paths are flagged beside their saving. tvt reads key names and sizes only, never values.
6. **tvt's own telemetry stays lean.** Signal names and query text stay out of tvt's telemetry by default (the spec's detail level). When opted in, attributes are capped at 512 characters with a truncated marker, and no per-scan or per-signal identifier goes on a metric, so a tvt scan of its own telemetry finds nothing to cut.
