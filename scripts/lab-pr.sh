#!/usr/bin/env bash
# S7's "done when" on the live lab (tvt test plan, manual test M4): scan the
# running tvt-lab, then tvt propose --output pr opens (or updates) one draft
# pull request in tvt-lab from the branch tvt/propose. Run it on the Mac
# that runs the lab.
#
# Tokens come from the login Keychain (service tvt-lab), never flags; this
# script never prints, logs or writes them:
#
#   security add-generic-password -U -s tvt-lab -a github-token -w    # fine-grained, tvt-lab, Pull requests: read and write
#   security add-generic-password -U -s tvt-lab -a grafana-token -w   # the lab Grafana's Viewer service account
#
#   scripts/lab-pr.sh check   say what's set up and what's missing
#   scripts/lab-pr.sh         scan, then open the draft PR
#   scripts/lab-pr.sh patch   scan, then write the patch only (no GitHub
#                             token needed): the manual tests that apply a
#                             change to the lab (M3, M7) start from it
#
# The branch is pushed with the tvt-lab checkout's own Git credentials (an
# SSH remote), not with the token.
#
# The Keychain only gives tokens to the logged-in session, not to ssh, so
# over ssh run it as a launchd job. macOS also keeps launchd jobs out of
# external volumes: then point TVT_LAB_DIR at a clone in your home folder
# and TVT_LAB_LOGS at a folder there. The logs are copied into it straight
# from the containers (podman exec and podman logs, no external volume)
# just before the scan, and while the scan waits for its probe queries the
# newest probe lines are copied every 5 seconds into probes files beside
# them, so the probe proves the copy is live (S12 test plan: the copies
# used to be made by hand, hours old, and both query logs came out
# unproven). TVT_LAB_LOKI_EXCLUDE, a grep -E pattern, leaves matching Loki
# query lines out (ad hoc queries that would count as reads).
#
# TVT_LABEL_TOP (default 2000) is the scan's --label-top: the lab has about
# 1,550 metrics, and S10's bucket and label findings need per-label
# cardinality on small metrics such as coding_agent_api_requests_total,
# which the scan's default of the 50 largest leaves out.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
LAB="${TVT_LAB_DIR:-$HERE/../tvt-lab}"
# The lab's own tvt.yaml (tvt-lab #47 on) when it has one; the frozen
# fixture config before that.
CONFIG="${TVT_LAB_CONFIG:-}"
if [[ -z "$CONFIG" ]]; then
  if [[ -f "$LAB/tvt.yaml" ]]; then CONFIG="$LAB/tvt.yaml"; else CONFIG="$HERE/testdata/bench/writers-2/tvt-lab.yaml"; fi
fi
OUT="${TVT_LAB_OUT:-$HOME/tvt-lab-pr/$(date -u +%Y%m%dT%H%M%SZ)}"
REPO=github.com/Opsmastery-Summit/tvt-lab
export PATH="/opt/podman/bin:/usr/local/go/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:$PATH"

token() { security find-generic-password -s tvt-lab -a "$1" -w 2>/dev/null; }

# copy_logs DIR: the lab's logs from its containers.
copy_logs() {
  local d="$1"
  mkdir -p "$d/prometheus"
  podman exec tvt-lab-prometheus-1 cat /var/log/prometheus/query.log > "$d/prometheus/query.log.new" &&
    mv "$d/prometheus/query.log.new" "$d/prometheus/query.log"
  podman exec tvt-lab-grafana-1 sh -c 'cd /var/lib/grafana/logs && tar cf - grafana.log*' | tar xf - -C "$d"
  podman logs --since 720h tvt-lab-loki-1 2>&1 | grep 'caller=metrics.go' |
    { if [[ -n "${TVT_LAB_LOKI_EXCLUDE:-}" ]]; then grep -v -E "$TVT_LAB_LOKI_EXCLUDE"; else cat; fi; } > "$d/loki.log.new" || true
  mv "$d/loki.log.new" "$d/loki.log"
  : > "$d/prometheus/probes.log"; : > "$d/loki-probes.log"
}

# copy_probes DIR: the newest probe lines, every 5 s, until killed.
copy_probes() {
  local d="$1"
  while sleep 5; do
    podman exec tvt-lab-prometheus-1 tail -n 2000 /var/log/prometheus/query.log 2>/dev/null | grep -F tvt_query_log_probe > "$d/prometheus/probes.log.new" || true
    mv "$d/prometheus/probes.log.new" "$d/prometheus/probes.log"
    podman logs --since 5m tvt-lab-loki-1 2>&1 | grep 'caller=metrics.go' | grep -F 'source=tvt-probe' > "$d/loki-probes.log.new" || true
    mv "$d/loki-probes.log.new" "$d/loki-probes.log"
  done
}

check() {
  local ok=0 a accounts=(grafana-token)
  [[ "${MODE:-pr}" == pr ]] && accounts+=(github-token)
  for a in "${accounts[@]}"; do
    if token "$a" >/dev/null; then echo "keychain tvt-lab/$a: set"; else echo "keychain tvt-lab/$a: MISSING"; ok=1; fi
  done
  if [[ -d "$LAB/.git" ]]; then echo "tvt-lab checkout: $LAB ($(git -C "$LAB" remote get-url origin))"; else echo "tvt-lab checkout: MISSING at $LAB (set TVT_LAB_DIR)"; ok=1; fi
  if [[ -n "$(git -C "$LAB" status --porcelain -- prometheus alloy fluent-bit otelcol 2>/dev/null)" ]]; then echo "tvt-lab pipeline files have local changes: commit or revert them first"; ok=1; fi
  for u in http://localhost:3000/api/health http://localhost:9090/-/ready http://localhost:3100/loki/api/v1/status/buildinfo; do
    if curl -fsS -o /dev/null "$u"; then echo "lab: $u up"; else echo "lab: $u DOWN"; ok=1; fi
  done
  return $ok
}

run() {
  check >/dev/null || { check; exit 1; }
  local gh="" gt
  [[ "$MODE" == pr ]] && gh="$(token github-token)"
  gt="$(token grafana-token)"
  mkdir -p "$OUT"
  # The lab's logs, as tvt's e2e suite reads them: Grafana's router log out
  # of its container, Loki's query lines from its container log.
  local logs="${TVT_LAB_LOGS:-}" probes=()
  if [[ -n "$logs" ]]; then
    copy_logs "$logs"
    copy_probes "$logs" &
    TAILER=$!
    trap 'kill "${TAILER:-}" 2>/dev/null || true' EXIT
    probes=(--prom-query-log "$logs/prometheus/probes.log" --loki-query-log "$logs/loki-probes.log")
  else
    logs="$LAB/lab-logs"
    (cd "$LAB" && make -s export-logs >/dev/null)
    (cd "$LAB" && LAB_LOGS="$LAB/lab-logs" podman compose logs --no-log-prefix --since 720h loki 2>&1 | grep 'caller=metrics.go' > "$logs/loki.log" || true)
  fi
  cd "$HERE"
  make -s build
  TVT_GRAFANA_TOKEN="$gt" ./bin/tvt scan --config "$CONFIG" --db "$OUT/tvt.db" \
    --grafana http://localhost:3000 --prom http://localhost:9090 --loki http://localhost:3100 \
    --grafana-log "$logs/grafana.log*" \
    --prom-query-log "$logs/prometheus/query.log" \
    --loki-query-log "$logs/loki.log" ${probes[@]+"${probes[@]}"} \
    --label-top "${TVT_LABEL_TOP:-2000}"
  ./bin/tvt report --db "$OUT/tvt.db" --config "$CONFIG" --label-drops --top 0 --json "$OUT/report.json" --html "$OUT/report.html"
  GITHUB_TOKEN="$gh" ./bin/tvt propose --db "$OUT/tvt.db" --config "$CONFIG" --report "$OUT/report.json" \
    --checkout "$REPO=$LAB" --validate require --output "$MODE" --dir "$OUT/proposals"
  echo "scan, report and $MODE in $OUT"
}

case "${1:-run}" in
  run) MODE=pr run ;;
  patch) MODE=patch run ;;
  check) MODE="${MODE:-pr}" check ;;
  *) echo "usage: $0 [check|run|patch]" >&2; exit 2 ;;
esac
