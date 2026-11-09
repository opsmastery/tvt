#!/bin/sh
# Capture real API responses from a running tvt-lab into testdata/lab/, so
# collectors can be tested offline against actual backend output.
#
#   ./scripts/capture-fixtures.sh            # defaults to the local lab
#   PROM=http://prom:9090 ./scripts/capture-fixtures.sh
#
# Only point this at the lab or at a partner environment with their consent:
# fixtures contain metric names, rule expressions and dashboard JSON.
set -eu

PROM=${PROM:-http://localhost:9090}
LOKI=${LOKI:-http://localhost:3100}
GRAFANA=${GRAFANA:-http://localhost:3000}
GRAFANA_AUTH=${GRAFANA_AUTH:-admin:admin}
LAB_DIR=${LAB_DIR:-../tvt-lab}
OUT=${OUT:-testdata/lab}

mkdir -p "$OUT/prometheus" "$OUT/loki" "$OUT/grafana"
get() { # url outfile [curl args...]
  url=$1; out=$2; shift 2
  if curl -fsS "$@" "$url" -o "$out"; then echo "  ok  $out"; else echo "  FAIL $url" >&2; fi
}

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

echo "Loki ($LOKI)"
get "$LOKI/loki/api/v1/rules" "$OUT/loki/rules.yaml"
get "$LOKI/loki/api/v1/index/volume" "$OUT/loki/volume_by_stream.json" -G \
  --data-urlencode 'query={app=~".+"}' --data-urlencode 'since=24h' --data-urlencode 'aggregateBy=series' \
  --data-urlencode 'targetLabels=app,component,level'
get "$LOKI/loki/api/v1/labels" "$OUT/loki/labels.json"

echo "Grafana ($GRAFANA)"
get "$GRAFANA/api/search?type=dash-db" "$OUT/grafana/search.json" -u "$GRAFANA_AUTH"
get "$GRAFANA/api/datasources"         "$OUT/grafana/datasources.json" -u "$GRAFANA_AUTH"
for uid in shop-overview legacy-queue; do
  get "$GRAFANA/api/dashboards/uid/$uid" "$OUT/grafana/dashboard_$uid.json" -u "$GRAFANA_AUTH"
done
if (cd "$LAB_DIR" && podman compose logs --no-log-prefix grafana 2>/dev/null) \
     | grep 'msg="Request Completed"' | tail -n 500 > "$OUT/grafana/router.log"; then
  echo "  ok  $OUT/grafana/router.log"
fi

echo "Done. Review $OUT before committing."
