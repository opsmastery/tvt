# 0003. `tvt check` runs offline against a committed protection manifest

Date: 2026-11-08 · Status: proposed (finalize before Sprint 4)

## Context

`tvt check` runs in CI on config PRs. Re-scanning the backends from CI would need credentials and network access to production observability systems, and it would be slow.

## Decision

`tvt scan` writes `.tvt/protect.json` into the config repo. The manifest lists every protected signal and label, with the reason it is protected (alert, SLO, external consumer, viewed dashboard). `tvt check` evaluates the PR's config against the manifest offline. If the manifest is older than a configurable age (default 7 days), the check fails.

## Consequences

- CI needs no credentials, and the check takes seconds.
- The manifest has to be refreshed. In continuous mode, the service opens a PR to refresh it.
- The manifest reveals metric and dashboard names, but no query text or values.
