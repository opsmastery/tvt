#!/bin/sh
# Capture real API responses from a running tvt-lab into testdata/lab/, so
# collectors can be tested offline against actual backend output.
#
#   ./scripts/capture-fixtures.sh            # defaults to the local lab
#   PROM=http://prom:9090 ./scripts/capture-fixtures.sh
#   SECTIONS=mimir ./scripts/capture-fixtures.sh   # only some backends
#
# Mimir is captured when it answers on $MIMIR (tvt-lab: make up-mimir).
#
# Only point this at the lab or at a partner environment with their consent:
# fixtures contain metric names, rule expressions and dashboard JSON.
set -eu

PROM=${PROM:-http://localhost:9090}
LOKI=${LOKI:-http://localhost:3100}
GRAFANA=${GRAFANA:-http://localhost:3000}
GRAFANA_AUTH=${GRAFANA_AUTH:-admin:admin}
MIMIR=${MIMIR:-http://localhost:9009}
SECTIONS=${SECTIONS:-prometheus loki grafana mimir}
LAB_DIR=${LAB_DIR:-../tvt-lab}
OUT=${OUT:-testdata/lab}

mkdir -p "$OUT/prometheus" "$OUT/loki" "$OUT/grafana" "$OUT/mimir"
get() { # url outfile [curl args...]
  url=$1; out=$2; shift 2
  if curl -fsS "$@" "$url" -o "$out"; then echo "  ok  $out"; else echo "  FAIL $url" >&2; fi
}
want() { case " $SECTIONS " in *" $1 "*) return 0 ;; esac; return 1; }
# tvt-lab's compose file requires LAB_LOGS (its Makefile sets it); without
# it every podman compose command fails.
LAB_LOGS=${LAB_LOGS:-$(cd "$LAB_DIR" && pwd)/lab-logs}
export LAB_LOGS
compose() { (cd "$LAB_DIR" && podman compose "$@"); }
# Logs are captured into $TMP and kept only when the capture holds lines: a
# failed or empty capture must never wipe the pinned fixture the golden
# tests read.
TMP="$OUT/.capture.tmp"
trap 'rm -f "$TMP"' EXIT
keep() { # fixture
  if [ -s "$TMP" ]; then mv "$TMP" "$1"; echo "  ok  $1"
  else rm -f "$TMP"; echo "  FAIL $1: nothing captured, kept the old fixture (is LAB_DIR=$LAB_DIR right?)" >&2; fi
}

if want prometheus; then
echo "Prometheus ($PROM)"
get "$PROM/api/v1/status/tsdb?limit=100000" "$OUT/prometheus/tsdb.json"
get "$PROM/api/v1/rules"                  "$OUT/prometheus/rules.json"
get "$PROM/api/v1/label/__name__/values"  "$OUT/prometheus/metric_names.json"
get "$PROM/api/v1/status/buildinfo"       "$OUT/prometheus/buildinfo.json"
get "$PROM/api/v1/targets?state=active"  "$OUT/prometheus/targets.json"           # scrape interval per job (S3 costs)
get "$PROM/api/v1/targets/metadata"       "$OUT/prometheus/targets_metadata.json"  # metric family -> job, type
get "$PROM/api/v1/status/flags"           "$OUT/prometheus/flags.json"             # exemplar-storage
get "$PROM/metrics"                       "$OUT/prometheus/metrics.txt"            # its own TSDB stats (self-hosted card)
get "$PROM/api/v1/series?match[]=shop_cart_operations_total&limit=200" "$OUT/prometheus/series_cart_ops.json"
# The query log is on the host (tvt-lab mounts it at lab-logs/prometheus);
# the copy inside the container stopped when the lab moved it.
tail -n 500 "$LAB_LOGS/prometheus/query.log" > "$TMP" 2>/dev/null || :
keep "$OUT/prometheus/query.log"
# query_agents.log (S3 items 5-6) is a hand-picked window of the mounted
# log ($LAB_DIR/lab-logs/prometheus/query.log, rule evaluations dropped)
# holding the agents' queries and the Grafana-managed rule's evaluations:
# the agents run every 15 minutes, so the last 500 lines rarely have them.
# Recapture it by hand when those seeds change.
fi

if want loki; then
echo "Loki ($LOKI)"
get "$LOKI/loki/api/v1/rules" "$OUT/loki/rules.yaml"
get "$LOKI/prometheus/api/v1/rules" "$OUT/loki/prom_rules.json"  # effective group intervals
get "$LOKI/loki/api/v1/index/volume" "$OUT/loki/volume_by_stream.json" -G \
  --data-urlencode 'query={app=~".+"}' --data-urlencode 'since=24h' --data-urlencode 'aggregateBy=series' \
  --data-urlencode 'targetLabels=app,component,level'
get "$LOKI/loki/api/v1/labels" "$OUT/loki/labels.json"
# Line patterns per stream (S5 items 3-4): one patterns API answer per stream
# in the volume fixture, over the pattern ingester's last 3 hours, as a JSON
# object keyed by stream selector. Loki doesn't log patterns calls as queries.
if python3 -I - "$LOKI" "$OUT/loki/volume_by_stream.json" > "$TMP" <<'PY'
import json, sys, time, urllib.parse, urllib.request
loki, vol = sys.argv[1], sys.argv[2]
end = int(time.time())
out = {}
streams = [r["metric"] for r in json.load(open(vol))["data"]["result"]]
names = sorted({k for m in streams for k in m})
for m in streams:
    sel = "{" + ", ".join('%s="%s"' % (k, v) for k, v in sorted(m.items())) + "}"
    # Exactly this stream: other streams' label names pinned to "" (as tvt
    # asks, loki.ExactStreamSelector); keyed by the plain selector.
    exact = "{" + ", ".join('%s="%s"' % (k, m.get(k, "")) for k in names) + "}"
    q = urllib.parse.urlencode({"query": exact, "start": end - 3 * 3600, "end": end})
    out[sel] = json.load(urllib.request.urlopen(loki + "/loki/api/v1/patterns?" + q, timeout=30))
json.dump(out, sys.stdout, indent=1, sort_keys=True)
PY
then keep "$OUT/loki/patterns.json"; else rm -f "$TMP"; echo "  FAIL $OUT/loki/patterns.json" >&2; fi
# Loki's query log: the metrics.go lines of the last two hours (frontend,
# querier and ruler; item 4 keeps the frontend's user queries).
compose logs --no-log-prefix --since 2h loki 2>&1 | grep 'caller=metrics.go' > "$TMP" || :
keep "$OUT/loki/query.log"
fi

if want grafana; then
echo "Grafana ($GRAFANA)"
get "$GRAFANA/api/search?type=dash-db" "$OUT/grafana/search.json" -u "$GRAFANA_AUTH"
get "$GRAFANA/api/datasources"         "$OUT/grafana/datasources.json" -u "$GRAFANA_AUTH"
# Grafana-managed alert and recording rules (hard keeps): the ruler config
# API, the one view with every query's datasource and the group interval.
get "$GRAFANA/api/ruler/grafana/api/v1/rules" "$OUT/grafana/ruler_config.json" -u "$GRAFANA_AUTH"
# Every dashboard search lists: the collector fetches each one, and a listed
# dashboard with no fixture fails the scan (fail closed).
for uid in $(python3 -I -c 'import json,sys; print(" ".join(d["uid"] for d in json.load(open(sys.argv[1]))))' "$OUT/grafana/search.json"); do
  get "$GRAFANA/api/dashboards/uid/$uid" "$OUT/grafana/dashboard_$uid.json" -u "$GRAFANA_AUTH"
done
compose logs --no-log-prefix grafana 2>/dev/null | grep 'msg="Request Completed"' | tail -n 500 > "$TMP" || :
keep "$OUT/grafana/router.log"
# The persistent file log (GF_PATHS_LOGS on the grafana-data volume) is what
# `tvt scan --grafana-log` reads. Check it for secrets before committing.
compose exec -T grafana cat /var/lib/grafana/logs/grafana.log > "$TMP" 2>/dev/null || :
keep "$OUT/grafana/grafana.log"
fi

# Mimir's cardinality API caps limit at 500, so tvt partitions metric names by
# prefix. label_values_name.json is the unpartitioned (truncated) answer;
# label_values_name_by_prefix.json is a JSON array of one answer per first
# character, which tests merge into the full metric list.
if want mimir; then
echo "Mimir ($MIMIR)"
if curl -fs -o /dev/null "$MIMIR/ready"; then
  M=$MIMIR/prometheus/api/v1
  get "$M/status/buildinfo" "$OUT/mimir/buildinfo.json"
  get "$M/rules"            "$OUT/mimir/rules.json"
  get "$M/cardinality/label_values" "$OUT/mimir/label_values_name.json" -G \
    --data-urlencode 'label_names[]=__name__' --data-urlencode 'limit=500'
  # Mimir caps limit at 500, so names are captured per prefix, and a prefix
  # with more than 500 names is split again (plus the exact name, which no
  # longer prefix matches), until every partition is complete.
  CHARS='a b c d e f g h i j k l m n o p q r s t u v w x y z A B C D E F G H I J K L M N O P Q R S T U V W X Y Z 0 1 2 3 4 5 6 7 8 9 _ :'
  sep=''
  emit() { printf '%s%s' "$sep" "$1"; sep=','; }
  names() { # selector
    curl -fsS -G "$M/cardinality/label_values" --data-urlencode 'label_names[]=__name__' \
      --data-urlencode "selector=$1" --data-urlencode 'limit=500'
  }
  part() { # prefix
    body=$(names "{__name__=~\"$1.*\"}") || return 1
    n=$(printf '%s' "$body" | sed -n 's/.*"label_values_count":\([0-9]*\).*/\1/p' | head -n 1)
    if [ "${n:-0}" -le 500 ]; then
      emit "$body"
      return 0
    fi
    exact=$(names "{__name__=\"$1\"}") || return 1
    emit "$exact"
    for c in $CHARS; do part "$1$c" || return 1; done
  }
  if {
    printf '['
    for c in $CHARS; do
      case $c in [0-9]) continue ;; esac   # metric names can't start with a digit
      part "$c" || exit 1
    done
    printf ']\n'
  } > "$OUT/mimir/label_values_name_by_prefix.json"; then
    echo "  ok  $OUT/mimir/label_values_name_by_prefix.json"
  else
    echo "  FAIL label_values by prefix" >&2
  fi
  sel='{__name__="shop_cart_operations_total"}'
  get "$M/cardinality/label_names" "$OUT/mimir/label_names_cart_ops.json" -G \
    --data-urlencode "selector=$sel" --data-urlencode 'limit=500'
  get "$M/cardinality/label_values" "$OUT/mimir/label_values_cart_ops.json" -G \
    --data-urlencode "selector=$sel" --data-urlencode 'limit=0' \
    --data-urlencode 'label_names[]=instance' --data-urlencode 'label_names[]=job' \
    --data-urlencode 'label_names[]=op' --data-urlencode 'label_names[]=user_id'
  # The query-frontend's query stats lines. The viewer doesn't query
  # Mimir, so send a panel query through Grafana's datasource proxy first
  # to have an attributed one (or run `tvt scan --mimir` for a probe line).
  compose logs --no-log-prefix --since 48h mimir 2>&1 | grep 'msg="query stats"' | tail -n 500 > "$TMP" || :
  keep "$OUT/mimir/query.log"
else
  echo "  skip: no Mimir at $MIMIR (tvt-lab: make up-mimir)"
fi
fi

# The golden test asserts against the lab's ground truth, so keep a copy
# with the fixtures it describes.
if cp "$LAB_DIR/ground-truth.yaml" "$OUT/ground-truth.yaml"; then
  echo "  ok  $OUT/ground-truth.yaml"
else
  echo "  FAIL ground-truth.yaml (is LAB_DIR=$LAB_DIR right?)" >&2
fi

# Declared dependencies the golden scan reads through tvt.yaml (Sprint 2
# item 7): the lab's SLO definitions, runbooks and postmortems, copied as they are.
for d in slos runbooks postmortems; do
  if [ -d "$LAB_DIR/$d" ] && rm -rf "${OUT:?}/$d" && cp -R "$LAB_DIR/$d" "$OUT/$d"; then
    echo "  ok  $OUT/$d/"
  else
    echo "  FAIL $d/ (is LAB_DIR=$LAB_DIR right?)" >&2
  fi
done

echo "Done. Review $OUT before committing."
