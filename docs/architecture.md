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
| `dormant` | Nothing reads it, but its name says error, latency, saturation or queue depth, its owner's tier is 0 or 1, a runbook or postmortem names it without querying it, or its owner has a `tvt.yaml` season the window missed | Ask the owner ("dormant: confirm" in the report); never a drop candidate |
| `candidate` | Everything else | Ranked by the weights below |

`hard_keep` and `keep_high` are both protected; they stay separate so the report can tell a declared dependency from "people look at it". Owners and tiers come from `ownership.owners` in `tvt.yaml` (first match; a label takes its metric's owner). A label nobody reads still takes `hard_keep` or `keep_high` when a protecting consumer keeps its metric's labels in the result (`refs.result_labels`: no aggregation, as in `rate(x[5m]) > 100`): removing it would merge series and change what the alert fires on or the viewed panel shows. It isn't counted as a read, so a raw ad hoc query protects nothing; `tvt why` lists such consumers as inferred readers.

## Usage weights (defaults, tunable)

Weights rank candidates. A viewed dashboard never reaches ranking (it makes the signal `keep_high`), but its weight still applies to label candidates and to the report's evidence.

| Evidence | Class | Weight |
| --- | --- | --- |
| Alert, SLO, runbook, recording rule that feeds an alert, declared external consumer | declared | Hard keep (never scored) |
| Dashboard viewed in the window | observed | 5, halving every 15 days since the last view (protected: `keep_high`) |
| Ad hoc or agent query | observed | 2, halving every 15 days since last seen |
| Recording rule nothing protected reads | declared | 1, never decays |
| Dashboard not viewed in the window | declared | 0.5, never decays |
| Sweep (a query over every metric name, such as an agent's inventory) | observed | 0.25: says the signal exists, not that anyone needs it |
| Nothing | | 0 |

A signal's weight is its strongest single piece of evidence. Weights are not summed, so many weak references can't outvote "nobody looks at this", and every weight can be explained by one reference (`scores.strongest`).

Declared and incident evidence never decays: an alert evaluates its query whether anyone looks or not. Observed evidence decays from its last-seen date, and last-seen dates are kept indefinitely (`store.LastReads`), so a read that has rotated out of every log still counts, for less and less: "last read nine months ago" never turns into "never read", and never protects a signal either.

## Confidence

Confidence is how far the evidence can be trusted to have shown every reader, in 0..1:

- **window:** a scan whose `--window` is shorter than `window_days` (tvt.yaml, default 30) scores every candidate with confidence 0 (`held = window`). Nothing is recommended from less.
- **coverage:** the mean coverage of the usage logs that could have shown the signal read: its backend's query log (Prometheus, Mimir or Loki) and Grafana's router log, which shows dashboard views. An unconfigured log counts as 0; with neither, confidence is 0 (`held = coverage`).
- **age:** days since the signal first appeared ÷ 14, capped at 1. A signal tvt saw in its first scan of that backend predates tvt, so its age is unknown and costs nothing; one that first appears in a later scan is new (`held = new` in that scan).

Only `candidate` signals are scored. Each scan stores one row per candidate in `scores`: cost, usage weight, strongest evidence, confidence, priority and, when confidence is 0, why.

## Design rules

- Read-only against every backend. Changes only happen through reviewed PRs. The only query tvt sends to a backend that logs queries is the query-log probe: a marker that selects no series, tagged `Source=tvt-probe`, which proves the log is written and is never counted as usage.
- Credentials come from the environment and are never logged.
- Collectors are tested against recorded responses, so no live stack is needed.
- The JSON report is the internal API.
- `check` fails closed.

See `docs/adr/` for the decisions behind these.
