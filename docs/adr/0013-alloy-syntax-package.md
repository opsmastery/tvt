# 0013. Read Alloy config with Grafana's syntax package; write by splicing

Date: 2026-11-16 · Status: accepted (Nov 16)

## Context

S7's Alloy writer and the rename-aware matching in S7 item 9 both need to read Alloy configuration: find `prometheus.relabel` and `loki.process` components, read their `rule` blocks and stages, and follow `forward_to` between components. The writer then adds blocks to the same files. The choice was between Grafana's own syntax package (after a license check) and a narrow parser for only the blocks tvt writes (ADR 0007, Sprint 2 planning).

A spike on Nov 16 imported `github.com/grafana/alloy/syntax` into a scratch module and into a detached copy of tvt, and ran it on tvt-lab's `alloy/config.alloy` (108 lines, 10 components, 30 comment lines).

What the spike found:

- **License and module.** Apache 2.0, a separate Go module versioned apart from Alloy itself. Its README says the submodule "does not have a stable API". Releases: v0.1.0 (Apr 2024), v0.1.1 (Jul 2026), v0.1.2 (Aug 2026, `go 1.26.7`), v0.1.3 (Nov 13, 2026, `go 1.27.3`).
- **Go version.** v0.1.2 needs exactly the `go 1.26.7` directive tvt already has. v0.1.3 raises the directive to 1.27.3, which ADR 0004 didn't allow while Go 1.26 is supported (until Go 1.28 ships, about Feb 2027). Jason chose to raise tvt's minimum to 1.27 on Nov 21, 2026, and tvt now uses v0.1.3.
- **Weight.** Importing `parser`, `printer` and `token/builder` adds 138 KB to the stripped binary (18.23 MB to 18.37 MB, +0.75%). `go.mod` gains six lines, but only two newly linked modules (`fatih/color`, `mattn/go-colorable`, through the `diag` package); `golang.org/x/sys` and `go-isatty` were already there. govulncheck reports nothing.
- **Reading works.** The parser read every component, nested block and attribute in the lab file, including the spanmetrics connector's nested `histogram { explicit { ... } }` and the relabel rule's `source_labels`, `regex` and `action`.
- **Reprinting the whole file is not safe for a PR.** `printer.Fprint` writes `alloy fmt` style: tab indentation and a blank line after every block. The lab file uses two spaces, so reprinting changed 70 lines to add one rule. Comments in the original survive a reprint, but comments in a block parsed from another source and spliced into the tree are dropped, because the AST keeps comments in `File.Comments` by position rather than on nodes.
- **The builder writes clean blocks.** `token/builder` produced a correctly aligned `rule { ... }` block from Go values, and the result parsed.

For comparison, contrib's OTTL module (ADR 0012) measured the same way adds 2.45 MB and 17 `go.mod` lines.

## Decision

1. **Read Alloy with `github.com/grafana/alloy/syntax`, pinned to v0.1.2,** using only `parser`, `ast`, `printer` (for printing single expressions) and `token/builder`. Not `vm`: tvt never evaluates Alloy.
2. **Wrap it in one package** (`pkg/propose/alloy`, used by `scan`, `propose` and `check`) so the unstable API touches one place. Golden tests on the lab config and on Alloy's own example files guard upgrades.
3. **Write by splicing bytes, never by reprinting the file.** `propose` builds the new block with `token/builder`, re-indents it to the file's own style (detected from the surrounding lines), and inserts it into the original bytes at a position taken from the AST: after the last `rule` block of a `prometheus.relabel`, or at the right point in a `loki.process` stage list. Every byte outside the inserted text is unchanged, so the PR diff is only tvt's block, inside the `tvt:begin`/`tvt:end` markers from S7 item 10.
4. **Re-parse what was written.** The edited file must parse, and its AST must equal the original's plus the inserted block. Then S7 item 5's validation runs `alloy validate` when the binary is available; it checks syntax, missing components, component name conflicts, required properties and unknown properties.
5. **Only literal values count as known.** A `regex`, `source_labels` or `target_label` that is a string or a list of strings is read. Anything else (`sys.env(...)`, a reference to another component's export, string concatenation, a function call) makes that rule an evidence gap for every signal it could touch (S7 item 9). tvt never guesses what an expression evaluates to.
6. **What tvt follows in the MVP:** all `*.alloy` files in a declared pipeline directory (Alloy loads a directory as one config), and `forward_to` chains between components in it. **Evidence gaps, not followed:** `import.git`, `import.http` and `import.string` modules, `declare` blocks, and config rendered by a chart that generates Alloy from its own values (for example the k8s-monitoring chart's `metricsTuning` and `extraMetricProcessingRules`). `import.file` inside the same repo can follow later. Alloy config stored as a string in Helm values (the `alloy` chart's `configMap.content`) is parsed and spliced inside that string.
7. **Stay on v0.1.2 until the `go` directive moves.** When Dependabot lands (S12), it ignores `github.com/grafana/alloy/syntax` minor and patch updates that raise the `go` directive. Revisit when Go 1.28 ships.

## Alternatives considered

- **A narrow parser for the blocks tvt writes.** To skip the rest of a file safely it still has to tokenize the whole language: strings with escapes, raw strings, nested objects and arrays, function calls, operators. That's most of a parser, without Grafana's tests or upkeep. Rejected.
- **Reprint the file with `printer`.** Simplest code, but it reformats the user's file and drops spliced comments. Rejected for writing; still used for single expressions.
- **Shell out to the `alloy` binary.** It has no API for the syntax tree, and partners' CI may not have it. Kept only as optional validation.
- **v0.1.3.** Same API surface for tvt's use, but needs Go 1.27.3. Deferred, not rejected.

## Consequences

- The Alloy writer is a small amount of tvt code: AST walking, the splice and the re-indent. The parsing risk sits with Grafana's tested package.
- An API change in a future syntax release costs one package's worth of fixes, caught by the golden tests.
- Partners whose Alloy comes from a generating chart, remote modules or `declare` blocks see evidence gaps until those are supported, never wrong proposals.
- ADR 0004's list of expected dependencies gains the Alloy syntax package.
