# 0010. Reduce before drop: the least-lossy order

Date: 2026-11-16 · Status: accepted (Nov 16)

## Context

A drop is the most lossy action tvt can propose, and often not the best one. A metric whose `pod` label is always aggregated away can keep its data without the label. A log stream read only by counts can become a metric. A dormant signal can keep a short history instead of none. Reviews on Nov 12 (VictoriaMetrics, the Prometheus storage calculator, Loki deduplication, monday.com, span metrics) each added such an action. They need one order, so `propose` picks the gentlest action that works and `check` can refuse a harsher one.

## Decision

1. **Actions, from least to most lossy.** For each signal, `propose` tries them in this order and picks the first one that keeps every reader's result unchanged (or within the replay tolerance in S8):

   | Signal | Order |
   | --- | --- |
   | Metrics | change the source setting (ADR 0009) → bucket label values (`status_code` to `5xx`) → remove a label nobody groups or filters on (stream aggregation `without:`, or the Alloy and Collector equivalents) → raise the scrape interval → shorter retention or a downsampling tier → reroute to the archive → drop |
   | Logs | cap a large attribute → collapse identical lines with a count (`logdedup`) → turn a stream read only by counts into a metric (count connector, Phase 2) → route levels below warn to the cheapest tier → sample 1 in N → reroute to the archive → drop |
   | Spans | cap a large attribute, and keep one copy of a value carried twice → sample (every policy keeps errors, slow requests and consequential actions) → drop |

2. **`check` enforces the order.** It refuses a drop when an earlier action in the table would keep every reader working (S8 item 3).
3. **Every action has its own guard.**
   - Label removal and bucketing: protected queries replayed with and without the change (S8 item 4).
   - Scrape interval: refused if any `rate` or `increase` window would hold fewer than four samples, or an alert would pass its detection objective (S8 item 5).
   - Retention and downsampling: refused past the longest lookback any reader uses (S8 item 5).
   - Collapse: refused until every count query over the stream is rewritten to sum the count attribute (S8 item 6).
   - Sampling and level tiers: the error-pattern guard keeps lines matching `error|exception|traceback|critical|fatal` (S8 item 2).
4. **Drops are staged.** Reroute to the archive first, drop after the quiet period (default 30 days) with no new reader. Every drop PR is generated with its revert PR (S10 item 6).
5. **Records are never sampled, collapsed or dropped** (ADR 0011). Capping a record's content attribute is allowed when its outcome fields stay.

## Consequences

- Recommendations show the action and why the less lossy ones before it didn't work, so an owner sees "label removal would break panel 3" instead of only "drop".
- Some savings are smaller than a drop would give. The report shows both numbers.
- Each action is a template per pipeline kind. S10 builds them; an action with no template for a pipeline is skipped, and the next one in the order is tried.

## Amendment: sampling after rerouting (Nov 21, 2026)

Jason's decision: for an unread stream, rerouting to a cold tier stays the default, ahead of sampling, though the logs ladder above puts sampling first. A reroute keeps every line, so an owner who finds a reader later loses nothing. Sampling is written only where a reroute can't be: an Alloy pipeline with no cold component, and only when `tiers.sample_one_in` asks for it. Records are never sampled, and error-pattern lines are always kept.
