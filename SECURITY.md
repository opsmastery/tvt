# Security policy

## Reporting a vulnerability

Please don't open a public issue. Report it privately through GitHub: the repository's **Security** tab, then **Report a vulnerability**. You'll get an acknowledgement within three working days and a first assessment within ten.

Include the tvt version (`tvt version`), the command you ran, and what you saw. Leave tokens and real telemetry out of the report; a redacted config or a synthetic reproduction is enough.

## Supported versions

Only the latest release, pre-releases included, gets fixes until `v1.0.0`.

## What tvt touches

tvt is built so that a compromise of tvt can't change your telemetry:

- **Backends are read-only.** Grafana, Prometheus, Mimir and Loki are read through their read APIs, and an OpenTelemetry Collector only through its own `/metrics` endpoint. The only queries tvt runs are its probe query, Grafana Usage Insights, and one tagged Loki level query (`Source=tvt-levels`). CI fails any collector test that sends a write.
- **The only writes are Git.** `tvt propose` pushes a branch and opens a draft pull request in the pipeline repos you map. Nothing merges without your review, and `tvt check` blocks a change that would break an alert, a recording rule, an SLO or a viewed dashboard.
- **Names and sizes, not content.** tvt reads metric, label, attribute and stream names, cardinalities and byte volumes. It doesn't read log lines or span attribute values. For the largest log streams it reads Loki's detected patterns (line templates with the variable parts replaced by `<_>`) and per-level byte totals. Label values are read only to count them and to group status-code labels into classes. Patterns and names land in the local database and report, so treat those files like the backend they came from.
- **Credentials come from the environment** (`TVT_GRAFANA_TOKEN`, `TVT_PROM_TOKEN`, `TVT_LOKI_TOKEN`, `GITHUB_TOKEN`), never from flags, and are only sent over https or to a loopback host unless you pass `--allow-insecure-token`. They're never written to the database, the report or a PR.
- **Nothing leaves by default.** tvt doesn't phone home, and with no `OTEL_*` settings it exports no traces, metrics or logs of its own; data only goes to the backends you name and your Git host. What stays on disk is the scan database and the reports you write. Git work happens on the machine tvt runs on.

## Release integrity

Release archives are built reproducibly (CI builds every target twice and compares the hashes), and `checksums.txt` is signed with cosign (keyless, Sigstore). [docs/install.md](docs/install.md) shows how to verify a download before you run it. Dependencies are scanned with govulncheck and the full history with gitleaks on every PR, and Dependabot proposes updates weekly.
