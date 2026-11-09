# 0002. Apache 2.0 CLI; Team edition lives elsewhere

Date: 2026-11-08 · Status: accepted

## Decision

This repo is Apache 2.0 from the first commit, and it holds `scan`, `report`, `propose` and `check`. Team-edition features (continuous mode, cost attribution history, SSO) go in a separate private module that imports this one.

## Consequences

- No relicensing later.
- Nothing proprietary ever lands here, so contributors don't need a CLA for the open parts.
