# S13: alert noise I

**Status:** Planned.

**Done when:** lab alert findings match ground truth.

## Before you start

Continuous-mode work pulled forward from the roadmap's "Next" section; it starts once the MVP's code is complete. All findings are report only with ready-to-apply snippets: tvt never edits a rule, and every signal an alert reads stays a hard keep. Read `docs/tasks/README.md` and `docs/architecture.md` first.

- S8 computes each alert's detection delay (item 5).
- tvt-lab's S13–S14 seeds (`lab-backlog.md`).

## Must have (in this order)

1. **Detection delay per alert** in the report (computed in S8): worst-case time to page, with the parts that make it up.
2. **Static alert lint** (`tvt report --alerts`): runbook annotation present, reachable and changed within 90 days (git history of the runbook file); owner or team label; severity label; a summary that templates the instance and value; a `for` debounce; `keep_firing_for` on thresholds that hover. Rules on `predict_linear`, `deriv` or multiwindow burn rates are tagged leading, with the leading share per service beside SLO coverage. An opt-in minimum score can gate rule PRs in CI.
3. **Thresholds against a robust baseline.** For each static threshold, the median and median absolute deviation (MAD) of the expression over 14 days: a threshold inside median + 3×MAD fires routinely; one far above the 14-day max may never fire. The snippet suggests k=3 and k=5 values.
4. **Findings roll up by template.** Rules sharing a normalized expression and differing only in selectors are one template; lint and noise findings are reported once per template with the instance count.
