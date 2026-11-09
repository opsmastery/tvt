# 0004. Standard library first, no CGO

Date: 2026-11-08 · Status: accepted

## Decision

- Ship as a single static binary (`CGO_ENABLED=0`). For SQLite, use the pure-Go `modernc.org/sqlite`.
- Use the standard library unless a dependency clearly earns its place. The expected dependencies are the Prometheus PromQL parser, our own LogQL reference extractor (ADR 0005: Loki's syntax package is AGPL-3.0 and adds about 20 MB), a YAML library, and the GitHub/GitLab clients.
- The CLI starts on the standard `flag` package. Move to cobra if the command tree grows past a few levels.

## Consequences

- Cross-compiling for linux, darwin and windows on amd64 and arm64 is trivial.
- The initial scaffold has no third-party dependencies at all.
