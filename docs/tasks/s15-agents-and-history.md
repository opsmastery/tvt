# S15: agents and history

**Status:** Planned.

**Done when:** `--as-of` shows each lab state with no leakage, and MCP and CLI answers match.

## Before you start

Continuous-mode work pulled forward from the roadmap's "Next" section; it starts once the MVP's code is complete. All findings are report only with ready-to-apply snippets: tvt never edits a rule, and every signal an alert reads stays a hard keep. Read `CLAUDE.md`, `docs/tasks/README.md` and `docs/architecture.md` first.

- tvt-lab's S15 seeds (`lab-backlog.md`).

## Must have (in this order)

1. **Read-only MCP server** over the JSON report and usage graph: `why_kept`, `consumers_of`, `savings_if_dropped`.
2. **A tvt agent skill** (`SKILL.md`): how an agent uses tvt through its CLI or MCP tools, and what it must never do (write config without a reviewed PR).
3. **Contract test:** the same question gives the same entities, evidence, time boundary and confidence through the CLI, the report and MCP.
4. **`tvt report --as-of <scan>`:** rebuild a report from a stored scan using only evidence observed up to it, with the cut-off shown.
5. **Agent workflows are declared readers.** Agent skill files and alert-triggered workflow definitions that run queries are parsed like runbook steps: declared dependencies, so hard keeps.
6. **Readers broken by a release.** Comparing scans, flag any panel, rule or alert whose metric, label or stream disappeared or was renamed between two scans, with the release where tvt can see one. A reliability finding, not a saving.
7. **Agent audit evidence survives the cuts.** Agent telemetry on the GenAI conventions is usage evidence: a telemetry-reading tool call (`gen_ai.operation.name=execute_tool` for a query tool, or its `tool_result` log) names the query an agent ran in `tool_parameters` or `full_command`. Read those queries as agent reads (client `agent`). Reduction guides (the CNCF post on filtering agent telemetry, Oct 22, 2026) delete those attributes and drop successful tool spans, which erases the evidence: `propose` caps a telemetry-reading tool's query argument (truncate with a marker) or keeps the metric and stream names extracted from it, and `check` refuses a rule that removes it from both the span and the log.
