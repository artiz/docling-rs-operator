#!/usr/bin/env bash
# End-to-end autoscaling check for a DoclingServer.
#
#   hack/scale-test.sh <doclingserver> <file.pdf> [total=800] [parallel=32] [maxReplicas=4]
#
# 1. switches the server to HPA mode on CPU (50 % of a 500m request, so one
#    replica saturates fast) and waits for Ready
# 2. ships the PDF into the cluster as a ConfigMap (<= 1 MiB) and runs a Job
#    that hammers the Service from inside the cluster with `parallel` workers
# 3. prints HPA / DoclingServer / pod CPU every 15 s, passes once
#    readyReplicas >= 2, then reports how long scale-down takes
#
# Needs metrics-server (`kubectl top pods` must work).
set -euo pipefail

NAME=${1:?usage: $0 <doclingserver> <file.pdf> [total] [parallel] [maxReplicas]}
FILE=${2:?usage: $0 <doclingserver> <file.pdf> [total] [parallel] [maxReplicas]}
TOTAL=${3:-800}
PAR=${4:-32}
MAX=${5:-4}
NS=${NAMESPACE:-$(kubectl config view --minify -o jsonpath='{..namespace}')}
NS=${NS:-default}
JOB="$NAME-load"
CM="$NAME-load-sample"

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

say "0. preflight"
kubectl top pods -n "$NS" >/dev/null 2>&1 || {
  echo "metrics-server is not serving pod metrics; HPA on CPU cannot work." >&2
  echo "  minikube: minikube addons enable metrics-server" >&2
  echo "  kind:     kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml" >&2
  echo "            then: kubectl -n kube-system patch deploy metrics-server --type=json -p '[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--kubelet-insecure-tls\"}]'" >&2
  exit 1
}
SIZE=$(wc -c <"$FILE" | tr -d ' ')
[ "$SIZE" -le 1000000 ] || { echo "$FILE is $SIZE bytes; ConfigMaps hold <= 1 MiB, use a smaller PDF" >&2; exit 1; }
kubectl get dsrv "$NAME" -n "$NS" >/dev/null
AFF=$(kubectl get svc "$NAME" -n "$NS" -o jsonpath='{.spec.sessionAffinity}' 2>/dev/null || true)
if [ "$AFF" = "ClientIP" ]; then
  echo "Service $NAME has sessionAffinity=ClientIP: the load Job is ONE client address and will be" >&2
  echo "pinned to ONE replica, so no scale test is meaningful. Set spec.service.sessionAffinity: None" >&2
  echo "(the operator default since the sessionAffinity change; restart 'make run' after git pull)." >&2
  exit 1
fi

say "1. HPA mode: 1..$MAX replicas, CPU target 50% of 500m"
kubectl patch dsrv "$NAME" -n "$NS" --type merge -p "$(cat <<EOF
{"spec":{
  "scaling":{"mode":"hpa","minReplicas":1,"maxReplicas":$MAX,"targetCPUUtilization":50,
             "behavior":{"scaleUp":{"stabilizationWindowSeconds":0},
                         "scaleDown":{"stabilizationWindowSeconds":60}}},
  "server":{"resources":{"requests":{"cpu":"500m","memory":"1Gi"},
                         "limits":{"cpu":"1","memory":"2Gi"}}}
}}
EOF
)"
echo "waiting for the rollout (new resources) and Ready..."
sleep 5
kubectl rollout status deploy/"$NAME" -n "$NS" --timeout=10m
kubectl wait dsrv "$NAME" -n "$NS" --for=condition=Ready --timeout=10m
kubectl get hpa "$NAME" -n "$NS"
BASE=$(kubectl get deploy "$NAME" -n "$NS" -o jsonpath='{.spec.replicas}')

say "2. load Job: $TOTAL requests, $PAR workers, from inside the cluster"
kubectl delete job "$JOB" -n "$NS" --ignore-not-found --wait=true >/dev/null
kubectl create configmap "$CM" -n "$NS" --from-file="sample.pdf=$FILE" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
SECRET=$(kubectl get dsrv "$NAME" -n "$NS" -o jsonpath='{.status.apiKeySecret.name}')
PORT=$(kubectl get svc "$NAME" -n "$NS" -o jsonpath='{.spec.ports[0].port}')
PER=$(( (TOTAL + PAR - 1) / PAR ))

kubectl apply -n "$NS" -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: $JOB
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: load
        image: docker.io/curlimages/curl:8.11.1
        env:
        - {name: URL, value: "http://$NAME.$NS.svc:$PORT/v1/convert"}
        - {name: PAR, value: "$PAR"}
        - {name: PER, value: "$PER"}
        - name: DOCLING_API_KEY
          valueFrom: {secretKeyRef: {name: "${SECRET:-none}", key: api-key, optional: true}}
        command: ["sh", "-c"]
        args:
        - |
          set -u
          OUT=/tmp/results
          : > \$OUT
          HDR=""; [ -n "\${DOCLING_API_KEY:-}" ] && HDR="-H X-Api-Key:\$DOCLING_API_KEY"
          echo "POST \$URL  workers=\$PAR  per-worker=\$PER"
          START=\$(date +%s)
          w=0
          while [ \$w -lt \$PAR ]; do
            ( i=0; while [ \$i -lt \$PER ]; do
                curl -s -o /dev/null --max-time 600 \$HDR -F file=@/data/sample.pdf \
                     -w '%{http_code} %{time_total}\n' "\$URL" >> \$OUT
                i=\$((i+1)); done ) &
            w=\$((w+1))
          done
          wait
          END=\$(date +%s)
          echo "status codes:"; awk '{c[\$1]++} END {for (k in c) printf "  %-4s %d\n", k, c[k]}' \$OUT | sort
          echo "completed \$(wc -l < \$OUT) requests in \$((END-START))s"
        volumeMounts:
        - {name: sample, mountPath: /data, readOnly: true}
      volumes:
      - name: sample
        configMap: {name: $CM}
EOF

say "3. watching (Ctrl-C stops the watch, the Job keeps running)"
PASS=""
T0=$(date +%s)
while :; do
  NOW=$(( $(date +%s) - T0 ))
  READY=$(kubectl get dsrv "$NAME" -n "$NS" -o jsonpath='{.status.readyReplicas}')
  DESIRED=$(kubectl get hpa "$NAME" -n "$NS" -o jsonpath='{.status.desiredReplicas}' 2>/dev/null || true)
  HPA=$(kubectl get hpa "$NAME" -n "$NS" -o jsonpath='{.status.currentMetrics[0].resource.current.averageUtilization}%/{.spec.metrics[0].resource.target.averageUtilization}% desired={.status.desiredReplicas}' 2>/dev/null || true)
  JOBST=$(kubectl get job "$JOB" -n "$NS" -o jsonpath='{.status.succeeded}{.status.failed}' 2>/dev/null)
  echo "t+${NOW}s  readyReplicas=${READY:-0}  hpa: ${HPA}  job: ${JOBST:-running}"
  kubectl top pods -n "$NS" -l "docling.artiz.dev/server=$NAME" --no-headers 2>/dev/null | sed 's/^/    /'
  # Pass only on an HPA decision, not on a rollout surge.
  if [ -z "$PASS" ] && [ "${DESIRED:-0}" -gt "${BASE:-1}" ] && [ "${READY:-0}" -ge "${DESIRED:-0}" ]; then
    PASS=1; echo "✅ HPA scaled ${BASE:-1} -> ${READY} replicas after ${NOW}s"
  fi
  if [ -n "$JOBST" ]; then
    echo; echo "load Job finished:"; kubectl logs "job/$JOB" -n "$NS" | tail -8
    break
  fi
  if [ -z "$PASS" ] && [ "$NOW" -ge 900 ]; then
    echo "⏱ 15 min without scale-up; check 'kubectl describe hpa $NAME'"; break
  fi
  if [ -n "$PASS" ] && [ "$NOW" -ge 2400 ]; then
    echo "⏱ load Job still running after 40 min; stopping the watch (kubectl logs job/$JOB for results)"; break
  fi
  sleep 15
done

if [ -n "$PASS" ]; then
  say "4. waiting for the load to end, then scale-down to 1 (stabilization 60s + HPA sync)"
  kubectl wait job "$JOB" -n "$NS" --for=condition=Complete --timeout=40m >/dev/null 2>&1 || true
  T1=$(date +%s)
  while :; do
    READY=$(kubectl get dsrv "$NAME" -n "$NS" -o jsonpath='{.status.readyReplicas}')
    NOW=$(( $(date +%s) - T1 ))
    [ "${READY:-0}" -le 1 ] && { echo "✅ back to ${READY} replica after ${NOW}s"; break; }
    [ "$NOW" -lt 600 ] || { echo "still ${READY} replicas after 10 min"; break; }
    sleep 15
  done
else
  echo "❌ no scale-up observed"
  kubectl describe hpa "$NAME" -n "$NS" | sed -n '/Conditions/,$p'
fi

say "cleanup"
kubectl delete job "$JOB" configmap "$CM" -n "$NS" --ignore-not-found >/dev/null
echo "server $NAME stays in HPA mode (1..$MAX); revert with: kubectl patch dsrv $NAME --type merge -p '{\"spec\":{\"scaling\":{\"mode\":\"none\"}}}'"
[ -n "$PASS" ]
