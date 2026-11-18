# 0012. Reading the OTTL already in a Collector

Date: 2026-11-16 · Status: accepted (Nov 16): option 2; corpus gate passed Nov 18 (99.8% recognized)

## Context

`check` has to know what a pipeline's existing `filter` conditions drop and what its `transform` statements delete, rename or set (S8 item 8), and which attributes routing, tail-sampling and filter conditions read (S8 item 9). Until it can read OTTL, it fails closed on any statement that touches a signal tvt changes, which blocks most real Collector configs.

OTTL is v1 from opentelemetry-collector-contrib v0.164.0: the language reference (paths, literals, operators, function calls, `where` clauses), the standard functions and the Go API are frozen for 1.x. Lambda functions, experimental `xottl` functions and `xprofile` are outside that guarantee. `github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl` is now its own module (v1.4.1, Nov 9, 2026, `go 1.26.0`).

Measured on Nov 16, the same way as ADR 0013: importing `pkg/ottl` adds 2.45 MB to the stripped binary (18.23 MB to 20.68 MB, +13%) and 17 lines to `go.mod`, including pdata and the Collector component and featuregate modules.

## Options

1. **Import `pkg/ottl`** (Apache 2.0). Exact grammar and function signatures, maintained upstream, frozen for 1.x. Costs 2.45 MB and a set of Collector modules that move together on every contrib release.
2. **A narrow extractor.** Tokenize OTTL statements and recognize only paths and the editors that change them (`delete_key`, `delete_matching_keys`, `keep_keys`, `set`, `replace_pattern`, `truncate_all` and the like), plus the paths a condition reads. Anything it doesn't recognize stays fail-closed for the signals it could touch. Small, but tvt maintains a grammar.
3. **Both.** The extractor by default, the full parser behind a build tag for partners with heavy OTTL.

## Decision

Option 2, a narrow extractor. tvt needs to know which paths a statement reads and changes, not to run it, and the language is frozen, so a grammar written once from the v1 reference stays correct for 1.x. 2.45 MB is about 13% of the binary for that. The parse rate on real configs (the OTTL corpus in the parser tests, SnowOps Labs, partner configs) decides: below 95% of statements recognized, this ADR is reopened for option 1. Until the extractor exists, `check` stays fail-closed on OTTL it can't read.

## Corpus result (Nov 18)

The prototype is `parse.OTTLStatement` and `parse.OTTLCondition` (`pkg/parse/ottl.go`, `ottl_parser.go`, about 800 lines with no new dependency): a lexer and recursive-descent parser for the v1 grammar that returns, per statement, the editor, its target path, the paths read (all of them, and the where clause's apart), the functions called, and whether any is unknown, experimental or a lambda. `TestOTTLCorpus` is the gate, with `pkg/parse/testdata/ottl-corpus/baseline` at the measured rate.

The corpus (`testdata/ottl-corpus`) has 429 statements and conditions: every example in the contrib READMEs that use OTTL at v0.165.0 (pkg/ottl, the transform, filter, tail sampling and log dedup processors, the routing, count, sum and signal-to-metrics connectors) and the opentelemetry.io OTTL posts and docs page. Eight docs examples are invalid OTTL (unbalanced parentheses, mismatched quotes, uppercase `AND`, PromQL, Go code); they're marked and must fail to parse. Of the other 421:

- 420 recognized (99.76%): parsed, every function known. The miss is LANGUAGE.md's illustrative `sum(...)`, a function no component provides.
- 389 modeled (92.4%); 31 parse but use a lambda or an experimental `xottl` function, almost all from the xottl README. `check` fails closed on those, as decided.
- 0 parse failures.

Two named sources added nothing: SnowOps Labs has no OTTL (its traces go through Alloy with no transform or filter), and tvt-lab's Collector config has none of its own yet. The S8 lab seeds add some, and they join the corpus when a benchmark state captures them. Third-party blog posts aren't vendored (license), so they aren't in the measure.

The docs corpus is written by the people who wrote the grammar, so it's the easy case. Partner configs (anonymized, as for the PromQL and LogQL corpora) are the real test; the baseline holds until they arrive, and a drop below 0.95 still reopens this ADR.
