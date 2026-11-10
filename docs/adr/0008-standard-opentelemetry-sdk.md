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
