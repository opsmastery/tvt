# tvt's own telemetry

tvt can explain each scan with OpenTelemetry traces and metrics (ADR 0008). It's off unless you ask for it, and lean when it's on: signal names and query text stay out by default, so a tvt scan of tvt's own telemetry finds nothing to cut (S12 item 6).

## Turning it on

Only the standard `OTEL_*` environment variables configure it. With none set, nothing is exported and nothing changes.

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318   # OTLP over HTTP: traces and metrics
export OTEL_SERVICE_NAME=tvt-nightly                             # optional; default tvt
tvt scan ...

OTEL_TRACES_EXPORTER=console tvt scan ...                        # spans to stderr, for a look
```

| Variable | Values |
| --- | --- |
| `OTEL_TRACES_EXPORTER` | `otlp`, `console` (stderr) or `none`. Unset: `otlp` when an OTLP endpoint is set, else `none` |
| `OTEL_METRICS_EXPORTER` | `otlp` or `none`, defaulting the same way |
| `OTEL_EXPORTER_OTLP_*` | Endpoint, headers, timeout, TLS, as the OTLP exporters read them. OTLP is over HTTP only; tvt doesn't link gRPC |
| `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_SERVICE_NAME` | Resource attributes, over `service.name=tvt` and `service.version` |
| `OTEL_SDK_DISABLED=true` | Off, whatever else is set |
| `TVT_TELEMETRY_DETAIL` | `basic` (default) or `names` (below) |

`tvt scan` is instrumented today. The other commands talk to no backend except Git, and stay quiet.

## What it emits

**Spans.** One `tvt.scan` root per run, with the window, the Grafana edition, the counts of signals, consumers and references, and log coverage. Each evidence source that can't vouch for the whole window is an `evidence_gap` event on it (source, configured, coverage, gap). Every backend request is a `tvt.collect.<backend>` client child (`grafana`, `prometheus`, `mimir`, `loki`, and `otelcol` for a Collector's `/metrics`) with the method, host, status code and route. A route keeps only API words and replaces every other path segment with `{}`: `/api/v1/label/{}/values`, `/api/dashboards/uid/{}`. A failure records the error's Go type, not its text, which often holds a URL or a name.

**Metrics.** Bounded attributes only, and nothing per scan or per signal:

| Metric | Attributes |
| --- | --- |
| `tvt.scan.runs` (counter) | `outcome`: ok, error, interrupted |
| `tvt.scan.duration` (histogram, s) | `outcome` |
| `tvt.scan.signals` (gauge) | `tvt.class`: hard_keep, keep_high, dormant, candidate |
| `tvt.collect.requests` (counter) | `tvt.backend`, `outcome` (2xx, 4xx, 5xx, error) |
| `tvt.collect.duration` (histogram, s, to response headers) | `tvt.backend`, `outcome` |

## Detail levels

- **`basic`** (default): counts, durations, outcomes and normalized routes. No signal name, URL path, query text or error message goes anywhere.
- **`names`**: spans also get the raw `url.path`, the request's query text (`db.query.text`) and error messages. Metrics never do.

At either level, every string attribute is capped at 512 characters and a cut value ends with `…[truncated]`, so it can't pass for a whole one. Headers are never recorded, so no token reaches an exporter.

## Tests

`internal/telemetry` and `cmd/tvt`'s `TestScanTelemetryShape` and `TestScanTelemetryNoSecrets` run the lab scan with in-memory providers. They assert the trace shape (one root, every other span a collect child of it, gap events), that each metric carries only its listed attributes, that no token, auth header, signal name or probe query appears at the basic level, that paths and queries do appear at the names level, and that nothing exceeds 512 characters.
