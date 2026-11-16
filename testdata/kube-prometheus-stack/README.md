# kube-prometheus-stack fixtures

[kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) (Apache 2.0, prometheus-community) rendered with `helm template` at the version in `VERSION`, so tests can read a real chart's objects without a cluster (`docs/tasks/lab-backlog.md`).

- `render.yaml`: `make kps-fixtures` (release `kps`, namespace `monitoring`, default values). Every `kind: Secret` is dropped (helm generates a Grafana admin password and webhook certificates on each render) and `checksum/secret` annotations are blanked, so two renders are identical and the secrets scanner stays quiet.
- `values.yaml`: the chart's default values at that version (`helm show values`).
- `extra-monitors.yaml`: hand-written, not from the chart. Monitors a team adds next to the chart: `payments` and `batch` carry `release: kps` and are selected; `shop` (no release label) and `legacy-jobs` (`release: other`) are not, and must never count (S5 item 8).

Bump the pin only on purpose (`make kps-fixtures KPS_VERSION=x.y.z`), with the golden diffs reviewed in the PR. Tests: `pkg/collect/kube` (`TestResolveKubePrometheusStack`) and `cmd/tvt` (`TestScanSelectedMonitors`; `TestKubePrometheusStack`, S6 item 8: the chart's rules, dashboards and kubelet drops against a hand-checked list of readers, each value checked against `render.yaml` by hand, so a pin bump means re-checking them).
