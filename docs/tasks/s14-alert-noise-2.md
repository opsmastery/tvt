# S14: alert noise II

**Status:** Planned.

**Done when:** each seeded noisy alert lands in the right class.

## Before you start

Continuous-mode work pulled forward from the roadmap's "Next" section; it starts once the MVP's code is complete. All findings are report only with ready-to-apply snippets: tvt never edits a rule, and every signal an alert reads stays a hard keep. Read `CLAUDE.md`, `docs/tasks/README.md` and `docs/architecture.md` first.

- S13 merged.
- tvt-lab's S13–S14 seeds (`lab-backlog.md`).

## Must have (in this order)

1. **Firing-history classes** from the `ALERTS` series (and the Mimir or Loki ruler equivalents): transient (resolved within 5 minutes), fires during maintenance (overlaps a silence, `mute_time_intervals` or a change window), co-firing duplicates (heavily overlapping firing intervals on the same target), and silent for 90 days. Each class comes with a snippet (`for`, `keep_firing_for`, `mute_time_intervals`, `inhibit_rules`, `group_by`). A silent alert is flagged for its owner to confirm, never proposed for removal.
2. **Overlapping wildcard and specific alerts** on the same metric and target, in the duplicate class, with the exclusion matcher to add to the wildcard.
3. **Delivery failures:** `alertmanager_notifications_failed_total` against `alertmanager_notifications_total` per integration. An alert that fires into a failing receiver is silent.
