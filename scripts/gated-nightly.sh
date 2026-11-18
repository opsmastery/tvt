#!/usr/bin/env bash
# The gated test suites, nightly on a Mac (tvt test plan, recommended
# addition 3): the real validators (TVT_TEST_VALIDATORS: promtool, alloy,
# otelcol-contrib 0.80.0 to 0.165.0, fluent-bit, kubeconform, in Podman) and,
# when helm is installed, the chart renders (TVT_TEST_HELM). CI skips both,
# so a validator image or chart change that breaks a writer shows up the next
# morning instead of at release time. No credentials are involved.
#
#   scripts/gated-nightly.sh            run once now
#   scripts/gated-nightly.sh check      say what's set up and what's missing
#   scripts/gated-nightly.sh install    install the launchd job (daily, 03:41 local)
#   scripts/gated-nightly.sh uninstall  remove it
#
# Logs go to $TVT_GATED_OUT (default ~/tvt-gated/<date>.log), kept 14 days;
# the last line says PASS or FAIL, and a failure posts a macOS notification.
#
# install copies this script to ~/tvt-gated/bin and the job tests its own
# clone in ~/tvt-gated/tvt: macOS won't let a launchd job read a checkout on
# an external volume (/Volumes/...) without Full Disk Access.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${TVT_GATED_OUT:-$HOME/tvt-gated}"
REPO="${TVT_GATED_REPO:-$OUT/tvt}"
LABEL=com.opsmastery.tvt-gated-nightly
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
KEEP_DAYS=14
export PATH="/opt/podman/bin:/usr/local/go/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:$PATH"

check() {
  local ok=0
  for t in go git podman; do
    if command -v "$t" >/dev/null; then echo "$t: $(command -v "$t")"; else echo "$t: MISSING"; ok=1; fi
  done
  if podman info >/dev/null 2>&1; then echo "podman machine: answering"; else echo "podman machine: NOT answering (validators would be skipped)"; ok=1; fi
  if command -v helm >/dev/null; then echo "helm: $(command -v helm) (chart renders run)"; else echo "helm: missing (chart renders are skipped; brew install helm to add them)"; fi
  if launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1; then echo "launchd $LABEL: installed"; else echo "launchd $LABEL: not installed"; fi
  return $ok
}

# The chart repos the render tests pull from (pinned chart versions are in
# pkg/propose/testdata). Added once, refreshed each run.
charts() {
  helm repo list 2>/dev/null | grep -q '^prometheus-community' || helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
  helm repo list 2>/dev/null | grep -q '^fluent' || helm repo add fluent https://fluent.github.io/helm-charts >/dev/null
  helm repo update prometheus-community fluent >/dev/null || echo "helm repo update failed; rendering from the cached index"
}

tests() {
  echo "tvt gated suites, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  cd "$REPO"
  git pull --ff-only --quiet || echo "git pull failed; testing the checked-out version"
  echo "commit $(git rev-parse --short HEAD)"
  local env=(TVT_TEST_VALIDATORS=1)
  if command -v helm >/dev/null; then env+=(TVT_TEST_HELM=1); charts; else echo "helm missing: chart renders skipped"; fi
  if ! podman info >/dev/null 2>&1; then echo "podman isn't answering: the validators can't run"; fi
  local rc=0
  # -p 2: fewer validator containers at once on the Podman machine.
  env "${env[@]}" go test -count=1 -p 2 -timeout 45m ./... || rc=$?
  if [[ $rc -eq 0 ]]; then echo "PASS"; else echo "FAIL (go test exit $rc)"; fi
  return $rc
}

run() {
  mkdir -p "$OUT"
  local log rc=0
  log="$OUT/$(date -u +%Y-%m-%d).log"
  tests >"$log" 2>&1 || rc=$?
  find "$OUT" -maxdepth 1 -name '*.log' -mtime +"$KEEP_DAYS" -delete
  tail -1 "$log"
  if [[ $rc -ne 0 ]]; then
    osascript -e 'display notification "Gated test suites failed: see ~/tvt-gated" with title "tvt"' >/dev/null 2>&1 || true
  fi
  return $rc
}

install() {
  mkdir -p "$HOME/Library/LaunchAgents" "$OUT/bin"
  if [[ ! -d "$REPO/.git" ]]; then
    git clone --quiet "$(git -C "$HERE" remote get-url origin)" "$REPO"
  fi
  cp "$HERE/scripts/gated-nightly.sh" "$OUT/bin/gated-nightly.sh"
  chmod +x "$OUT/bin/gated-nightly.sh"
  cat >"$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array><string>$OUT/bin/gated-nightly.sh</string></array>
  <key>StartCalendarInterval</key>
  <dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>41</integer></dict>
  <key>StandardOutPath</key><string>$OUT/launchd.log</string>
  <key>StandardErrorPath</key><string>$OUT/launchd.log</string>
</dict>
</plist>
EOF
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$PLIST"
  echo "installed $PLIST (daily at 03:41 local; tests $REPO; logs in $OUT)"
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
