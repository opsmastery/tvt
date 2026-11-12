# S13–S16: continuous-mode work pulled forward

These come from the roadmap's "Next" section and start once the MVP's code is complete. All findings are report only with ready-to-apply snippets: tvt never edits a rule, and every signal an alert reads stays a hard keep. Read `CLAUDE.md`, `docs/tasks/README.md` and `docs/architecture.md` first.

## S13: alert noise I

**Done when:** lab alert findings match ground truth.

1. **Detection delay per alert** in the report (computed in S8): worst-case time to page, with the parts that make it up.
2. **Static alert lint** (`tvt report --alerts`): runbook annotation present, reachable and changed within 90 days (git history of the runbook file); owner or team label; severity label; a summary that templates the instance and value; a `for` debounce; `keep_firing_for` on thresholds that hover. Rules on `predict_linear`, `deriv` or multiwindow burn rates are tagged leading, with the leading share per service beside SLO coverage. An opt-in minimum score can gate rule PRs in CI.
3. **Thresholds against a robust baseline.** For each static threshold, the median and median absolute deviation (MAD) of the expression over 14 days: a threshold inside median + 3×MAD fires routinely; one far above the 14-day max may never fire. The snippet suggests k=3 and k=5 values.
4. **Findings roll up by template.** Rules sharing a normalized expression and differing only in selectors are one template; lint and noise findings are reported once per template with the instance count.

## S14: alert noise II

**Done when:** each seeded noisy alert lands in the right class.

1. **Firing-history classes** from the `ALERTS` series (and the Mimir or Loki ruler equivalents): transient (resolved within 5 minutes), fires during maintenance (overlaps a silence, `mute_time_intervals` or a change window), co-firing duplicates (heavily overlapping firing intervals on the same target), and silent for 90 days. Each class comes with a snippet (`for`, `keep_firing_for`, `mute_time_intervals`, `inhibit_rules`, `group_by`). A silent alert is flagged for its owner to confirm, never proposed for removal.
2. **Overlapping wildcard and specific alerts** on the same metric and target, in the duplicate class, with the exclusion matcher to add to the wildcard.
3. **Delivery failures:** `alertmanager_notifications_failed_total` against `alertmanager_notifications_total` per integration. An alert that fires into a failing receiver is silent.

## S15: agents and history

**Done when:** `--as-of` shows each lab state with no leakage, and MCP and CLI answers match.

1. **Read-only MCP server** over the JSON report and usage graph: `why_kept`, `consumers_of`, `savings_if_dropped`.
2. **A tvt agent skill** (`SKILL.md`): how an agent uses tvt through its CLI or MCP tools, and what it must never do (write config without a reviewed PR).
3. **Contract test:** the same question gives the same entities, evidence, time boundary and confidence through the CLI, the report and MCP.
4. **`tvt report --as-of <scan>`:** rebuild a report from a stored scan using only evidence observed up to it, with the cut-off shown.
5. **Agent workflows are declared readers.** Agent skill files and alert-triggered workflow definitions that run queries are parsed like runbook steps: declared dependencies, so hard keeps.
6. **Readers broken by a release.** Comparing scans, flag any panel, rule or alert whose metric, label or stream disappeared or was renamed between two scans, with the release where tvt can see one. A reliability finding, not a saving.

## S16: Collector and conventions

**Done when:** seeded Collector and rename cases are reported correctly.

1. **Collector config checks:** a remaining `batch` processor (with its removal version) or one beside `sending_queue` batching, a debug exporter left in any pipeline, `memory_limiter` missing or after other processors (or a percentage limit in a hostPID pod), agent/gateway version skew, retired OTTL feature gates (`ottl.PanicDuplicateName`, `ottl.contexts.enableOTelColContext`, `ottl.set.allowNil`), regex statements with no `where`, `error_mode: propagate` in production pipelines, and uncast comparisons on attributes whose type varies.
2. **Rename-aware metric catalog.** Map old and new semantic-convention names (`process.threads` → `process.thread.count`, attribute `state` → `cpu.mode`), so a new name read through its old one isn't unused; list dashboards and rules still on legacy names, and while both are emitted, show the duplicate series cost.
3. **Logs read only by counts become a metric.** When every reader of a stream is a `count_over_time` or `rate` query, propose a count connector that emits the metric, rewrite the queries to use it, and stage the lines' archive or drop.
4. **spanmetrics.** Span-name normalization from semconv attributes before the connector when `span.name` looks like raw paths or IDs (fixing names at the source stays the first recommendation; confirm `set_semconv_span_name` is a standard OTTL v1 function first); unread dimensions removed with `exclude_dimensions` or scoped to the one metric that uses them; a config with no `resource_metrics_key_attributes`, or volatile ones (pod, container ID, version), flagged.
