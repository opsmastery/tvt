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

if want prometheus; then
echo "Prometheus ($PROM)"
get "$PROM/api/v1/status/tsdb?limit=100000" "$OUT/prometheus/tsdb.json"
get "$PROM/api/v1/rules"                  "$OUT/prometheus/rules.json"
get "$PROM/api/v1/label/__name__/values"  "$OUT/prometheus/metric_names.json"
get "$PROM/api/v1/status/buildinfo"       "$OUT/prometheus/buildinfo.json"
get "$PROM/api/v1/series?match[]=shop_cart_operations_total&limit=200" "$OUT/prometheus/series_cart_ops.json"
if (cd "$LAB_DIR" && podman compose exec -T prometheus tail -n 500 /prometheus/query.log) \
     > "$OUT/prometheus/query.log" 2>/dev/null; then
  echo "  ok  $OUT/prometheus/query.log"
else
  echo "  FAIL query.log (is LAB_DIR=$LAB_DIR right?)" >&2
fi
fi

if want loki; then
echo "Loki ($LOKI)"
get "$LOKI/loki/api/v1/rules" "$OUT/loki/rules.yaml"
get "$LOKI/prometheus/api/v1/rules" "$OUT/loki/prom_rules.json"  # effective group intervals
get "$LOKI/loki/api/v1/index/volume" "$OUT/loki/volume_by_stream.json" -G \
  --data-urlencode 'query={app=~".+"}' --data-urlencode 'since=24h' --data-urlencode 'aggregateBy=series' \
  --data-urlencode 'targetLabels=app,component,level'
get "$LOKI/loki/api/v1/labels" "$OUT/loki/labels.json"
# Loki's query log: the metrics.go lines of the last two hours (frontend,
# querier and ruler; item 4 keeps the frontend's user queries).
if (cd "$LAB_DIR" && podman compose logs --no-log-prefix --since 2h loki 2>&1) \
     | grep 'caller=metrics.go' > "$OUT/loki/query.log"; then
  echo "  ok  $OUT/loki/query.log"
else
  echo "  FAIL loki query.log (is LAB_DIR=$LAB_DIR right?)" >&2
fi
fi

if want grafana; then
echo "Grafana ($GRAFANA)"
get "$GRAFANA/api/search?type=dash-db" "$OUT/grafana/search.json" -u "$GRAFANA_AUTH"
get "$GRAFANA/api/datasources"         "$OUT/grafana/datasources.json" -u "$GRAFANA_AUTH"
# Grafana-managed alert and recording rules (hard keeps): the ruler config
# API, the one view with every query's datasource and the group interval.
get "$GRAFANA/api/ruler/grafana/api/v1/rules" "$OUT/grafana/ruler_config.json" -u "$GRAFANA_AUTH"
for uid in shop-overview legacy-queue; do
  get "$GRAFANA/api/dashboards/uid/$uid" "$OUT/grafana/dashboard_$uid.json" -u "$GRAFANA_AUTH"
done
if (cd "$LAB_DIR" && podman compose logs --no-log-prefix grafana 2>/dev/null) \
     | grep 'msg="Request Completed"' | tail -n 500 > "$OUT/grafana/router.log"; then
  echo "  ok  $OUT/grafana/router.log"
fi
# The persistent file log (GF_PATHS_LOGS on the grafana-data volume) is what
# `tvt scan --grafana-log` reads. Check it for secrets before committing.
if (cd "$LAB_DIR" && podman compose exec -T grafana cat /var/lib/grafana/logs/grafana.log 2>/dev/null) \
     > "$OUT/grafana/grafana.log"; then
  echo "  ok  $OUT/grafana/grafana.log"
else
  echo "  FAIL grafana.log (is LAB_DIR=$LAB_DIR right?)" >&2
fi
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
  if {
    printf '['; sep=''
    for c in a b c d e f g h i j k l m n o p q r s t u v w x y z \
             A B C D E F G H I J K L M N O P Q R S T U V W X Y Z _ :; do
      printf '%s' "$sep"; sep=','
      curl -fsS -G "$M/cardinality/label_values" --data-urlencode 'label_names[]=__name__' \
        --data-urlencode "selector={__name__=~\"$c.*\"}" --data-urlencode 'limit=500' || exit 1
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
  if (cd "$LAB_DIR" && podman compose logs --no-log-prefix --since 48h mimir 2>&1) \
       | grep 'msg="query stats"' | tail -n 500 > "$OUT/mimir/query.log"; then
    echo "  ok  $OUT/mimir/query.log"
  else
    echo "  FAIL mimir query.log (is LAB_DIR=$LAB_DIR right?)" >&2
  fi
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

echo "Done. Review $OUT before committing."
