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
5. **score** excludes hard keeps and ranks the rest by `cost / (1 + usage weight) × confidence`.
6. **propose** writes config. **check** evaluates any config PR against the protection manifest and fails closed.

## Usage weights (defaults, tunable)

| Evidence | Weight |
| --- | --- |
| Alert, SLO, recording rule that feeds an alert, declared external consumer | Hard keep (never scored) |
| Dashboard viewed in the last 30 days | 5 |
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

- Read-only against every backend. Changes only happen through reviewed PRs.
- Credentials come from the environment and are never logged.
- Collectors are tested against recorded responses, so no live stack is needed.
- The JSON report is the internal API.
- `check` fails closed.

See `docs/adr/` for the decisions behind these.
