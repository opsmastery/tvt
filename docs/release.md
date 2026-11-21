# Release checklist

A `v*` tag runs `.github/workflows/release.yml`: lint, the race suite, then goreleaser builds archives for Linux, macOS and Windows (amd64 and arm64), writes `checksums.txt`, signs it with cosign (keyless, through the workflow's GitHub OIDC identity) and publishes the GitHub release. A tag with a suffix (`v0.1.0-rc1`) publishes a pre-release.

## Before the tag

1. **main is green.** Every job on the last main commit passed: `test`, `benchmark`, `reproducible`, `release-config`, `vulncheck`, `secrets`.
2. **No open Dependabot or govulncheck finding** that affects a shipped package. Merge or dismiss each with a reason.
3. **The benchmark baseline is current.** `make bench` passes, and any change to `testdata/bench/baseline.json` since the last tag was reviewed on purpose.
4. **The live lab agrees.** On the Mac mini, `./scripts/lab-pr.sh patch` runs a scan, a report and a propose against the lab without error, and `tvt check` passes the patch. Note anything new in its gaps.
5. **Docs match the binary.** Every command's `-h` matches its page in `docs/` (scan, report, why, propose, check, confirm, version), and the README's command list is current.
6. **The schema is frozen.** If `SchemaVersion` moved since the last tag, `pkg/store/testdata/schema/vN.sql` exists and the upgrade test passes from the previous release's version.
7. **Release notes.** Write the highlights, any schema upgrade, and any change to what tvt reads (a new query, endpoint or log) at the top of the release, since goreleaser's changelog is commit titles only.

## Tag

```sh
git checkout main && git pull --ff-only
git tag -a v0.1.0-rc1 -m "v0.1.0-rc1"
git push origin v0.1.0-rc1
gh run watch "$(gh run list --workflow release --limit 1 --json databaseId -q '.[0].databaseId')"
```

Never move or reuse a tag. A bad release gets a new tag (`-rc2`), and the bad one is marked in its notes.

## After the tag

1. **Verify the release as a user would**, following [install.md](install.md) on a clean machine for each OS you claim: a fresh macOS user and a fresh Linux VM.
   - `cosign verify-blob` on `checksums.txt` prints `Verified OK`.
   - `sha256sum --ignore-missing -c checksums.txt` passes for the archive.
   - `tvt version` prints the tag and commit.
   - Scan the lab, write `report.html`, and open it in a browser. Look at it; don't only grep it.
2. **Reproduce one binary.** On a clean checkout of the tag, `goreleaser build --clean --single-target` gives the same SHA-256 as the `tvt` in that platform's release archive.
3. **Record it.** Note the machines, OS versions and results in the sprint's task doc.
