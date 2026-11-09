# 0001. One repo; the scoring engine is a library

Date: 2026-11-08 · Status: accepted

## Context

The spec describes Telemetry Value Tracker as the standalone, cross-vendor version of the Value Analyzer in Legacy Bridge. Both use the same scoring engine. The open question was whether to ship it as part of Legacy Bridge, standalone, or both.

## Decision

Build `tvt` as its own repo. Keep the engine (`pkg/score`, `pkg/cost`, `pkg/graph`, `pkg/parse`) free of I/O and CLI concerns so Legacy Bridge can import it as a Go module.

## Consequences

- There is one scoring implementation and no fork.
- The open CLI works as a funnel without Legacy Bridge.
- Changes to the public `pkg/` APIs need care once Legacy Bridge depends on them. Tag releases.
