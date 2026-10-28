/*
Copyright (c) 2026 Artem Kustikov

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	doclingv1alpha1 "github.com/artiz/docling-rs-operator/api/v1alpha1"
)

// recoveryCooldown is the minimum time between two crash-loop triggered
// re-downloads, so a bad image or a bad node does not burn the attempt
// budget within seconds.
const recoveryCooldown = 5 * time.Minute

// DoclingServerReconciler reconciles a DoclingServer object.
type DoclingServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// KEDAAvailable is set at startup when the keda.sh/v1alpha1 ScaledObject
	// kind is served by the cluster.
	KEDAAvailable bool
}

// +kubebuilder:rbac:groups=docling.artiz.dev,resources=doclingservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=docling.artiz.dev,resources=doclingservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=docling.artiz.dev,resources=doclingservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;secrets;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives one DoclingServer towards its desired state:
//
//  1. API-key Secret (generated unless spec.auth.secretRef is set)
//  2. shared RWX PVC + download Job for the desired model set (checksum
//     verified by the upstream script, DOCLING_RS_MODELS_VERIFY=require)
//  3. Deployment, which is only created/updated once the desired model
//     directory is ready; a model version switch is download-then-rollout
//  4. Service (LoadBalancer by default), PDB, HPA or KEDA ScaledObject
//  5. cleanup Job for superseded model directories once the rollout is done
//  6. crash-loop detection with bounded re-download (self-healing)
func (r *DoclingServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ds := &doclingv1alpha1.DoclingServer{}
	if err := r.Get(ctx, req.NamespacedName, ds); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ds.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, ds)
	}
	if !controllerutil.ContainsFinalizer(ds, finalizerName) {
		// Metadata-only updates do not bump the generation and would be
		// filtered by the For() predicate, so keep going in this pass.
		controllerutil.AddFinalizer(ds, finalizerName)
		if err := r.Update(ctx, ds); err != nil {
			return ctrl.Result{}, err
		}
	}
	if ds.Spec.Paused {
		log.V(1).Info("reconciliation paused")
		return ctrl.Result{}, nil
	}

	orig := ds.DeepCopy()
	ds.Status.ObservedGeneration = ds.Generation

	cfg, err := resolve(ds)
	if err != nil {
		r.setCondition(ds, doclingv1alpha1.ConditionReady, metav1.ConditionFalse, doclingv1alpha1.ReasonInvalidSpec, err.Error())
		r.Recorder.Event(ds, corev1.EventTypeWarning, doclingv1alpha1.ReasonInvalidSpec, err.Error())
		return ctrl.Result{}, r.patchStatus(ctx, ds, orig)
	}

	result, reconcileErr := r.reconcile(ctx, ds, cfg)
	r.summarize(ds)
	if err := r.patchStatus(ctx, ds, orig); err != nil {
		if reconcileErr == nil {
			reconcileErr = err
		} else {
			log.Error(err, "failed to update status")
		}
	}
	return result, reconcileErr
}

func (r *DoclingServerReconciler) reconcile(ctx context.Context, ds *doclingv1alpha1.DoclingServer, cfg *resolved) (ctrl.Result, error) {
	var result ctrl.Result

	if err := r.reconcileAuth(ctx, ds, cfg); err != nil {
		return result, err
	}

	modelsDir, modelsReady, err := r.reconcileModels(ctx, ds, cfg)
	if err != nil {
		return result, err
	}

	// Without any usable model directory there is nothing to run yet.
	if cfg.ModelsSource == doclingv1alpha1.ModelsSourceRelease && modelsDir == "" {
		r.setCondition(ds, doclingv1alpha1.ConditionAvailable, metav1.ConditionFalse, doclingv1alpha1.ReasonWaitingForModels, "Deployment is created once the first model set is downloaded")
		ds.Status.Replicas, ds.Status.ReadyReplicas = 0, 0
		return result, nil
	}

	dep, err := r.reconcileDeployment(ctx, ds, cfg, modelsDir)
	if err != nil {
		return result, err
	}
	if err := r.reconcileService(ctx, ds, cfg); err != nil {
		return result, err
	}
	if err := r.reconcilePDB(ctx, ds); err != nil {
		return result, err
	}
	if err := r.reconcileScaling(ctx, ds, cfg, dep.Name); err != nil {
		return result, err
	}

	health, err := r.inspectPods(ctx, ds)
	if err != nil {
		return result, err
	}
	ds.Status.Replicas = dep.Status.Replicas
	ds.Status.ReadyReplicas = dep.Status.ReadyReplicas
	if avail, msg := deploymentAvailable(dep); avail {
		r.setCondition(ds, doclingv1alpha1.ConditionAvailable, metav1.ConditionTrue, doclingv1alpha1.ReasonDeploymentAvailable, health.message())
	} else {
		r.setCondition(ds, doclingv1alpha1.ConditionAvailable, metav1.ConditionFalse, doclingv1alpha1.ReasonDeploymentProgressing, msg)
	}

	if modelsReady && cfg.ModelsSource == doclingv1alpha1.ModelsSourceRelease {
		if err := r.reconcileCleanup(ctx, ds, cfg, dep); err != nil {
			return result, err
		}
	}

	if requeue := r.reconcileHealth(ctx, ds, cfg, dep, health); requeue > 0 {
		result.RequeueAfter = requeue
	}
	return result, nil
}

// reconcileAuth makes sure the API-key Secret exists.
func (r *DoclingServerReconciler) reconcileAuth(ctx context.Context, ds *doclingv1alpha1.DoclingServer, cfg *resolved) error {
	if !cfg.AuthEnabled {
		ds.Status.APIKeySecret = nil
		return nil
	}
	ds.Status.APIKeySecret = cfg.APIKeyRef.DeepCopy()
	if ds.Spec.Auth.SecretRef != nil {
		// User-managed Secret: only verify it exists so the pods will start.
		var s corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: cfg.APIKeyRef.Name}, &s); err != nil {
			if apierrors.IsNotFound(err) {
				r.Recorder.Eventf(ds, corev1.EventTypeWarning, "SecretMissing", "spec.auth.secretRef Secret %q not found", cfg.APIKeyRef.Name)
			}
			return err
		}
		if _, ok := s.Data[cfg.APIKeyRef.Key]; !ok {
			return fmt.Errorf("secret %q has no key %q", cfg.APIKeyRef.Name, cfg.APIKeyRef.Key)
		}
		return nil
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: cfg.APIKeyRef.Name, Namespace: ds.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		for k, v := range commonLabels(ds, "auth") {
			secret.Labels[k] = v
		}
		if len(secret.Data[cfg.APIKeyRef.Key]) == 0 {
			key, err := generateAPIKey()
			if err != nil {
				return err
			}
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data[cfg.APIKeyRef.Key] = []byte(key)
		}
		return controllerutil.SetControllerReference(ds, secret, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op == controllerutil.OperationResultCreated {
		r.Recorder.Eventf(ds, corev1.EventTypeNormal, "APIKeyGenerated", "generated API key Secret %s", secret.Name)
	}
	return nil
}

// reconcileModels manages the PVC and the download Job. It returns the model
// directory the Deployment must use ("" when nothing is ready yet or the
// models are bundled in the image) and whether the desired set is ready.
func (r *DoclingServerReconciler) reconcileModels(ctx context.Context, ds *doclingv1alpha1.DoclingServer, cfg *resolved) (string, bool, error) {
	st := &ds.Status.Models
	st.Source = cfg.ModelsSource

	if cfg.ModelsSource == doclingv1alpha1.ModelsSourceImage {
		// Bundled models: anything previously on the PVC is left in place
		// (switching back to release mode must not re-download).
		st.DesiredDir, st.DownloadJob, st.ClaimName = "", "", ""
		r.setCondition(ds, doclingv1alpha1.ConditionModelsReady, metav1.ConditionTrue, doclingv1alpha1.ReasonBundledInImage, "models are bundled in "+cfg.Image)
		return "", true, nil
	}

	// PVC.
	st.ClaimName = cfg.ClaimName
	if ds.Spec.Models.Storage.ExistingClaim == "" {
		pvc := buildPVC(ds, cfg)
		err := r.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{})
		if apierrors.IsNotFound(err) {
			if err := controllerutil.SetControllerReference(ds, pvc, r.Scheme); err != nil {
				return "", false, err
			}
			if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
				return "", false, fmt.Errorf("create PVC: %w", err)
			}
			r.Recorder.Eventf(ds, corev1.EventTypeNormal, "ClaimCreated", "created models PVC %s (%s, %v)", pvc.Name, cfg.StorageSize.String(), cfg.AccessModes)
		} else if err != nil {
			return "", false, err
		}
	}

	// Desired directory. A new spec hash resets the recovery counter so the
	// attempt budget is per model set.
	hash := modelSetHash(cfg)
	base := modelsDirName(cfg.ModelsVersion, hash, 0)
	if st.DesiredDir != "" && !sameModelSet(st.DesiredDir, base) {
		st.RecoveryAttempts = 0
	}
	desired := modelsDirName(cfg.ModelsVersion, hash, st.RecoveryAttempts)
	st.DesiredDir = desired

	if st.ActiveDir == desired {
		r.setCondition(ds, doclingv1alpha1.ConditionModelsReady, metav1.ConditionTrue, doclingv1alpha1.ReasonVerified, "model set "+desired+" is downloaded and verified")
		return st.ActiveDir, true, nil
	}

	// Download Job for the desired directory.
	job := buildDownloadJob(ds, cfg, desired)
	st.DownloadJob = job.Name
	existing := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKeyFromObject(job), existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := controllerutil.SetControllerReference(ds, job, r.Scheme); err != nil {
			return "", false, err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", false, fmt.Errorf("create download job: %w", err)
		}
		r.Recorder.Eventf(ds, corev1.EventTypeNormal, doclingv1alpha1.ReasonDownloading, "started download Job %s for model set %s", job.Name, desired)
		r.setCondition(ds, doclingv1alpha1.ConditionModelsReady, metav1.ConditionFalse, doclingv1alpha1.ReasonDownloading, servingWhile(st.ActiveDir, "downloading "+desired))
		return st.ActiveDir, false, nil
	case err != nil:
		return "", false, err
	}

	state, msg := jobStatus(existing)
	switch state {
	case jobSucceeded:
		if st.ActiveDir != "" && st.ActiveDir != desired {
			st.PreviousDirs = appendUnique(st.PreviousDirs, st.ActiveDir)
		}
		st.ActiveDir = desired
		st.ActiveVersion = cfg.ModelsVersion
		r.Recorder.Eventf(ds, corev1.EventTypeNormal, doclingv1alpha1.ReasonVerified, "model set %s downloaded and verified", desired)
		r.setCondition(ds, doclingv1alpha1.ConditionModelsReady, metav1.ConditionTrue, doclingv1alpha1.ReasonVerified, "model set "+desired+" is downloaded and verified")
		return st.ActiveDir, true, nil
	case jobFailed:
		r.setCondition(ds, doclingv1alpha1.ConditionModelsReady, metav1.ConditionFalse, doclingv1alpha1.ReasonDownloadFailed, servingWhile(st.ActiveDir, fmt.Sprintf("download Job %s failed: %s (delete the Job to retry)", existing.Name, msg)))
		r.setCondition(ds, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonDownloadFailed, fmt.Sprintf("download Job %s failed: %s", existing.Name, msg))
		return st.ActiveDir, false, nil
	default:
		r.setCondition(ds, doclingv1alpha1.ConditionModelsReady, metav1.ConditionFalse, doclingv1alpha1.ReasonDownloading, servingWhile(st.ActiveDir, "downloading "+desired))
		return st.ActiveDir, false, nil
	}
}

// reconcileDeployment applies the serve Deployment.
func (r *DoclingServerReconciler) reconcileDeployment(ctx context.Context, ds *doclingv1alpha1.DoclingServer, cfg *resolved, modelsDir string) (*appsv1.Deployment, error) {
	dep := buildDeployment(ds, cfg, modelsDir)
	before := &appsv1.Deployment{}
	known := r.Get(ctx, client.ObjectKeyFromObject(dep), before) == nil
	if err := r.apply(ctx, ds, dep); err != nil {
		return nil, fmt.Errorf("apply Deployment: %w", err)
	}
	if !known {
		r.Recorder.Eventf(ds, corev1.EventTypeNormal, doclingv1alpha1.ReasonRollingOut, "Deployment %s created (models: %s)", dep.Name, modelsDir)
	} else if before.Generation != dep.Generation {
		r.Recorder.Eventf(ds, corev1.EventTypeNormal, doclingv1alpha1.ReasonRollingOut, "Deployment %s updated (models: %s)", dep.Name, modelsDir)
	}
	return dep, nil
}

// reconcileService applies the Service and records the endpoint.
func (r *DoclingServerReconciler) reconcileService(ctx context.Context, ds *doclingv1alpha1.DoclingServer, cfg *resolved) error {
	svc := buildService(ds, cfg)
	if err := r.apply(ctx, ds, svc); err != nil {
		return fmt.Errorf("apply Service: %w", err)
	}
	ds.Status.Endpoint = ""
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		host := ing.Hostname
		if host == "" {
			host = ing.IP
		}
		if host != "" {
			ds.Status.Endpoint = fmt.Sprintf("http://%s:%d", host, cfg.ServicePort)
			break
		}
	}
	if ds.Status.Endpoint == "" && svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		ds.Status.Endpoint = fmt.Sprintf("http://%s.%s.svc:%d", svc.Name, svc.Namespace, cfg.ServicePort)
	}
	return nil
}

// reconcilePDB applies or removes the PodDisruptionBudget.
func (r *DoclingServerReconciler) reconcilePDB(ctx context.Context, ds *doclingv1alpha1.DoclingServer) error {
	if ds.Spec.Scaling.MinAvailable == nil {
		return deleteIfExists(ctx, r.Client, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: ds.Name, Namespace: ds.Namespace}})
	}
	if err := r.apply(ctx, ds, buildPDB(ds, *ds.Spec.Scaling.MinAvailable)); err != nil {
		return fmt.Errorf("apply PDB: %w", err)
	}
	return nil
}

// reconcileScaling applies the HPA or the KEDA ScaledObject and removes the
// other one. KEDA creates its own HPA, so both must never coexist.
func (r *DoclingServerReconciler) reconcileScaling(ctx context.Context, ds *doclingv1alpha1.DoclingServer, cfg *resolved, deploymentName string) error {
	hpaKey := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: ds.Name, Namespace: ds.Namespace}}
	soKey := &unstructured.Unstructured{}
	soKey.SetGroupVersionKind(kedaScaledObjectGVK)
	soKey.SetName(ds.Name)
	soKey.SetNamespace(ds.Namespace)

	switch cfg.ScalingMode {
	case doclingv1alpha1.ScalingNone:
		ds.Status.Scaler = ""
		if err := deleteIfExists(ctx, r.Client, hpaKey); err != nil {
			return err
		}
		if r.KEDAAvailable {
			if err := deleteIfExists(ctx, r.Client, soKey); err != nil {
				return err
			}
		}
		r.setCondition(ds, doclingv1alpha1.ConditionScalingReady, metav1.ConditionTrue, doclingv1alpha1.ReasonScalingDisabled, fmt.Sprintf("fixed at %d replicas", cfg.MinReplicas))
		return nil

	case doclingv1alpha1.ScalingKEDA:
		if err := deleteIfExists(ctx, r.Client, hpaKey); err != nil {
			return err
		}
		if !r.KEDAAvailable {
			ds.Status.Scaler = ""
			r.setCondition(ds, doclingv1alpha1.ConditionScalingReady, metav1.ConditionFalse, doclingv1alpha1.ReasonKEDANotInstalled, "keda.sh/v1alpha1 ScaledObject is not served by this cluster; install KEDA or use spec.scaling.mode=hpa")
			return nil
		}
		so := buildScaledObject(ds, cfg, deploymentName)
		if err := r.apply(ctx, ds, so); err != nil {
			return fmt.Errorf("apply ScaledObject: %w", err)
		}
		ds.Status.Scaler = "scaledobject/" + so.GetName()
		r.setCondition(ds, doclingv1alpha1.ConditionScalingReady, metav1.ConditionTrue, doclingv1alpha1.ReasonKEDAConfigured, fmt.Sprintf("KEDA ScaledObject %s: %d-%d replicas", so.GetName(), cfg.MinReplicas, cfg.MaxReplicas))
		return nil

	default: // hpa
		if r.KEDAAvailable {
			if err := deleteIfExists(ctx, r.Client, soKey); err != nil {
				return err
			}
		}
		hpa := buildHPA(ds, cfg, deploymentName)
		if err := r.apply(ctx, ds, hpa); err != nil {
			return fmt.Errorf("apply HPA: %w", err)
		}
		ds.Status.Scaler = "hpa/" + hpa.Name
		r.setCondition(ds, doclingv1alpha1.ConditionScalingReady, metav1.ConditionTrue, doclingv1alpha1.ReasonHPAConfigured, fmt.Sprintf("HPA %s: %d-%d replicas", hpa.Name, cfg.MinReplicas, cfg.MaxReplicas))
		return nil
	}
}

// reconcileCleanup deletes superseded model directories once the Deployment
// fully runs the active one, keeping spec.models.keepPreviousVersions.
func (r *DoclingServerReconciler) reconcileCleanup(ctx context.Context, ds *doclingv1alpha1.DoclingServer, cfg *resolved, dep *appsv1.Deployment) error {
	st := &ds.Status.Models
	keep := int(ds.Spec.Models.KeepPreviousVersions)
	if len(st.PreviousDirs) <= keep {
		return nil
	}
	if dep.Spec.Template.Annotations[annotationModelsDir] != st.ActiveDir || !deploymentRolledOut(dep) {
		// Old pods may still read the old directory.
		return nil
	}
	victims := append([]string(nil), st.PreviousDirs[:len(st.PreviousDirs)-keep]...)
	job := buildCleanupJob(ds, cfg, victims)
	existing := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKeyFromObject(job), existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := controllerutil.SetControllerReference(ds, job, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create cleanup job: %w", err)
		}
		r.Recorder.Eventf(ds, corev1.EventTypeNormal, "ModelsCleanup", "started cleanup Job %s for %v", job.Name, victims)
		return nil
	case err != nil:
		return err
	}
	state, msg := jobStatus(existing)
	switch state {
	case jobSucceeded:
		st.PreviousDirs = removeAll(st.PreviousDirs, victims)
		r.Recorder.Eventf(ds, corev1.EventTypeNormal, "ModelsCleanup", "removed superseded model directories %v", victims)
		return client.IgnoreNotFound(r.Delete(ctx, existing, client.PropagationPolicy(metav1.DeletePropagationBackground)))
	case jobFailed:
		r.Recorder.Eventf(ds, corev1.EventTypeWarning, "ModelsCleanupFailed", "cleanup Job %s failed: %s", existing.Name, msg)
		return nil
	default:
		return nil
	}
}

// reconcileHealth detects crash-looping serve pods and, when enabled, kicks
// off a bounded re-download of the model set into a fresh directory (the
// regular download-then-rollout path), which heals a corrupted volume.
func (r *DoclingServerReconciler) reconcileHealth(ctx context.Context, ds *doclingv1alpha1.DoclingServer, cfg *resolved, dep *appsv1.Deployment, health podHealth) time.Duration {
	st := &ds.Status.Models
	log := logf.FromContext(ctx)

	// A failed download is already reported by reconcileModels.
	if c := meta.FindStatusCondition(ds.Status.Conditions, doclingv1alpha1.ConditionDegraded); c != nil &&
		c.Status == metav1.ConditionTrue && c.Reason == doclingv1alpha1.ReasonDownloadFailed &&
		meta.IsStatusConditionFalse(ds.Status.Conditions, doclingv1alpha1.ConditionModelsReady) {
		return 0
	}

	if health.CrashLooping == 0 {
		if st.ActiveDir != st.DesiredDir && cfg.ModelsSource == doclingv1alpha1.ModelsSourceRelease {
			r.setCondition(ds, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonRecovering, "re-downloading models into "+st.DesiredDir)
			return 0
		}
		r.setCondition(ds, doclingv1alpha1.ConditionDegraded, metav1.ConditionFalse, doclingv1alpha1.ReasonHealthy, health.message())
		return 0
	}

	msg := health.message()
	switch {
	case cfg.ModelsSource != doclingv1alpha1.ModelsSourceRelease || !cfg.RecoveryEnabled:
		r.setCondition(ds, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonPodsCrashLooping, msg)
		return 0
	case st.ActiveDir != st.DesiredDir:
		// A recovery download is already in progress.
		r.setCondition(ds, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonRecovering, msg+"; re-downloading models into "+st.DesiredDir)
		return 0
	case dep.Spec.Template.Annotations[annotationModelsDir] != st.ActiveDir:
		// Pods still from a previous template; let the rollout finish.
		r.setCondition(ds, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonPodsCrashLooping, msg+"; rollout in progress")
		return 0
	case st.RecoveryAttempts >= cfg.RecoveryMax:
		r.setCondition(ds, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonRecoveryExhausted,
			fmt.Sprintf("%s; gave up after %d model re-downloads (change spec.models to reset)", msg, st.RecoveryAttempts))
		return 0
	case st.LastRecoveryTime != nil && time.Since(st.LastRecoveryTime.Time) < recoveryCooldown:
		wait := recoveryCooldown - time.Since(st.LastRecoveryTime.Time)
		r.setCondition(ds, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonPodsCrashLooping,
			fmt.Sprintf("%s; next model re-download in %s", msg, wait.Round(time.Second)))
		return wait
	}

	st.RecoveryAttempts++
	now := metav1.Now()
	st.LastRecoveryTime = &now
	st.DesiredDir = ""
	log.Info("serve pods crash-looping, re-downloading models", "attempt", st.RecoveryAttempts)
	r.Recorder.Eventf(ds, corev1.EventTypeWarning, doclingv1alpha1.ReasonRecovering,
		"%s; re-downloading model set (attempt %d/%d)", msg, st.RecoveryAttempts, cfg.RecoveryMax)
	r.setCondition(ds, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonRecovering, msg+"; re-downloading models")
	// The next reconcile derives the new directory and starts the Job.
	return time.Second
}

// finalize handles deletion. Objects with owner references are collected by
// the garbage collector. Model directories on a user-provided PVC are
// removed by a cleanup Job before the finalizer is released.
func (r *DoclingServerReconciler) finalize(ctx context.Context, ds *doclingv1alpha1.DoclingServer) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ds, finalizerName) {
		return ctrl.Result{}, nil
	}
	st := ds.Status.Models
	if ds.Spec.Models.Storage.ExistingClaim != "" && (st.ActiveDir != "" || len(st.PreviousDirs) > 0) {
		cfg, err := resolve(ds)
		if err == nil {
			dirs := append([]string(nil), st.PreviousDirs...)
			if st.ActiveDir != "" {
				dirs = appendUnique(dirs, st.ActiveDir)
			}
			if st.DesiredDir != "" {
				dirs = appendUnique(dirs, st.DesiredDir)
			}
			job := buildCleanupJob(ds, cfg, dirs)
			existing := &batchv1.Job{}
			err := r.Get(ctx, client.ObjectKeyFromObject(job), existing)
			switch {
			case apierrors.IsNotFound(err):
				if err := controllerutil.SetControllerReference(ds, job, r.Scheme); err != nil {
					return ctrl.Result{}, err
				}
				if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			case err != nil:
				return ctrl.Result{}, err
			}
			state, msg := jobStatus(existing)
			switch state {
			case jobRunning:
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			case jobFailed:
				// Do not block deletion forever on a storage problem.
				r.Recorder.Eventf(ds, corev1.EventTypeWarning, "ModelsCleanupFailed", "cleanup Job %s failed: %s; leaving directories %v on %s", existing.Name, msg, dirs, cfg.ClaimName)
			}
		}
	}
	controllerutil.RemoveFinalizer(ds, finalizerName)
	return ctrl.Result{}, r.Update(ctx, ds)
}

// summarize computes the Ready condition from the others.
func (r *DoclingServerReconciler) summarize(ds *doclingv1alpha1.DoclingServer) {
	models := meta.IsStatusConditionTrue(ds.Status.Conditions, doclingv1alpha1.ConditionModelsReady)
	avail := meta.IsStatusConditionTrue(ds.Status.Conditions, doclingv1alpha1.ConditionAvailable)
	degraded := meta.IsStatusConditionTrue(ds.Status.Conditions, doclingv1alpha1.ConditionDegraded)
	switch {
	case models && avail && !degraded:
		r.setCondition(ds, doclingv1alpha1.ConditionReady, metav1.ConditionTrue, doclingv1alpha1.ReasonHealthy, "models verified, Deployment available")
	default:
		var why string
		for _, t := range []string{doclingv1alpha1.ConditionDegraded, doclingv1alpha1.ConditionModelsReady, doclingv1alpha1.ConditionAvailable} {
			c := meta.FindStatusCondition(ds.Status.Conditions, t)
			if c == nil {
				continue
			}
			if (t == doclingv1alpha1.ConditionDegraded) == (c.Status == metav1.ConditionTrue) {
				why = c.Type + ": " + c.Message
				break
			}
		}
		r.setCondition(ds, doclingv1alpha1.ConditionReady, metav1.ConditionFalse, doclingv1alpha1.ReasonNotReady, why)
	}
}

func (r *DoclingServerReconciler) setCondition(ds *doclingv1alpha1.DoclingServer, t string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&ds.Status.Conditions, metav1.Condition{
		Type:               t,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ds.Generation,
	})
}

// patchStatus writes status only when it changed, keeping reconciles
// idempotent and the resourceVersion stable.
func (r *DoclingServerReconciler) patchStatus(ctx context.Context, ds, orig *doclingv1alpha1.DoclingServer) error {
	if equality.Semantic.DeepEqual(orig.Status, ds.Status) {
		return nil
	}
	return r.Status().Patch(ctx, ds, client.MergeFrom(orig))
}

func servingWhile(active, msg string) string {
	if active == "" {
		return msg
	}
	return "serving " + active + " while " + msg
}

// sameModelSet reports whether dir belongs to the same spec hash as base,
// ignoring the recovery suffix.
func sameModelSet(dir, base string) bool {
	if dir == base {
		return true
	}
	return len(dir) > len(base) && dir[:len(base)] == base && dir[len(base):len(base)+2] == "-r"
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func removeAll(list, victims []string) []string {
	var out []string
	for _, x := range list {
		keep := true
		for _, v := range victims {
			if x == v {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, x)
		}
	}
	return out
}

func deleteIfExists(ctx context.Context, c client.Client, obj client.Object) error {
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return client.IgnoreNotFound(c.Delete(ctx, obj))
}

// apply server-side applies obj with the operator as field manager and ds
// as controller owner. Only fields present in obj are owned, so fields set
// by others (HPA replicas, cloud-provider nodePorts, API defaults) are
// untouched, and an unchanged apply is a no-op write on the API server.
func (r *DoclingServerReconciler) apply(ctx context.Context, ds *doclingv1alpha1.DoclingServer, obj client.Object) error {
	if err := controllerutil.SetControllerReference(ds, obj, r.Scheme); err != nil {
		return err
	}
	gvk, err := apiutil.GVKForObject(obj, r.Scheme)
	if err != nil {
		return err
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	obj.SetManagedFields(nil)
	obj.SetResourceVersion("")
	return r.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(managedBy))
}

func toUnstructured(obj interface{}) (map[string]interface{}, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
}

// SetupWithManager registers the controller.
func (r *DoclingServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("doclingserver-controller")
	}
	if _, err := mgr.GetRESTMapper().RESTMapping(kedaScaledObjectGVK.GroupKind(), kedaScaledObjectGVK.Version); err == nil {
		r.KEDAAvailable = true
	}

	b := ctrl.NewControllerManagedBy(mgr).
		For(&doclingv1alpha1.DoclingServer{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(mapPodToServer), builder.WithPredicates(podStatusChanged()))
	if r.KEDAAvailable {
		so := &unstructured.Unstructured{}
		so.SetGroupVersionKind(kedaScaledObjectGVK)
		b = b.Owns(so)
	}
	return b.Named("doclingserver").Complete(r)
}

// mapPodToServer enqueues the DoclingServer owning a serve pod.
func mapPodToServer(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[labelName] != appName || labels[labelManagedBy] != managedBy {
		return nil
	}
	name := labels[labelServer]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// podStatusChanged only wakes the controller on pod status transitions
// (container state, readiness) and deletions; creations are covered by the
// Deployment watch.
func podStatusChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
			newPod, ok2 := e.ObjectNew.(*corev1.Pod)
			if !ok1 || !ok2 {
				return false
			}
			return isPodReady(oldPod) != isPodReady(newPod) ||
				isCrashLooping(oldPod) != isCrashLooping(newPod) ||
				!equality.Semantic.DeepEqual(oldPod.Status.ContainerStatuses, newPod.Status.ContainerStatuses)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
