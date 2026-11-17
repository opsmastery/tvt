# fluent/fluent-bit chart fixtures

[fluent-bit](https://github.com/fluent/helm-charts/tree/main/charts/fluent-bit) (Apache 2.0, the Fluent project) default values at the version in `VERSION`, from `helm show values fluent/fluent-bit --version $(cat VERSION)`. tvt's Fluent Bit Helm writer copies the chart's default `config.filters` from here when an overlay doesn't set it (Helm replaces strings). Bump on purpose, with the golden diffs reviewed.
