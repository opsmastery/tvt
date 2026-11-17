# 0011. Records, approvals bound to evidence, and the expected end state

Date: 2026-11-16 · Status: accepted (Nov 16)

## Context

tvt is an agent that proposes deleting data, so it has to meet the bar it applies to others. Andrew Mallaband's article on observing AI actions (reviewed Nov 12) and its comments set three requirements: keep every record of who did what, make an approval hold only for the conditions it was given under, and verify the result independently of the tool that made the change.

## Decision

1. **Records are a protected class beside hard keeps.** Audit logs, deploy and change events, Kubernetes audit, and agent tool-call spans (`gen_ai.operation.name=execute_tool`, `gen_ai.tool.name`) are records. They're detected by name patterns and OTel attributes, and declared with `records:` selectors in `tvt.yaml`.
2. **A record is never dropped, sampled or collapsed.** `propose` never targets one, and `check` fails any rule that would, including a filter on successful tool-call spans. Records protect the event, not every byte of it: capping a content attribute (truncated with a marker) and deleting identity attributes are allowed, as long as the outcome fields stay (tool name, success, error, duration, `gen_ai.tool.call.id`).
3. **An approval is bound to the evidence the owner saw.** The confirmation recorded in the protection manifest includes a hash of the readers, cost and evidence gaps shown to the owner. `check` refuses the merge if the usage graph has changed materially since: a new reader, or a gap reopened. The owner is asked again.
4. **Every recommendation states its expected end state** before merge, for example: "`checkout_cache_hits` returns no series within an hour of deploy; no rule or panel goes empty; stream bytes fall about 40%." `tvt verify` (Phase 2) checks it by querying the backend directly, never by trusting CI or the merge as proof.
5. **Decisions expire when their evidence changes.** A keep, dismissal or confirmation in the manifest records the evidence it rested on. When that changes (the reader is gone, the service was renamed, the owner moved teams), the entry is flagged for re-review instead of persisting or expiring silently.

## Consequences

- The manifest grows: each entry carries its evidence hash and the evidence it rested on.
- Re-asking owners when evidence changes adds review work. The capped queue and weekly digest (S9 item 6) bound it.
- tvt-lab needs an audit stream and a successful agent rollback span (`docs/tasks/lab-backlog.md`, S7).
