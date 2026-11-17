# S9: owner workflow

**Status:** Planned.

**Done when:** a lab drop goes from draft to ready only after a recorded confirmation, and changing its evidence reopens it.

## Before you start

Read `CLAUDE.md`, `docs/tasks/README.md`, `docs/architecture.md` and ADRs 0003, 0006 and 0007 first. The only writes tvt makes are Git branches and PRs in mapped pipeline repos; every backend stays read-only.

Part of the MVP (S7–S12). The MVP is done when `v0.1.0-rc1` is tagged; on the lab, `tvt propose` opens a draft PR for a seeded drop, `tvt check` passes it and fails a PR that would break any hard keep or record, the PR leaves draft only after a recorded owner confirmation, and the next `tvt scan` records the measured saving in the ledger.

- S8's `check` merged: confirmations are enforced there.
- tvt-lab's S9 seeds (`lab-backlog.md`).

## Must have (in this order)

1. **Owners.** From the ownership map in `tvt.yaml`, CODEOWNERS, `team`/`service` labels and Backstage `catalog-info.yaml` (`spec.owner`, through the component's name or annotations). When none of those name an owner, fall back to OTel resource attributes and Kubernetes metadata: `service.namespace` and `service.name` mapped through `tvt.yaml`, then the workload's `app.kubernetes.io/part-of` and `app.kubernetes.io/name` labels and team labels on its namespace. An owner found this way is marked inferred in the recommendation. A dormant signal with no owner from any source stays report only and is listed as an evidence gap (missing ownership), never routed to a guess.
2. **Draft until confirmed, bound to evidence.** Every drop or label removal opens as a draft PR assigned to its owner. The confirmation, recorded in the protection manifest, includes a hash of the evidence the owner saw (readers, cost, gaps); `check` refuses the merge if the usage graph has changed materially since (a new reader, a gap reopened).
3. **Recommendation status.** Open, confirmed, or dismissed with a reason; a dismissal writes keep plus a review date to the manifest so tvt doesn't ask again. The owner review always asks whether the signal is read only at certain times of year; a yes writes keep plus a review date after the next season.
4. **Decisions expire when their evidence changes.** Each manifest entry (keep, dismissal, confirmation) records the evidence it rested on; when that changes (reader gone, service renamed, owner moved teams), the entry is flagged for re-review instead of persisting or expiring silently. Review dates remain the backstop.
5. **Expected end state.** Each recommendation states what should be true after the merge ("returns no series within an hour of deploy; no rule or panel goes empty; stream bytes fall about 40%"), for the owner to read before merging and for `tvt verify` to check later.
6. **Capped queue and weekly digest** per owner instead of a stream of PRs.
