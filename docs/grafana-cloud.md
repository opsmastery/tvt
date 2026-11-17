# tvt on Grafana Cloud

What tvt reads on a Grafana Cloud stack, confirmed on the test stack on Nov 16, 2026 (a trial stack, Grafana 12, Mimir and Loki on the `prod-us-west-0` cluster). Sprint 2 item 11, S6 item 6.

## Running a scan

```sh
export TVT_GRAFANA_TOKEN=...   # Cloud Grafana service account token (glsa_), Viewer
export TVT_PROM_USER=3775056   # the stack's metrics instance ID
export TVT_PROM_TOKEN=...      # Cloud access policy token (glc_): metrics:read, logs:read, rules:read
export TVT_LOKI_USER=1883196   # the stack's logs instance ID
export TVT_LOKI_TOKEN=$TVT_PROM_TOKEN

tvt scan --grafana-edition cloud \
  --grafana https://<stack>.grafana.net \
  --mimir https://prometheus-prod-67-prod-us-west-0.grafana.net/api/prom \
  --loki https://logs-prod-021.grafana.net \
  --config examples/tvt.cloud.yaml
tvt report
```

With `TVT_PROM_USER` or `TVT_LOKI_USER` set, the token goes out as HTTP basic auth (`user:token`), which is what Cloud's Prometheus and Loki endpoints take; without a user it stays a bearer token. `--mimir` takes the Cloud URL as it is: `/api/prom` is already the API root. The instance IDs and URLs are on the stack's Details page in the Cloud portal, or in the `grafanacloud-<stack>-prom` and `-logs` data sources (Basic authentication, User).

The Grafana token and the access policy token must come from the same stack and org as the URLs. A token from another stack gets 401 on every call, with no other hint.

## What each source gives

| Evidence | OSS | Grafana Cloud |
| --- | --- | --- |
| Cardinality | TSDB status, series API | Mimir cardinality API (`/api/prom/api/v1/cardinality/...`), enabled on Cloud |
| Rules | rules APIs | `/api/prom/api/v1/rules`, Loki rules, and Grafana-managed rules from the Grafana API (Asserts and Knowledge Graph install hundreds) |
| Dashboard views | Grafana router log (`--grafana-log`) | Usage Insights `dashboard-view` events |
| Query text | Prometheus, Mimir, Loki query logs | none: see the gap below |
| Grafana SLO | plugin API | same (`/api/plugins/grafana-slo-app/resources/v1/slo`) |

## Usage Insights

Usage Insights is a Loki data source, `grafanacloud-<stack>-usage-insights`, with the fixed uid `grafanacloud-usage-insights`. tvt reads it through Grafana's data source proxy with the Grafana token (`/api/datasources/proxy/uid/grafanacloud-usage-insights/loki/api/v1/query_range`), a day per request, paged forward. This is the one query tvt sends through the data source proxy, the exception to invariant 4: it reads Grafana's own usage events, never a signal of the user's. Requests carry `X-Query-Tags: Source=tvt`, and the test server refuses any other proxy query.

Stream labels: `instance_type` (`grafana`, `metrics`, `logs`), `instance_id`, `org_id`, `service_name`. Lines are logfmt.

- `instance_type="grafana"`, `eventName=dashboard-view`: one per dashboard load in the UI, with `dashboardUid`, `dashboardName`, `folderName`, `orgId`, `username`, `timestamp`. tvt's `ViewSource` (`grafana.UsageInsights`).
- `eventName=data-request`: one per panel query, with `dashboardUid`, `panelId`, `panelName`, `datasourceUid`, `datasourceType`, `source` (`dashboard`, `unknown`). Not read yet; it could attribute panel queries to the data source they run on.
- `instance_type="metrics"` and `"logs"`: one per request to Mimir or Loki, with `path`, `query_hash`, `samples_read`, `status`, `user_agent`, and for Grafana-managed rules `source=grafana-alert`, `rule_name`, `rule_type`, `rule_folder`. There is no query text, only a hash.

Views by tvt's own service account are excluded, as with the router log. Coverage is the share of the window after the first Grafana event Usage Insights holds: a new stack covers only part of the window and every candidate's confidence drops with it.

Usage Insights is not on the free tier. A free stack has no view evidence, which the report lists as a gap.

## The gap Cloud leaves

Queries sent straight to Cloud's Mimir and Loki APIs (scripts, agents, other API clients) are logged with a `query_hash` and no text, so tvt can't see what they read. A Cloud scan stores this as the source `grafana cloud direct queries`, with the gap on every row it affects; it is left out of log coverage, since it is a gap rather than a log. Grafana-managed rules aren't affected: tvt reads their definitions.

## Adaptive Metrics

`/aggregations/recommendations` and `/aggregations/rules` on the Prometheus host answer the access policy token (HTTP 200), but a new stack returns empty lists: recommendations need days of usage. Per-metric usage counts from Adaptive Metrics as query evidence are still to do, once the test stack has recommendations.

## What the first scans found

The first scan of the test stack took over five minutes and stored 3.27 million references for 1,032 consumers. Three causes, all fixed:

1. **Recording-rule chains.** Following chains per reader re-walked shared subtrees whenever a label set below grew. Each rule's inputs are now computed once (a closure, to a fixpoint) and reused; the lab gives identical references.
2. **Label-only selectors.** Asserts rules select metrics by label (`rate({asserts_env!="", asserts_metric_request="total", __name__!~"ALERTS.*|.+:.+"}[5m])`). tvt read those as every metric, so 1,135 signals were hard keeps. The scan now asks the backend which metrics carry those labels (`/api/v1/label/__name__/values?match[]=...`, a metadata read) and resolves the selector to them.
3. **Value variables.** The Billing dashboard uses `query_result()` variables as numbers (`- (9999999999 * $is_customer_of_partner)`). tvt read a bare variable as a metric name with unknown values, so every metric. A `query_result()` variable with no regex, or with a regex that captures the sample value (`{} (.*) .*`), now names no metric.

After the fixes: 28,088 references, 356 hard keeps, a scan in about 80 seconds.

Two Cloud-specific readings to keep in mind:

- Rules evaluated outside the stack aren't readers there. kube-prometheus-stack's rules run in the cluster's Prometheus; their outputs reach Cloud by remote write, and in Cloud nothing reads them, so they are candidates. That's correct for the Cloud bill, but the cluster still evaluates them.
- Panels on a different Prometheus data source (the Billing dashboard's `grafanacloud-usage`) are still resolved against the scanned Mimir. Their metrics don't exist there, so they cost nothing, but mapping each panel to the backend its data source points at is still to do.

## Nightly scan

`scripts/cloud-nightly.sh` runs a scan and report against the test stack every night, from launchd on a Mac (the lab host), so no credential lives in GitHub. The two tokens are in the login Keychain under service `tvt-cloud` (accounts `grafana-token` and `cloud-token`); add them with `security add-generic-password -U -s tvt-cloud -a <account> -w`, which prompts for the value. The URLs and user IDs, which aren't secret, are in `~/.config/tvt/cloud.env` (see `scripts/cloud.env.example`). `scripts/cloud-nightly.sh check` says what's missing; `install` adds the launchd job (daily at 02:17 local, log `~/tvt-cloud/nightly.log`); reports go to `~/tvt-cloud/<date>/` and are kept 14 days. A LaunchAgent reads the login Keychain only while the user is logged in, so the Mac needs automatic login after a restart.
