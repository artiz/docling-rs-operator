# docling-rs-operator

A Kubernetes operator for the [docling.rs](https://github.com/docling-project/docling.rs)
HTTP conversion API (`docling-rs serve`). You create one `DoclingServer`
resource; the operator downloads and verifies the ONNX models onto a shared
volume, runs the server behind a LoadBalancer with API-key auth, scales it by
load, and heals it when the models or pods break. After that you only talk
HTTP: `POST /v1/convert` with a document, get Markdown or JSON back.

```
                ┌──────────────────────── DoclingServer (CR) ────────────────────────┐
                │ models: release models-v1, options…   accelerator: cpu|cuda|directml│
                │ scaling: hpa|keda 1..N                auth: Secret                  │
                └──────────────────────────────┬─────────────────────────────────────┘
                                               │ reconcile
   ┌────────────┐   Job: download + SHA256 ┌────▼─────┐   SSA   ┌────────────────────┐
   │ RWX PVC    │◄─────────────────────────│ operator │────────►│ Deployment         │
   │ <ver>-<h>/ │   Job: cleanup old dirs  │          │         │  docling-serve      │
   │ <ver>-<h2>/│◄─────────────────────────│          │         │  /app/.models (ro)  │
   └─────▲──────┘                          └────┬─────┘         └─────────▲──────────┘
         │ subPath, read-only                   │  HPA / KEDA ScaledObject │  /metrics
         └──────────────────────────────────────┼──────────────────────────┘
                                                ▼
                                   Service type=LoadBalancer :80  ──► clients (X-Api-Key)
```

## Why an operator and not a Helm chart

A chart renders a Deployment, an HPA and a PVC once. The things that go wrong
with an inference server are stateful and happen *after* install:

| Concern | What the operator does |
|---|---|
| **Models as a lifecycle** | `spec.models.version` names a GitHub release (`models-v1`, …). A Job downloads it into `<pvc>/<version>-<hash>/` with the upstream `download_dependencies.sh` and `DOCLING_RS_MODELS_VERIFY=require` (SHA256SUMS). The Deployment is not created until `ModelsReady=True`. |
| **Safe model switch** | A changed version/option set hashes to a new directory. The old set keeps serving while the new one downloads; only then the Deployment rolls (maxUnavailable 0) onto the new `subPath`; the old directory is deleted by a cleanup Job once the rollout is complete (`keepPreviousVersions` for fast rollback). |
| **Scaling by load** | The operator does not scrape metrics itself. It maintains an HPA (CPU and/or `docling_serve_requests_in_flight` via prometheus-adapter) or a KEDA `ScaledObject` with a Prometheus trigger on the same metric, and never lets both exist. `spec.replicas` of the Deployment is owned by the autoscaler. |
| **Self-healing** | Serve pods in `CrashLoopBackOff` (corrupt volume, half-written model) trigger a re-download into a fresh directory and a rollout, bounded by `spec.models.recovery.maxAttempts` and a 5-minute cooldown. Everything is reported as `Degraded` with a reason. |
| **Hygiene** | Owner references on every object, a finalizer that cleans a user-provided PVC, `metav1.Condition` status (`ModelsReady`, `Available`, `ScalingReady`, `Degraded`, `Ready`), server-side apply so no-op reconciles produce no writes, minimal RBAC, envtest suite. |

## Quick start

```bash
kubectl apply -f https://github.com/artiz/docling-rs-operator/releases/latest/download/install.yaml
```

or from a checkout:

```bash
make install          # CRD
make deploy IMG=ghcr.io/artiz/docling-rs-operator:latest
```

Create a server (any `ReadWriteMany` storage class works):

```yaml
apiVersion: docling.artiz.dev/v1alpha1
kind: DoclingServer
metadata:
  name: docling
spec:
  models:
    storage:
      storageClassName: nfs-csi
  server:
    concurrency: 2
    resources:
      requests: {cpu: "1", memory: 2Gi}
      limits: {cpu: "4", memory: 4Gi}
  scaling:
    minReplicas: 1
    maxReplicas: 5
```

Watch it come up and use it:

```bash
kubectl get dsrv docling -w
# NAME      READY   MODELS     REPLICAS   ENDPOINT
# docling   False   Downloading
# docling   True    Verified   1          http://203.0.113.10:80

KEY=$(kubectl get secret docling-api-key -o jsonpath='{.data.api-key}' | base64 -d)
URL=$(kubectl get dsrv docling -o jsonpath='{.status.endpoint}')
curl -H "X-Api-Key: $KEY" -F file=@paper.pdf "$URL/v1/convert"          # Markdown
curl -H "X-Api-Key: $KEY" -F file=@paper.pdf "$URL/v1/convert?to=json"  # docling JSON
```

More samples in [`config/samples`](config/samples): CUDA, DirectML, KEDA.

## The `DoclingServer` API

```yaml
spec:
  image:                 # repository/tag/pullPolicy/pullSecrets; defaults to the upstream
    tag: "1"             #   docling-rs-serve (cpu) / docling-rs-serve-cuda (cuda) image
  accelerator:
    type: cpu | cuda | directml
    count: 1             # GPUs per replica
    runtimeClassName: nvidia
    executionProvider:   # DOCLING_RS_EP override (cpu, cuda, auto, …)
  models:
    source: release | image     # image = models bundled in the container, no PVC
    version: models-v1          # GitHub release tag hosting the models + SHA256SUMS
    scriptURL: …download_dependencies.sh   # pin to a commit for reproducibility
    options: {asr: true, int8: true, chunk: true, enrich: false, ner: false, embed: false,
              fonts: false, asrPresets: [parakeet_tdt_0.6b_v3]}
    storage: {existingClaim: "", storageClassName: nfs-csi, size: 20Gi, accessModes: [ReadWriteMany]}
    downloader: {image: curlimages/curl:8.11.1, env: [{name: HTTPS_PROXY, value: …}], …}
    recovery: {enabled: true, maxAttempts: 3}
    keepPreviousVersions: 0
  server:                # maps 1:1 to docling-serve flags
    port: 5001
    concurrency: 2
    maxBodyMB: 256
    queueSize: 16
    resultTTLSeconds: 600
    warmup: true
    idleUnloadSeconds: 0
    maxMemoryMB: null
    allowURLFetch: false
    enableUI: true
    logLevel: info
    extraArgs: []
    env: []
    resources: {}
  auth:
    enabled: true
    secretRef: {name: my-key, key: api-key}   # omit → generated <name>-api-key
  service:
    type: LoadBalancer
    port: 80
    annotations: {}
    loadBalancerSourceRanges: []
    sessionAffinity: None                     # ClientIP if clients use /v1/convert/async
  scaling:
    mode: hpa | keda | none
    minReplicas: 1
    maxReplicas: 5
    targetCPUUtilization: 70
    targetInFlightPerReplica: 0               # e.g. = server.concurrency
    keda: {prometheusAddress: http://prometheus:9090, query: "", pollingInterval: 15, cooldownPeriod: 120}
    behavior: {}                              # autoscaling/v2 HPA behavior
    minAvailable: 1                           # creates a PodDisruptionBudget
  pod:                   # nodeSelector, tolerations, affinity, topologySpreadConstraints,
                         # securityContext, extraVolumes/extraVolumeMounts (tessdata), …
  paused: false
```

### Status

```yaml
status:
  conditions:
  - {type: ModelsReady,  status: "True",  reason: Verified}      # or Downloading / DownloadFailed / BundledInImage
  - {type: Available,    status: "True",  reason: DeploymentAvailable}
  - {type: ScalingReady, status: "True",  reason: HPAConfigured} # or KEDAConfigured / KEDANotInstalled / ScalingDisabled
  - {type: Degraded,     status: "False", reason: Healthy}       # or Recovering / PodsCrashLooping / RecoveryExhausted / DownloadFailed
  - {type: Ready,        status: "True",  reason: Healthy}
  models:
    activeDir: models-v1-3f9c2a1b7e     # directory on the PVC served right now
    desiredDir: models-v1-3f9c2a1b7e
    previousDirs: []                    # superseded, pending cleanup
    recoveryAttempts: 0
    claimName: docling-models
  endpoint: http://203.0.113.10:80
  apiKeySecret: {name: docling-api-key, key: api-key}
  scaler: hpa/docling
  readyReplicas: 2
```

## Accelerators

| `accelerator.type` | Image | Scheduling | Runtime |
|---|---|---|---|
| `cpu` (default) | `ghcr.io/docling-project/docling-rs-serve` | – | `DOCLING_RS_EP=cpu` |
| `cuda` | `ghcr.io/docling-project/docling-rs-serve-cuda` (CUDA 13, cuDNN 9, driver ≥ 580) | `nvidia.com/gpu: <count>` limit, `nvidia.com/gpu` toleration, optional `runtimeClassName` | `DOCLING_RS_EP=cuda` |
| `directml` | **your own** Windows image built with `--features directml` (`spec.image.repository` is required; upstream ships none) | `kubernetes.io/os=windows` node selector, `microsoft.com/directx: <count>` (Windows GPU device plugin), models mounted at `C:\models` | `DOCLING_RS_EP=auto` |

The model download Job always runs on Linux; for DirectML point
`spec.models.downloader.nodeSelector` at a Linux pool and use an SMB-backed
RWX class (e.g. Azure Files) that Windows nodes can mount.

## Scaling signal

`docling-serve` exposes `docling_serve_requests_in_flight` (gauge),
`docling_serve_requests_total{status_class}` and
`docling_serve_request_duration_seconds` on `/metrics`; the pods carry the
usual `prometheus.io/scrape` annotations.

* **hpa** with `targetCPUUtilization` works out of the box.
* **hpa** with `targetInFlightPerReplica` needs
  [prometheus-adapter](https://github.com/kubernetes-sigs/prometheus-adapter)
  exposing the gauge as a *pods* metric; a rule is in
  [`config/prometheus/prometheus-adapter-rule.yaml`](config/prometheus/prometheus-adapter-rule.yaml).
* **keda** needs KEDA and a Prometheus the ScaledObject can query. Without the
  KEDA CRD the operator reports `ScalingReady=False/KEDANotInstalled` and
  creates nothing.

A sensible target is `targetInFlightPerReplica = server.concurrency`: once
every replica is converting at its concurrency limit, new requests queue and
another replica is added. Scale-down has a 5-minute stabilization window by
default (override with `scaling.behavior`).

## Operating notes

* **Retrying a failed download** – the Job is kept for `kubectl logs`; delete
  it and the operator re-creates it (the upstream script is idempotent and
  only fetches missing files).
* **Rolling back a model version** – set `keepPreviousVersions: 1`; switching
  `version` back then needs no download (the directory is still there).
* **Resetting recovery** – `recoveryAttempts` is per model set; any change to
  `spec.models` (even `scriptURL`) starts a fresh budget.
* **Shared PVC across servers** – use `storage.existingClaim`; directories are
  content-addressed so two servers with the same model set share nothing but
  the claim (each keeps its own directory). On deletion a cleanup Job removes
  only this server's directories.
* **PodSecurity `restricted`** – the downloader runs as root by default so the
  PVC root is writable on any backend; set
  `models.downloader.podSecurityContext` (`runAsUser`, `fsGroup`) otherwise.
* **Async jobs** – `/v1/convert/async` results live in the memory of the
  replica that accepted the job. With more than one replica set
  `service.sessionAffinity: ClientIP`, or stick to the synchronous
  `/v1/convert`. The default is `None` so a single client address is spread
  across replicas.
* **Pause** – `spec.paused: true` stops reconciliation without deleting anything.

## Development

```bash
make test            # envtest (downloads kube-apiserver 1.33 into ./bin)
make run             # run against the current kubeconfig
make docker-build IMG=…
make build-installer # dist/install.yaml
```

Load and autoscaling checks against a real cluster:

```bash
hack/load.sh paper.pdf 2000 50 http://localhost:8080     # N requests, P workers, status/latency summary
hack/scale-test.sh docling paper.pdf 800 32              # HPA mode, in-cluster load Job, waits for scale-up/down
```

The envtest suite covers: models not ready → ready → version switch with
cleanup, download failure, bundled models, CUDA and DirectML pod specs,
crash-loop recovery with exhausted attempts, KEDA without the CRD, deletion
with an existing claim, and no-op reconciles leaving autoscaler-owned
replicas and resourceVersions untouched.

See [`docs/DESIGN.md`](docs/DESIGN.md) for the reconcile loop in detail.

## License

MIT
