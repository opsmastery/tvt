# 0008. Self-observability uses the standard OpenTelemetry Go SDK

Date: 2026-11-10 · Status: accepted

## Context

The spec (goal 7, "Self-observability") requires tvt to emit traces, metrics and logs that explain each run, and to make them configurable. ADR 0004 puts the standard library first. We considered two options:

- `log/slog` plus a hand-written OTLP exporter in the CLI, keeping the full SDK for `tvt serve` only;
- the standard OpenTelemetry SDK everywhere.

## Decision

Use the standard OpenTelemetry Go SDK and exporters in the CLI and in `tvt serve`:

- `go.opentelemetry.io/otel`, `otel/sdk`, `otel/sdk/metric`, `otel/sdk/log`;
- OTLP exporters (gRPC and HTTP), plus the `stdout` exporters for local debugging;
- `go.opentelemetry.io/contrib/bridges/otelslog`, so all logging goes through `log/slog` and picks up trace and span IDs;
- `otelhttp` for collector HTTP clients, with request spans and metrics and no headers or query strings recorded;
- the Prometheus exporter for `/metrics` in `tvt serve`.

Configuration follows the OpenTelemetry spec: the `OTEL_*` environment variables and SDK autoconfiguration, with a `telemetry:` block in `tvt.yaml` mapped onto the same settings. With nothing configured, traces and metrics use no-op providers, and logs go to stderr through `slog`.

## Why

- **Standard over clever.** It's the library partners already know how to configure and route, and the one their Collectors, Alloy and vendors expect.
- **Correct by default.** Context propagation, batching, retries, resource detection and semantic conventions come from the SDK instead of our own exporter.
- **One code path,** instead of separate CLI and service implementations.
- All of it is Apache 2.0, matching ADR 0002.

## Consequences

- This is an accepted exception to ADR 0004. The binary grows, mostly from gRPC and protobuf. Measure the size and build time when it lands and record them here. If the size is a problem, drop the gRPC exporter and keep OTLP/HTTP before dropping the SDK.
- Instrumentation conventions live in `internal/telemetry`: tracer and meter names, span names (`tvt.scan`, `tvt.collect.<backend>.<call>`, `tvt.parse`, `tvt.score`, `tvt.store.write`, `tvt.propose.<pipeline>`, `tvt.check`), attribute keys, and the rule against per-signal metric labels.
- Tests use the SDK's in-memory span and metric exporters. The golden scan asserts the trace's shape and checks that no token, auth header or (by default) query text appears in any span, log or metric.

## Landed (Nov 21, 2026)

`internal/telemetry` (S12 item 6, Sprint 2 item 9), instrumenting `tvt scan`; see `docs/telemetry.md`. What changed from the plan:

- **OTLP over HTTP only.** No gRPC exporter, which the size note above allowed, plus the `stdout` trace exporter. The binary grew from 21.9 MB to 27.4 MB (darwin/amd64, `-s -w -trimpath`); build time moved within noise on a loaded laptop.
- **No `otelhttp`.** A small `RoundTripper` in `internal/httpx`'s default client makes the collect spans and metrics, so routes can be normalized (`/api/v1/label/{}/values`) and no path, query or error text is recorded at the default detail level.
- **No slog bridge yet.** tvt prints its progress to stdout and errors to stderr; the bridge comes with `tvt serve`.
- **Detail level** `TVT_TELEMETRY_DETAIL=basic|names`, the spec's detail level, rather than a `telemetry:` block in `tvt.yaml`, so it's configured where the rest of OTel is.
