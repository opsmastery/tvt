# 0006. Line-level log cuts are handed to zeroreads

Date: 2026-11-09 · Status: accepted

## Context

Some of the most expensive unread logs are not whole streams but patterns inside a stream that is otherwise used: successful health checks (`route=/healthz status=200`) mixed into an ingress or API stream, for example. The tvt-lab `/healthz` scenario seeds exactly this case. There, the success lines are about 64% of the info stream's bytes.

Cutting them safely is a different problem from tvt's. It needs per-template proof that no alert, dashboard or query line filter can match the dropped lines, plus Loki regex-dialect handling and runtime tests of the generated filter. [zeroreads](https://github.com/Bisman-Singh/zeroreads) (Apache 2.0) already does this and is well tested (see the [comparison](https://claude.ai/artifact/7CHyVw2724T9u1PQSbfSGv)). ADR 0005's extractor deliberately skips line filters.

## Decision

tvt does not generate line-level log filters. It:

- ranks and acts on whole streams (route to cold storage, shorten retention), never on lines;
- reports line-level candidates: the stream, the dominant unread pattern (from Loki's patterns API), its share of bytes and monthly cost;
- points to zeroreads for the proven cut.

## Consequences

- tvt stays focused on metrics, cardinality, dashboard views and cost ranking, where zeroreads doesn't go.
- The report still captures the savings, so the business case includes line-level waste.
- Open question: integrate (emit zeroreads rules for each candidate) or just link. Revisit after the first partner sees a line-level finding.
- If zeroreads stalls or its license changes, revisit: the lab ground truth already defines the expected blast radius for a line-level cut.
