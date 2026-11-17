# S16: Collector and conventions

**Status:** Planned.

**Done when:** seeded Collector and rename cases are reported correctly.

## Before you start

Continuous-mode work pulled forward from the roadmap's "Next" section; it starts once the MVP's code is complete. All findings are report only with ready-to-apply snippets: tvt never edits a rule, and every signal an alert reads stays a hard keep. Read `CLAUDE.md`, `docs/tasks/README.md` and `docs/architecture.md` first.

- ADR 0012's OTTL reading (S8 item 8).
- tvt-lab's S16 seeds (`lab-backlog.md`).

## Must have (in this order)

1. **Collector config checks:** a remaining `batch` processor (with its removal version) or one beside `sending_queue` batching, a debug exporter left in any pipeline, `memory_limiter` missing or after other processors (or a percentage limit in a hostPID pod), agent/gateway version skew, retired OTTL feature gates (`ottl.PanicDuplicateName`, `ottl.contexts.enableOTelColContext`, `ottl.set.allowNil`), regex statements with no `where`, `error_mode: propagate` in production pipelines, and uncast comparisons on attributes whose type varies. OTTL upgrade risks: statements that need a feature gate (experimental `xottl` functions under `pkg.ottl.functions.enableExperimental`, lambdas under `ottl.functions.enableLambda`) or use removed functions (`Base64Decode`), with the version where each breaks.
2. **Rename-aware metric catalog.** Map old and new semantic-convention names (`process.threads` → `process.thread.count`, attribute `state` → `cpu.mode`), so a new name read through its old one isn't unused; list dashboards and rules still on legacy names, and while both are emitted, show the duplicate series cost.
3. **Logs read only by counts become a metric.** When every reader of a stream is a `count_over_time` or `rate` query, propose a count connector that emits the metric, rewrite the queries to use it, and stage the lines' archive or drop.
4. **spanmetrics.** Span-name normalization from semconv attributes before the connector when `span.name` looks like raw paths or IDs (fixing names at the source stays the first recommendation; confirm `set_semconv_span_name` is a standard OTTL v1 function first); unread dimensions removed with `exclude_dimensions` or scoped to the one metric that uses them; a config with no `resource_metrics_key_attributes`, or volatile ones (pod, container ID, version), flagged.
