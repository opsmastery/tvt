# PromQL parser corpus

`TestPromQLCorpus` parses every expression here plus the lab's dashboards and
rules (`testdata/lab`), prints the parse rate and fails if it drops below
`baseline`. Raise `baseline` when the rate improves; never lower it to make a
change pass.

## kube-prometheus.json

Rule and dashboard expressions from
[kube-prometheus](https://github.com/prometheus-operator/kube-prometheus)
`manifests/` at commit `d88939bfbe7ffa368a62dcc1ff477b1c19fa9674`
(Apache License 2.0): every `*-prometheusRule.yaml` and the dashboards in
`grafana-dashboardDefinitions.yaml` (kubernetes-mixin, node-mixin,
prometheus-mixin, alertmanager-mixin and others). Only the expressions and
where they came from are kept.

To regenerate, download those files from `manifests/` at a pinned commit into
a directory and run:

```sh
go run ./pkg/parse/internal/corpusgen kube-prometheus <dir> > pkg/parse/testdata/promql-corpus/kube-prometheus.json
```

Update the commit above when you do.
