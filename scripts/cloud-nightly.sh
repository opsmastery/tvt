#!/usr/bin/env bash
# The nightly scan and report of the Grafana Cloud test stack
# (docs/grafana-cloud.md), run by launchd on a Mac instead of GitHub
# Actions, so no credential lives in GitHub.
#
# Tokens come from the login Keychain (service tvt-cloud); URLs and user
# IDs, which aren't secret, from ~/.config/tvt/cloud.env. This script never
# prints, logs or writes a token.
#
#   scripts/cloud-nightly.sh            run once now
#   scripts/cloud-nightly.sh check      say what's set up and what's missing
#   scripts/cloud-nightly.sh install    install the launchd job (daily, 02:17 local)
#   scripts/cloud-nightly.sh uninstall  remove it
#
# Add the two tokens yourself; security prompts for each value, so it
# isn't in your shell history:
#
#   security add-generic-password -U -s tvt-cloud -a grafana-token -w
#   security add-generic-password -U -s tvt-cloud -a cloud-token -w
#
# Reports go to $TVT_CLOUD_OUT (default ~/tvt-cloud/<date>/), kept 14 days.
#
# install copies this script to ~/tvt-cloud/bin and the job builds from its
# own clone in ~/tvt-cloud/tvt: macOS won't let a launchd job read a
# checkout on an external volume (/Volumes/...) without Full Disk Access.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${TVT_CLOUD_OUT:-$HOME/tvt-cloud}"
REPO="${TVT_CLOUD_REPO:-$OUT/tvt}"
LABEL=com.opsmastery.tvt-cloud-nightly
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
ENVFILE="${TVT_CLOUD_ENV:-$HOME/.config/tvt/cloud.env}"
KEEP_DAYS=14
export PATH="/usr/local/go/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:$PATH"

token() { security find-generic-password -s tvt-cloud -a "$1" -w 2>/dev/null; }

check() {
  local ok=0
  for a in grafana-token cloud-token; do
    if token "$a" >/dev/null; then echo "keychain tvt-cloud/$a: set"; else echo "keychain tvt-cloud/$a: MISSING"; ok=1; fi
  done
  if [[ -f "$ENVFILE" ]]; then
    # shellcheck disable=SC1090
    source "$ENVFILE"
    for v in GRAFANA_URL PROM_URL PROM_USER LOKI_URL LOKI_USER; do
      if [[ -n "${!v:-}" ]]; then echo "$ENVFILE $v: set"; else echo "$ENVFILE $v: MISSING"; ok=1; fi
    done
  else
    echo "$ENVFILE: MISSING (copy scripts/cloud.env.example)"; ok=1
  fi
  if launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1; then echo "launchd $LABEL: installed"; else echo "launchd $LABEL: not installed"; fi
  return $ok
}

run() {
  # shellcheck disable=SC1090
  source "$ENVFILE"
  local gt ct
  gt="$(token grafana-token)" || { echo "tvt-cloud/grafana-token isn't in the Keychain" >&2; exit 1; }
  ct="$(token cloud-token)" || { echo "tvt-cloud/cloud-token isn't in the Keychain" >&2; exit 1; }
  local dir="$OUT/$(date -u +%Y-%m-%d)"
  mkdir -p "$dir"
  cd "$REPO"
  git pull --ff-only --quiet || echo "git pull failed; scanning with the checked-out version" >&2
  make build >/dev/null
  TVT_GRAFANA_TOKEN="$gt" TVT_PROM_USER="$PROM_USER" TVT_PROM_TOKEN="$ct" \
  TVT_LOKI_USER="$LOKI_USER" TVT_LOKI_TOKEN="$ct" \
    ./bin/tvt scan --grafana-edition cloud --grafana "$GRAFANA_URL" \
      --mimir "$PROM_URL" --loki "$LOKI_URL" \
      --config examples/tvt.cloud.yaml --db "$dir/cloud.db"
  ./bin/tvt report --db "$dir/cloud.db" --json "$dir/report.json" --html "$dir/report.html"
  find "$OUT" -mindepth 1 -maxdepth 1 -type d -mtime +"$KEEP_DAYS" -exec rm -rf {} +
  echo "report written to $dir"
}

install() {
  mkdir -p "$HOME/Library/LaunchAgents" "$OUT/bin"
  if [[ ! -d "$REPO/.git" ]]; then
    git clone --quiet "$(git -C "$HERE" remote get-url origin)" "$REPO"
  fi
  cp "$HERE/scripts/cloud-nightly.sh" "$OUT/bin/cloud-nightly.sh"
  chmod +x "$OUT/bin/cloud-nightly.sh"
  cat >"$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array><string>$OUT/bin/cloud-nightly.sh</string></array>
  <key>StartCalendarInterval</key>
  <dict><key>Hour</key><integer>2</integer><key>Minute</key><integer>17</integer></dict>
  <key>StandardOutPath</key><string>$OUT/nightly.log</string>
  <key>StandardErrorPath</key><string>$OUT/nightly.log</string>
</dict>
</plist>
EOF
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$PLIST"
  echo "installed $PLIST (daily at 02:17 local; builds from $REPO; log $OUT/nightly.log)"
}

uninstall() {
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  rm -f "$PLIST"
  echo "removed $LABEL"
}

case "${1:-run}" in
  run) run ;;
  check) check ;;
  install) install ;;
  uninstall) uninstall ;;
  *) echo "usage: $0 [run|check|install|uninstall]" >&2; exit 2 ;;
esac
