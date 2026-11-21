# Install

tvt is one static binary for Linux, macOS and Windows (amd64 and arm64). Download a release, verify it, then run it.

## 1. Download

Pick the archive for your machine from the [releases page](https://github.com/Opsmastery-Summit/tvt/releases), or use `gh` (it works while the repository is private, with your own GitHub login):

```sh
VERSION=0.1.0-rc1          # the tag without its leading v
OS=darwin                  # or linux
ARCH=arm64                 # or amd64
gh release download "v$VERSION" --repo Opsmastery-Summit/tvt \
  --pattern "tvt_${VERSION}_${OS}_${ARCH}.tar.gz" \
  --pattern 'checksums.txt*'
```

You now have the archive, `checksums.txt` and `checksums.txt.sigstore.json` (the signature and its certificate in one Sigstore bundle). Releases up to v0.1.0-rc1 have `checksums.txt.sig` and `checksums.txt.pem` instead; verify those with `--certificate checksums.txt.pem --signature checksums.txt.sig` in place of `--bundle`.

## 2. Verify

Install [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) (`brew install cosign` on macOS). Check that `checksums.txt` was signed by this repository's release workflow, then that the archive matches it:

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/Opsmastery-Summit/tvt/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
# Verified OK

shasum -a 256 --ignore-missing -c checksums.txt     # macOS
sha256sum --ignore-missing -c checksums.txt         # Linux
# tvt_0.1.0-rc1_darwin_arm64.tar.gz: OK
```

Stop if either check fails: the archive isn't the one the release workflow built.

Builds are reproducible, so you can also rebuild a tag yourself and compare. On a clean checkout of the tag, `goreleaser build --clean --single-target` (no `--snapshot`, so the version matches) writes a binary under `dist/` with the same SHA-256 as the `tvt` inside the release archive. Use the Go version in `go.mod`.

## 3. Install

```sh
tar -xzf "tvt_${VERSION}_${OS}_${ARCH}.tar.gz" tvt
sudo install -m 0755 tvt /usr/local/bin/tvt    # or anywhere on your PATH
tvt version
# tvt 0.1.0-rc1 (commit abc1234, built 2026-11-21T10:00:00Z)
```

On macOS, a file downloaded with a browser is quarantined and Gatekeeper refuses the unsigned binary. Files fetched with `gh` or `curl` aren't. For a browser download, after verifying it:

```sh
xattr -d com.apple.quarantine tvt
```

On Windows, unzip `tvt_${VERSION}_windows_amd64.zip` and run `tvt.exe version`. Verify with `cosign` the same way, and `Get-FileHash -Algorithm SHA256` for the archive.

From source, with Go 1.26 or later: `go install github.com/Opsmastery-Summit/tvt/cmd/tvt@latest` (the version shows as `dev`).

## 4. First run

tvt needs read access to Grafana and to your metrics or logs backend. Tokens come from the environment, never from flags. `tvt init` checks the access and the logs first ([init.md](init.md)):

```sh
export TVT_GRAFANA_TOKEN=...      # Grafana service account, Viewer role
tvt init --grafana https://grafana.example.com --prom https://prometheus.example.com
tvt scan --config tvt.yaml --grafana https://grafana.example.com --prom https://prometheus.example.com
tvt report --html report.html --json report.json
open report.html                  # macOS; xdg-open on Linux
```

The report is one self-contained HTML file with no scripts or external assets. See [scan.md](scan.md) for the evidence a scan reads and [report.md](report.md) for what the report shows.

To try it without your own stack, run [tvt-lab](https://github.com/Opsmastery-Summit/tvt-lab), a local Grafana stack seeded with known waste and known readers.

## Uninstall

```sh
sudo rm /usr/local/bin/tvt
```

tvt keeps nothing outside the files you name: the scan database (`tvt.db` by default) and the reports.
