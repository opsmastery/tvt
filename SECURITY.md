# Security policy

## Reporting a vulnerability

Please don't open a public issue. Report it privately through GitHub: the repository's **Security** tab, then **Report a vulnerability**. You'll get an acknowledgement within three working days and a first assessment within ten.

Include the tvt version (`tvt version`), the command you ran, and what you saw. Leave tokens and real telemetry out of the report; a redacted config or a synthetic reproduction is enough.

## Supported versions

Only the latest release, pre-releases included, gets fixes until `v1.0.0`.

## What tvt touches

tvt is built so that a compromise of tvt can't change your telemetry:

- **Backends are read-only.** Grafana, Prometheus, Mimir and Loki are read through their read APIs, and an OpenTelemetry Collector only through its own `/metrics` endpoint. The only queries tvt runs are its probe query, Grafana Usage Insights, a tagged Loki level query (`Source=tvt-levels`) and a tagged sample of the largest streams' recent lines (`Source=tvt-attrs`). CI fails any collector test that sends a write.
- **The only writes are Git.** `tvt propose` pushes a branch and opens a draft pull request in the pipeline repos you map. Nothing merges without your review, and `tvt check` blocks a change that would break an alert, a recording rule, an SLO or a viewed dashboard.
- **Names and sizes, not content.** tvt reads metric, label, attribute and stream names, cardinalities and byte volumes. It doesn't read span attribute values. For the largest log streams it reads Loki's detected patterns (line templates with the variable parts replaced by `<_>`), per-level byte totals, and a sample of up to 1,000 recent lines each, from which it keeps only key names, sizes and a count of distinct values by hash: the lines and values are dropped in memory, never stored or printed. Label values are read only to count them and to group status-code labels into classes. Patterns and names land in the local database and report, so treat those files like the backend they came from.
- **Credentials come from the environment** (`TVT_GRAFANA_TOKEN`, `TVT_PROM_TOKEN`, `TVT_LOKI_TOKEN`, `GITHUB_TOKEN`), never from flags, and are only sent over https or to a loopback host unless you pass `--allow-insecure-token`. They're never written to the database, the report or a PR.
- **Repo paths and branch names are checked before use.** Pipeline files in `tvt.yaml` and in a committed `.tvt/protect.json` must be relative paths inside the repo (no absolute paths or `..`), and branch names can't start with `-`, so a config or manifest can't make tvt read or write outside a checkout or pass an option to git.
- **Runbook fetching is off by default.** With `runbooks.fetch: true`, tvt GETs the runbook links in your alerts and dashboards from wherever it runs, so anyone who can set a runbook URL can point tvt at a host it can reach. Prefer `runbooks.mirrors`.
- **Nothing leaves by default.** tvt doesn't phone home, and with no `OTEL_*` settings it exports no traces, metrics or logs of its own; data only goes to the backends you name and your Git host. What stays on disk is the scan database and the reports you write. Git work happens on the machine tvt runs on.

## Release integrity

Release archives are built reproducibly (CI builds every target twice and compares the hashes), and `checksums.txt` is signed with cosign (keyless, Sigstore). [docs/install.md](docs/install.md) shows how to verify a download before you run it. Dependencies are scanned with govulncheck and the full history with gitleaks on every PR, and Dependabot proposes updates weekly.
