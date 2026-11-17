# Custom resource schemas

JSON schemas for the custom resources `tvt propose` writes, checked with
kubeconform (`validate.Tools["kubernetes"]`):

- `monitoring.coreos.com`: Prometheus Operator `ServiceMonitor`, `PodMonitor`, `Probe` (v1)
- `fluentbit.fluent.io`: Fluent Operator resources (v1alpha2)

Copied from [datreeio/CRDs-catalog](https://github.com/datreeio/CRDs-catalog)
(MIT) at commit `784ec895547a36e548e6810b41f27baade3a0890`. To update, copy the
same files from a newer commit and change the commit here.
