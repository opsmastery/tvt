# 0005. Write our own LogQL reference extractor

Date: 2026-11-08 · Status: accepted

## Context

`pkg/parse` needs the stream selectors, label filters, parser stages and `by`/`without` grouping in LogQL expressions from Loki rules, dashboard panels and query logs. Phase 0 spike 3 measured importing Loki's `pkg/logql/syntax` (Loki v3.7.9, Prometheus v0.316.0, `CGO_ENABLED=0 -trimpath -ldflags='-s -w'`, cold cache):

| Build | Binary | Modules | Packages | Cold build |
| --- | --- | --- | --- | --- |
| stdlib only | 1.6 MB | 1 | 60 | 6 s |
| + `promql/parser` | 6.7 MB | 256 | 260 | 17 s |
| + `promql/parser` + Loki `logql/syntax` | 27 MB | 583 | 801 | 49 s |

Other findings:

- **License.** Loki is AGPL-3.0. tvt is Apache 2.0 (ADR 0002), and the Team edition and Legacy Bridge import `pkg/parse`. Importing or vendoring Loki code would put AGPL code into all of those builds. Prometheus is Apache 2.0, so `promql/parser` is fine.
- **It doesn't build as a dependency without copying Loki's `replace` directives.** Loki pins forks of `hashicorp/memberlist` and `grafana/regexp`, and `go mod tidy` alone produces a build that fails in `grafana/dskit`. Every Loki upgrade would mean re-copying them.
- The syntax package imports Loki's log pipeline, `dskit`, gRPC and the OTel exporters: 19 Loki packages, 8.6k lines in `syntax` itself.

## Decision

Write a LogQL reference extractor in `pkg/parse`, from the public LogQL documentation, not from Loki's source. It only has to extract references, not evaluate queries:

- stream selectors and their matchers,
- labels created by parser stages (`json`, `logfmt`, `regexp`, `pattern`, `label_format`) and used in label filters,
- range and vector aggregations with `by`/`without`, and binary operators with `on`/`ignoring`,
- line filters can be skipped (they reference no signal).

The metric half of LogQL follows PromQL closely, so the grouping and vector-matching logic can share code with the PromQL path.

Anything the extractor can't parse counts as *referenced, low confidence* (plan risk 2). It never counts as unused.

## Consequences

- No AGPL code in the binary, which stays near 7 MB.
- About 3–4 days in Sprint 2, plus a corpus of LogQL test cases (Loki mixins, community dashboards, partner queries).
- The extractor can lag new LogQL syntax. Unparseable queries fail safe, and the report counts them.
- This supersedes the "vendor a subset" fallback in ADR 0004 and the plan's risk table.
