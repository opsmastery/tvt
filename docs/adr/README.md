# Architecture decision records

One decision per file, numbered in the order they were written. A decision that changes an earlier one amends it and says so in both files' status lines.

| ADR | Decision | Status | Needed by |
| --- | --- | --- | --- |
| [0001](0001-one-repo-engine-as-library.md) | One repo; the engine is a library Legacy Bridge imports | Accepted | |
| [0002](0002-license-and-open-core-boundary.md) | Apache 2.0 CLI; team-edition code in a separate private module | Accepted | |
| [0003](0003-protection-manifest-for-ci.md) | `tvt check` runs offline against a committed protection manifest | Accepted; amended by 0007 | |
| [0004](0004-stdlib-first-no-cgo.md) | Standard library first, no CGO, track a supported Go | Accepted | |
| [0005](0005-own-logql-extractor.md) | tvt's own LogQL reference extractor | Accepted | |
| [0006](0006-line-level-log-cuts-handed-to-zeroreads.md) | Line-level log cuts are handed to zeroreads | Accepted | |
| [0007](0007-pipeline-targets.md) | Declared pipelines, one owner per signal | Accepted; amended by 0009 | |
| [0008](0008-standard-opentelemetry-sdk.md) | Self-observability uses the standard OpenTelemetry SDK | Accepted | |
| [0009](0009-fluentbit-exporter-pipelines-source-first.md) | Fluent Bit and exporter pipeline kinds; cut at the source first | Accepted | |
| [0010](0010-reduce-before-drop.md) | Reduce before drop: the least-lossy order | Accepted | |
| [0011](0011-records-approvals-end-state.md) | Records, approvals bound to evidence, expected end state | Accepted | |
| [0012](0012-reading-existing-ottl.md) | Reading the OTTL already in a Collector: a narrow extractor | Accepted; corpus gate passed Nov 18 (99.8%) | S8 |
| [0013](0013-alloy-syntax-package.md) | Read Alloy with Grafana's syntax package; write by splicing | Accepted | |

## Format

```markdown
# NNNN. Decision as a short sentence

Date: YYYY-MM-DD · Status: proposed (accept before SNN) | accepted | superseded by NNNN · Amends ADR NNNN

## Context

## Decision

## Alternatives considered   (optional)

## Consequences
```

Status moves from proposed to accepted in the PR that accepts it, and this table changes in the same PR.
