# MetricsQL corpus

VictoriaMetrics queries that use MetricsQL extensions (WITH templates,
ranges left out or in steps, `default`, `keep_metric_names`, `limit`,
`or` inside filters, VictoriaMetrics-only functions), written for tvt from
the MetricsQL documentation, plus a few that are also valid PromQL.

Each entry lists the metric names it reads. `TestMetricsQLCorpus` asserts
that tvt never loses a reference: a query the PromQL parser rejects must
still come back as assumed references to every name listed. It also
reports what share of the corpus the PromQL parser accepts and fails if
that drops below `baseline` (a drop means valid PromQL started failing).
