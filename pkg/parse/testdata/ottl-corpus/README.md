# OTTL corpus

`TestOTTLCorpus` (ADR 0012) parses every OTTL statement and condition in the
`*.jsonl` files here, plus the OTTL in the lab's Collector configs
(`testdata/bench/*/checkout/otelcol`), with tvt's narrow extractor
(`parse.OTTLStatement`, `parse.OTTLCondition`). It fails when the share
recognized (parsed, and every function one tvt knows) drops below
`baseline`. Raise `baseline` when the rate improves; never lower it to make a
change pass. Below 0.95, ADR 0012 reopens for contrib's own parser.

Each line is `{"source", "kind", "text"}`: `kind` is `statement` (an editor
call with an optional `where`) or `condition` (anything else, bare converters
and math expressions included). `invalid` marks a docs example no Collector
accepts (a typo, PromQL); it's left out of the rate and must still fail to
parse.

## Sources

| File | What | License |
|---|---|---|
| `contrib-v0.165.0.jsonl` | Every example in the READMEs of `pkg/ottl` (README, LANGUAGE, `ottlfuncs`, `xottl/ottlfuncs`), the transform, filter, tail sampling and log dedup processors, and the routing, count, sum and signal-to-metrics connectors, at opentelemetry-collector-contrib v0.165.0 (OTTL v1 since v0.164.0) | Apache 2.0, © The OpenTelemetry Authors |
| `opentelemetry-io.jsonl` | The opentelemetry.io blog posts on OTTL (v1, context inference in the transform and filter processors, lambda functions, the unroll and log dedup processors) and the "Transforming telemetry" page | CC BY 4.0, © The OpenTelemetry Authors |

Third-party blog posts are deliberately not vendored. SnowOps Labs, named in
ADR 0012, has no OTTL (checked Nov 18, 2026 at `df2435d`): its traces go
through Alloy with no transform or filter. tvt-lab's Collector config has no
OTTL of its own yet; the S8 seeds add some (`lab-backlog.md`), and they join
the corpus when a benchmark state captures them.

## Regenerating

`extract.py` reads the READMEs and pages, saved next to it as
`<path_with_underscores>.md` (the opentelemetry.io ones prefixed `otelio_`),
and prints one JSON line per statement; it needs PyYAML. The source names in
the checked-in files were rewritten to the paths above.

## Design partners

As for the PromQL and LogQL corpora: written consent first (kept outside this
repo), an anonymous name (`partner-a.jsonl`), no company or team names
anywhere, and only the OTTL text, never the rest of their config.
