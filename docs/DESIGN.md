# Design

## Reconcile loop

`internal/controller/doclingserver_controller.go` — one pass per event, no
state outside `status`:

```
Reconcile
 ├─ deletion?            → finalize: cleanup Job on existingClaim, drop finalizer
 ├─ add finalizer        (continue in the same pass; metadata updates don't bump generation)
 ├─ paused?              → return
 ├─ resolve(spec)        defaults + validation → Ready=False/InvalidSpec, no retry
 ├─ reconcileAuth        generated Secret (create-only data) or verify secretRef
 ├─ reconcileModels      PVC (create-only) · desired dir = f(version, baseURL, script, args, attempt)
 │     activeDir == desired → ModelsReady=True/Verified
 │     else Job <name>-models-download-<h>: running → Downloading, failed → DownloadFailed+Degraded,
 │          succeeded → activeDir=desired, previousDirs += old
 ├─ no activeDir yet?    → Available=False/WaitingForModels, return (no Deployment)
 ├─ reconcileDeployment  SSA; template annotation docling.artiz.dev/models-dir=activeDir
 ├─ reconcileService     SSA; status.endpoint from LB ingress
 ├─ reconcilePDB         SSA or delete
 ├─ reconcileScaling     SSA HPA | SSA KEDA ScaledObject (unstructured) | delete both
 ├─ inspectPods          ready / CrashLoopBackOff counts
 ├─ reconcileCleanup     rollout complete on activeDir → cleanup Job for previousDirs[:-keep]
 ├─ reconcileHealth      crash loop → attempts++ (bounded, 5 min cooldown) → desiredDir = base-r<N>
 ├─ summarize            Ready = ModelsReady ∧ Available ∧ ¬Degraded
 └─ patchStatus          only if status changed (semantic equality)
```

## Model directories

Every input that changes the bytes on disk is hashed:

```go
type modelSetIdentity struct {
    Version    string   // release tag → DOCLING_RS_MODELS_URL
    BaseURL    string
    ScriptURL  string
    ScriptArgs []string // --no-asr, --enrich, --asr-model=…, sorted
}
dir = "<version>-<sha256(identity)[:10]>" + ("-r<attempt>" | "")
```

Layout on the claim:

```
/models/
  models-v1-3f9c2a1b7e/
    .docling-ready          # written last; contains the dir name
    .models/                # what download_dependencies.sh produces
      layout_heron.onnx
      tableformer/…
  models-v2-91ab0c7d3e/     # downloading; old one above still serves
```

The serve container mounts `subPath: <dir>/.models` read-only at
`/app/.models` (`C:\models` on Windows) and gets `DOCLING_RS_MODELS_DIR`.
Because the directory name is in the pod template, a switch *is* a rollout:
`maxUnavailable: 0`, startup probe on `/ready` covers `--warmup`.

The download Job is a thin wrapper around upstream's script so the operator
never has to know the asset list:

```sh
cd $MODELS_ROOT/$MODELS_DIR
[ "$(cat .docling-ready)" = "$MODELS_DIR" ] && exit 0
curl -fsSL $SCRIPT_URL -o /tmp/download_dependencies.sh
sh /tmp/download_dependencies.sh $SCRIPT_ARGS     # DOCLING_RS_MODELS_VERIFY=require
test -f .models/layout_heron.onnx
echo "$MODELS_DIR" > .docling-ready
```

Idempotent: a re-run after a partial download only fetches what is missing;
the Job's `Complete` condition is the source of truth, recorded into
`status.models.activeDir`. If the operator loses status (CR recreated on the
same claim), it re-runs the Job, which exits immediately on the marker.

## Why the Deployment is applied with server-side apply

`controllerutil.CreateOrUpdate` with a full template overwrite diffs against
the API-server-defaulted object and issues an update every pass. With SSA the
operator owns exactly the fields it sets:

* `spec.replicas` is omitted in `hpa`/`keda` mode — the autoscaler owns it, and
  a resize by the HPA does not fight the operator;
* cloud-provider `nodePort`s, `clusterIP`, defaulted probe fields stay untouched;
* an unchanged apply is a no-op on the API server (resourceVersion stable —
  asserted by the idempotency test).

`Secret`, `PVC` and `Job` are create-only by nature (random key, immutable
spec, immutable template) and use plain `Create`/`CreateOrUpdate`.

## Self-healing

A serve pod with `CrashLoopBackOff` (or ≥5 restarts and not ready) while the
Deployment already runs the active directory means the on-disk models are
suspect. Recovery reuses the normal switch path: bump
`status.models.recoveryAttempts`, which changes the desired directory to
`<base>-r<N>`; the next pass starts a download Job, the rollout moves pods,
cleanup deletes the bad directory. Bounds: `recovery.maxAttempts` per model
set, a 5-minute cooldown between attempts (requeued, not busy-looped), and a
`Degraded=True/RecoveryExhausted` end state that a spec change resets.

A crash loop that is *not* the models (bad image, OOM) therefore costs at most
`maxAttempts` downloads before the operator stops and reports. That is the
trade-off chosen over trying to diagnose container exit codes.

## Watches

* `For(DoclingServer)` with generation/annotation predicates — status patches
  do not re-trigger.
* `Owns(Deployment, Service, Secret, PVC, Job, HPA, PDB[, ScaledObject])`.
* `Watches(Pod)` mapped by the `docling.artiz.dev/server` label, filtered to
  readiness/crash-loop transitions, so a crash loop wakes the controller
  without polling.

KEDA presence is detected once at startup via the REST mapper; `Owns` is only
registered when the CRD is served.

## RBAC

Generated from markers into `config/rbac/role.yaml`: full control of the
owned kinds, `get/list/watch` on pods, `create/patch` on events, and
`keda.sh/scaledobjects`.

## Non-goals (for now)

* Ingress/Gateway objects — `service.type: ClusterIP` plus your own Ingress is
  one line.
* Scale-to-zero — the API is synchronous; `minReplicas: 1` keeps the
  warmed-up pipeline resident.
* Multi-tenant model sharing between servers beyond a shared claim.
