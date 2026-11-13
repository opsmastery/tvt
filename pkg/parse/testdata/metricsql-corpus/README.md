# MetricsQL corpus

VictoriaMetrics queries that use MetricsQL extensions (WITH templates,
ranges left out or in steps, `default`, `if`, `ifnot`,
`keep_metric_names`, `limit`, `or` inside filters, VictoriaMetrics-only
functions), written for tvt from the MetricsQL documentation, plus a few
the PromQL parser also accepts.

Each entry says whether the PromQL parser accepts it (`promql`; the
Grafana interval variable entry parses because tvt substitutes `$__interval`
before parsing) and lists the metric names it reads. `TestMetricsQLCorpus`
asserts both: the parse outcome, and that the references are exactly the
listed names. A rejected query must lose none of them and invent nothing
else: no MetricsQL keyword, no WITH binding, no reference matching every
metric.
