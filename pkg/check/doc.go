// Package check is the pre-merge safety gate behind `tvt check`.
//
// It evaluates a proposed pipeline config (Prometheus relabel rules, OTel
// Collector filter/transform processors) against the signal set in the
// committed protection manifest (.tvt/protect.json), computes exactly which
// signals and labels the change would drop, and fails if any of them is used
// by an alert, SLO, declared external consumer or viewed dashboard.
//
// It evaluates configs rather than guessing their intent, and fails closed:
// a config it cannot evaluate is a failure, not a pass.
package check
