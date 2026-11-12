# Architecture

```
 Grafana ──┐                                                  ┌─> report (HTML + JSON)
 Prom/Mimir├─> collect ─> parse ─> usage graph ─> cost ─> score┤
 Loki ─────┘                        (SQLite)                  └─> propose ─> PR
                                        │
                                        └─> .tvt/protect.json ─> check (CI)
```

1. **collect** reads cardinality, rules, dashboards, query logs and stream volumes through read-only interfaces (`pkg/collect`).
2. **parse** resolves each query to the metric names, labels and stream selectors it touches, including transitively through recording rules.
3. **graph** stores signals, consumers, the references between them, and costs.
4. **cost** prices each signal with the customer's price card.
5. **score** classifies every signal (below), then ranks the candidates by `cost / (1 + usage weight) × confidence`.
6. **propose** writes config. **check** evaluates any config PR against the protection manifest and fails closed.

## Classes

Every scan stores a class on every signal (`score.Classify`, applied in `scan.Build`):

| Class | When | What tvt may do |
| --- | --- | --- |
| `hard_keep` | An alert (Prometheus, Mimir, Loki or Grafana-managed), a recording rule feeding one, an SLO or runbook query, or an external consumer declared in `tvt.yaml` reads it | Never recommend |
| `keep_high` | A dashboard viewed in the window shows it | Never recommend; `check` protects it like a hard keep |
| `dormant` | Nothing reads it, but its name says error, latency, saturation or queue depth, or its owner's tier is 0 or 1 | Ask the owner; never a drop candidate |
| `candidate` | Everything else | Ranked by the weights below |

`hard_keep` and `keep_high` are both protected; they stay separate so the report can tell a declared dependency from "people look at it". Owners and tiers come from `ownership.owners` in `tvt.yaml` (first match; a label takes its metric's owner).

## Usage weights (defaults, tunable)

Weights rank candidates. A viewed dashboard never reaches ranking (it makes the signal `keep_high`), but its weight still applies to label candidates and to the report's evidence.

| Evidence | Weight |
| --- | --- |
| Alert, SLO, recording rule that feeds an alert, declared external consumer | Hard keep (never scored) |
| Dashboard viewed in the last 30 days | 5 (protected: `keep_high`) |
| Ad hoc or agent query | 2, halving every 15 days since last seen |
| Dashboard not viewed in 90 days | 0.5 |
| Nothing | 0 |

A signal's weight is its strongest single piece of evidence. Weights are not summed, so many weak references can't outvote "nobody looks at this", and every weight can be explained by one reference.

## Confidence

`window × log coverage × age`, where each factor is in 0..1:

- **window:** observation days ÷ 30
- **log coverage:** the share of backends whose query logs are usable for the window
- **age:** days since the signal first appeared ÷ 14

## Design rules

- Read-only against every backend. Changes only happen through reviewed PRs. The only query tvt sends to a backend that logs queries is the query-log probe: a marker that selects no series, tagged `Source=tvt-probe`, which proves the log is written and is never counted as usage.
- Credentials come from the environment and are never logged.
- Collectors are tested against recorded responses, so no live stack is needed.
- The JSON report is the internal API.
- `check` fails closed.

See `docs/adr/` for the decisions behind these.
