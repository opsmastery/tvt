# tvt why

`tvt why` answers one question about one signal: who reads it, and how does tvt know? It reads the latest scan in the database and never talks to a backend.

```sh
tvt why shop_cart_operations_total                 # a metric, by name
tvt why 'shop_cart_operations_total{op}'           # one label of a metric
tvt why '{service_name="checkout"}'                # a log stream, by selector
tvt why prom:metric:shop_cart_operations_total     # any signal, by ID
tvt why --json shop_cart_operations_total          # the same, as JSON
```

## Flags

| Flag | Default | What |
| --- | --- | --- |
| `--db` | `tvt.db` | The usage graph database, from `tvt scan` |
| `--config` | | `tvt.yaml`, for its `gaps.acknowledge` entries: an acknowledged gap is shown as acknowledged |
| `--json` | off | Print JSON instead of text |

Flags can come before or after the signal.

## What it prints

```
prom:metric:shop_cart_operations_total
  metric shop_cart_operations_total, in prom; scan 12 (2026-11-21, 30-day window)
  class candidate
  costs 4.10 USD/month
  last read 2026-11-20 by qlog:prometheus:b6f6db8bd4cc

readers (2)
  EVIDENCE  EXACT  SOURCE          READER                                                    FIRST SEEN  LAST SEEN
  observed  exact  query log       qlog:prometheus:b6f6db8bd4cc (ad hoc prometheus query)    2026-11-12  2026-11-20
                                     labels group:op
  observed  exact  dashboard JSON  panel:1:shop-overview:4:0 (Shop Overview / Cart ops)      2026-11-12  -
                                     labels group:op, legend:op

evidence gaps (1)
  - [partial] grafana router log covers 8% of the window
```

- **The header** names the signal, its class (`hard_keep`, `keep_high`, `candidate`, `dormant`), its monthly cost, its score and whether it's held, the last logged read, and its report row if it has one.
- **Readers** are every dashboard panel, variable, rule, query-log client and external consumer that selects it. `EVIDENCE` is the kind of read: `declared` (an alert, recording rule, SLO, runbook step or external consumer evaluates it by definition) or `observed` (someone was seen using it, in a view or a query log; the only kind that ages out). `EXACT` is `exact` when the reader names the signal, or `inferred` when tvt can't rule it out, such as a panel that picks its metric from a `$metric` variable; the reason is on the line below. The second line shows how a reader uses each label: `filter`, `group` or `legend`.
- **Evidence gaps** are the sources that couldn't have shown a read over the whole window, with how much of it they cover. A signal with no readers and open gaps isn't proven unread; the report holds it back.

A signal with no readers and no gaps is what the report calls unread. If you know a reader tvt missed, that's a bug: add it to `external_consumers` in `tvt.yaml` for now, and report it so the benchmark gets a case.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Printed |
| 1 | Error: no database, no scan, or no signal matches |
| 2 | Bad flags, or no signal given |
