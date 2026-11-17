# S1: collectors, store, `tvt scan`, test groundwork

**Status:** Done Nov 10 (PRs #1–#10, `v0.0.1-rc.1`).

**Done when:** each item below is merged through its own PR with CI green, `make race` passes, and `tvt scan` runs against the live lab with Prometheus and with Mimir (details under "Done when" at the end).

Written Nov 10, 2026. That session has Go, module downloads and the live tvt-lab, which this brief's author didn't. Work in this order, **one branch and one PR per numbered item, each with `--base main`**. Read `CLAUDE.md`, `docs/ROADMAP.md`, `docs/query-logs.md` and the [testing strategy](https://claude.ai/artifact/KLacfcEbfNS9WmaXZHEdWk) first.

Already written, but uncommitted in the working tree when you start (commit them all on item 0's branch): ADR 0008 and the ADR 0004 exception note, the partner-guide link in `docs/query-logs.md`, this brief, and the CI and release changes, namely `.github/workflows/ci.yml` (adds `goreleaser check`, `govulncheck` and `gitleaks` jobs), `.github/workflows/release.yml` (tag-triggered goreleaser with cosign keyless signing) and a `signs:` block in `.goreleaser.yaml`. None of them has run yet. Item 0 is to prove they work.

## 0. Prove the CI and release changes

- Run `goreleaser check` and `goreleaser release --snapshot --clean --skip=sign,publish` locally; fix anything they reject.
- Push the branch and confirm all four CI jobs pass. If gitleaks flags a fixture, decide whether it's a real secret. Allow only known lab placeholders (`admin:admin`) through `.gitleaks.toml`, with a comment.
- Cut `v0.0.1-rc.1` and confirm the release has archives, `checksums.txt`, and `checksums.txt.sig` and `.pem` files that `cosign verify-blob` accepts (the command is in `.goreleaser.yaml`). Delete the pre-release afterwards if you don't want it public.

## 1. Loki rules collector

- **Where:** a new `pkg/collect/loki` package (client plus `rules.go`), following `pkg/collect/prometheus`: one `Client` with a `get` helper, a `New(baseURL, token)`, an injectable clock and HTTP client, and every request a GET.
- **What:** implement `collect.RuleSource` from `GET /loki/api/v1/rules`. It returns YAML: namespace → groups → rules. Each rule becomes a `collect.Rule` with `Source: "loki"`, `Language: "logql"`, the namespace, group, name, kind (`alert` or `record`) and expr. Use `go.yaml.in/yaml/v3` (or `gopkg.in/yaml.v3`) if neither is in `go.mod` yet; ADR 0004 already expects a YAML library.
- **Empty ruler:** Loki answers 404 with `no rule groups found` when there are none. That means zero rules, not an error.
- **Wire it into `tvt scan`:** when `--loki` is set, collect Loki rules and add them as consumers. Replace the "not collected yet (Sprint 3)" line for rules only; volume stays Sprint 3.
- **Extraction:** until the LogQL extractor exists (Sprint 2, ADR 0005), a Loki rule is a consumer with an *assumed* reference to the streams its selector names. If you can't resolve that cheaply, mark it "assumed, reads all of the stream's labels". Never let it count as unused.
- **Tests:** a fixture test against `testdata/lab/loki/rules.yaml`, plus the empty-ruler 404 and a malformed body. **Golden:** `ShopAuditDenials` is a consumer, and the `{app="shop", component="audit"}` stream is hard-keep.

## 2. Mimir cardinality collector and Mimir rules

- **Why:** `--mimir` currently reuses the Prometheus client, but Mimir has no `/api/v1/status/tsdb`. Add a `pkg/collect/mimir` package that implements `collect.CardinalitySource` from Mimir's cardinality API, under the `/prometheus` prefix that `mimirAPI()` already adds:
  - `GET /prometheus/api/v1/cardinality/label_values?label_names[]=__name__&limit=…` gives series per metric name, which becomes `MetricCardinality`;
  - `GET /prometheus/api/v1/cardinality/label_names?selector={__name__="<metric>"}` gives label names and value counts for one metric, which becomes `LabelCardinality`.
  - This needs `limits.cardinality_analysis_enabled: true`. The lab has it, and the partner guide asks for it. If it's disabled, return a clear error that names the setting.
- **Rules:** keep using the Prometheus-compatible `GET /prometheus/api/v1/rules` (the existing `prometheus.Client.Rules`), but set `Source: "mimir"`.
- **Lab:** the lab's Mimir has no rules today. In tvt-lab, add a one-shot `mimir-rules` service to the `mimir` profile that loads a rule group, a copy of `shop-recording` plus one alert on a metric nothing else reads. Use `mimirtool rules load`, or `curl -X POST` YAML to `/prometheus/config/v1/rules/<namespace>`. Add those rules to `ground-truth.yaml` under the mimir source.
- **Fixtures:** run `make up-mimir` in tvt-lab, wait a few minutes for data, then extend `scripts/capture-fixtures.sh` with `MIMIR=http://localhost:9009` captures into `testdata/lab/mimir/`: cardinality label_values for `__name__`, label_names for `shop_cart_operations_total`, and rules.
- **Tests:** fixture tests for both cardinality calls, plus disabled-analysis and limit/truncation cases (mirror `truncated()` in the Prometheus client). Then a golden `tvt scan --mimir` run that yields the same signals as `--prom` for the shop metrics.

## 3. Testing groundwork

From the testing strategy's setup tasks, in order:

1. **`internal/testutil` fixture server.** It serves `testdata/` by path, and **fails the test** on any method other than GET and on `/api/v1/query`, `/api/v1/query_range` and `/loki/api/v1/query*` (invariant 4). Move the Prometheus, Grafana, new Loki and Mimir tests and the golden scan test onto it.
2. **The golden test reads ground truth.** The capture script copies `../tvt-lab/ground-truth.yaml` into `testdata/lab/ground-truth.yaml`. `cmd/tvt/scan_test.go` asserts the Sprint 1 rows from it (every seeded metric is a signal, every lab rule is a consumer, Shop Overview is viewed and Legacy Queue isn't, tvt's own loads are excluded) instead of hard-coded names. Never relax an assertion to make it pass.
3. **CI:** done in item 0 (`govulncheck`, `gitleaks`, `goreleaser check`).
4. **Fuzz targets** for `internal/logfmt`, the Prometheus query-log line parser and the Grafana router-log parser, seeded from the fixtures. They assert no panics, and that garbage never yields an empty "unused" result.

## Done when

- Each item is merged to `main` through its own PR, with CI green.
- `make race` passes, and the golden test reads `ground-truth.yaml`.
- `tvt scan --prom … --loki …` and `tvt scan --mimir … --loki …` both run against the live lab, and the summary shows the Loki and Mimir rules as consumers.
- Ticked in the plan doc's checklist and the testing strategy's setup tasks.
