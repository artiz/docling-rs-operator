#!/usr/bin/env bash
# Fire N conversions at a docling-serve endpoint with P parallel workers and
# summarise status codes, latency percentiles and throughput.
#
#   hack/load.sh <file.pdf> [total=2000] [parallel=100] [url=http://localhost:8080]
#
# Environment:
#   DOCLING_API_KEY   sent as X-Api-Key when set
#   TIMEOUT           per-request timeout in seconds (default 600)
#
# Over `kubectl port-forward` keep parallel <= 50: the forwarder is a single
# connection to ONE pod and drops under heavy concurrency. For thousands of
# concurrent requests run hack/scale-test.sh, which loads the Service from
# inside the cluster.
set -euo pipefail

FILE=${1:?usage: $0 <file> [total] [parallel] [url]}
TOTAL=${2:-2000}
PAR=${3:-100}
URL=${4:-http://localhost:8080}
TIMEOUT=${TIMEOUT:-600}

[ -r "$FILE" ] || { echo "cannot read $FILE" >&2; exit 1; }
OUT=$(mktemp -t docling-load.XXXXXX)
trap 'rm -f "$OUT"' EXIT

HDR=()
[ -n "${DOCLING_API_KEY:-}" ] && HDR=(-H "X-Api-Key: $DOCLING_API_KEY")

echo "POST $URL/v1/convert  file=$FILE ($(wc -c <"$FILE") bytes)  total=$TOTAL  parallel=$PAR"
START=$(date +%s)
seq "$TOTAL" | xargs -P "$PAR" -I{} \
  curl -s -o /dev/null --max-time "$TIMEOUT" ${HDR[@]+"${HDR[@]}"} \
       -F "file=@$FILE" -w '%{http_code} %{time_total}\n' "$URL/v1/convert" \
  >>"$OUT" || true
END=$(date +%s)
ELAPSED=$(( END - START ))
[ "$ELAPSED" -gt 0 ] || ELAPSED=1

echo
echo "status codes:"
awk '{c[$1]++} END {for (k in c) printf "  %-4s %d\n", k, c[k]}' "$OUT" | sort
echo
echo "latency (s) of 2xx responses:"
awk '$1 ~ /^2/ {print $2}' "$OUT" | sort -n | awk '
  {a[NR]=$1}
  END {
    if (NR == 0) { print "  none"; exit }
    printf "  n=%d  min=%.2f  p50=%.2f  p90=%.2f  p99=%.2f  max=%.2f\n",
      NR, a[1], a[int(NR*0.50)+ (NR*0.50==int(NR*0.50)?0:1)], a[int(NR*0.90)+(NR*0.90==int(NR*0.90)?0:1)],
      a[int(NR*0.99)+(NR*0.99==int(NR*0.99)?0:1)], a[NR]
  }'
DONE=$(wc -l <"$OUT" | tr -d ' ')
echo
printf "completed %s requests in %ds  (%.1f req/s)\n" "$DONE" "$ELAPSED" "$(echo "$DONE / $ELAPSED" | bc -l)"
