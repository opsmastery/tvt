// Package propose turns ranked recommendations into pipeline config:
// Prometheus metric_relabel_configs (drop, labeldrop), OTel Collector
// filter/transform processors, and log routing to cold storage. Vector and
// Cribl output come in Phase 3.
//
// Every generated change carries its evidence and blast radius so the PR
// description can explain it.
package propose
