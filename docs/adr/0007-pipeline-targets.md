# 0007. Pipeline targets: declared pipelines, one owner per signal

Date: 2026-11-09 · Status: proposed (finalize before Sprint 3)

## Context

`propose` has to put each change where the signal is actually shaped. Until now the docs assumed a single "config repo". Real teams have several pipelines:

- Prometheus scrape configs,
- an Alloy fleet,
- one or more OTel Collector deployments,

often rendered from Helm values in a GitOps repo, sometimes split across teams with different reviewers. Writing a change into the wrong file either does nothing (the signal never passes through it) or breaks someone else's pipeline. ADR 0003 also put a single protection manifest in "the config repo".

## Decision

1. **Pipelines are declared** in `tvt.yaml` under `pipelines:`. Each has a `name`; a `kind` (`prometheus`, `alloy`, `otelcol`; `vector` later); a `repo` and `branch`; the `files` to change, or `helm_values.file` plus `path` when the config is rendered from Helm; `handles`, the label selectors for the signals it ingests; and `reviewers`. See `examples/tvt.example.yaml`.
2. **Every signal maps to exactly one pipeline,** by matching its labels (metric series labels, or log stream labels) against `handles`. A signal that no pipeline claims, or that two claim, gets no proposal. It is still reported with its savings, as an evidence gap that blocks until the mapping is fixed or acknowledged.
3. **`propose` writes in the pipeline's own format,** in that pipeline's repo, one PR per repo with its reviewers:
   - Prometheus `metric_relabel_configs`,
   - Alloy `prometheus.relabel` or `loki.process` blocks,
   - OTel Collector `filter`/`transform` processors.

   It edits the source it was pointed at (a Helm values key), never a rendered file.
4. **One protection manifest per pipeline repo** (amends ADR 0003). `scan` writes `.tvt/protect.json` into each repo in `pipelines:`, and `check` runs in each repo's CI.
5. **Alloy is supported in the MVP** alongside Prometheus relabel. The OTel Collector follows in Sprint 4 if time allows; Vector and Cribl come in Phase 3.

## Consequences

- Partners must list their pipelines up front. That is part of the Phase 0 ask, and it shows how messy their setup is early.
- tvt-lab needs a real pipeline in the data path: Alloy, with its config in its own directory acting as a config repo, plus one signal no pipeline claims.
- `check` needs an evaluator per kind. For Alloy, decide before Sprint 4 between Grafana's Alloy syntax package (confirm the license first, as ADR 0005 did for Loki) and a narrow parser for the blocks tvt generates. Anything it can't evaluate fails closed.
- Templated config (Jsonnet, Kustomize, Alloy modules) beyond Helm values is out of scope for the MVP. Such signals are reported but not proposed.
