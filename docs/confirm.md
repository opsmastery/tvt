# tvt confirm

`tvt confirm` records an owner's decision about a recommendation in the pipeline repo, so the decision travels with the change and `tvt check` can enforce it. It writes `.tvt/decisions.yaml` in the checkout you point it at; you commit that file on the PR's branch. It needs no credentials and talks to nothing.

```sh
# On the tvt PR's branch, in the pipeline repo:
tvt confirm --report report.json --owner group:default/shop-team drop:prom:metric:shop_legacy_queue_depth
tvt confirm --report report.json --owner @jasonvance --all          # everything in .tvt/proposal.json they own
tvt confirm --report report.json --owner @jasonvance --dismiss "used by the quarterly audit" drop:prom:metric:shop_audit_total
tvt confirm --report report.json --owner @jasonvance --seasonal nov,dec drop:prom:metric:shop_peak_queue
git add .tvt/decisions.yaml && git commit -m "tvt: confirm" && git push
```

## Flags

| Flag | Default | What |
| --- | --- | --- |
| `--report` | (required) | The report JSON the owner reviewed (`tvt report --json`). Each confirm records the evidence hash this report shows |
| `--owner` | (required) | Who decides: the owner the report names, or your handle |
| `--dir` | `.` | The pipeline repo's checkout, on the PR's branch |
| `--all` | off | Confirm every recommendation in the repo's `.tvt/proposal.json` that `--owner` owns |
| `--dismiss REASON` | | Dismiss instead. tvt won't propose it again before `--review` |
| `--keep REASON` | | Keep instead (for example: read at year end) |
| `--seasonal MONTHS` | | It's read only in these months (`11,12` or `nov,dec`): keep it until after the next season, then tvt asks again |
| `--review` | 90 days | When to ask again after a dismiss or keep (`YYYY-MM-DD`) |

Name recommendations by their ID as the report shows it (`drop:prom:metric:...`) or by signal ID. A stream selector holds spaces, so quote it, and pass many IDs NUL-separated with `xargs -0`.

## Before you confirm

The owner answers one more question before each confirm (`tvt confirm -h` asks it too): is this signal read only at certain times of year, such as a year-end report or a holiday peak? A 30-day window can't see a reader that runs once a year. If it is, record `--seasonal MONTHS` instead of a confirm.

## What it records

Each decision in `.tvt/decisions.yaml` holds the signal, the action (`confirm`, `dismiss` or `keep`), the owner, the reason, the time, the review date, the owner tvt named at the time, and for a confirm the evidence hash: a digest of the recommendation's readers and open gaps, without query times or cost.

What happens next:

- **tvt check** refuses a drop with no matching confirm (`unconfirmed_drop`). Once the evidence changes after a confirm (a new reader, a gap reopened), the confirm no longer matches and check refuses it as `stale_confirmation`, naming what changed. See [check.md](check.md).
- **tvt report** reads the decisions from each pipeline checkout. A confirmed row shows as `confirmed`. A dismissed or kept row is held back until its review date, or until its evidence changes. Each decision is listed as `current`, `reopened`, `expired` or `orphaned`, so nothing persists or expires silently. See [report.md](report.md).
- **The PR** stays a draft: tvt opens it as one and never marks it ready. Mark it ready for review once check passes with every drop confirmed.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Recorded |
| 1 | Error: the report, the checkout or a named recommendation couldn't be read or found |
| 2 | Bad flags |
