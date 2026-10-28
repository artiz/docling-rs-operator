/*
Copyright (c) 2026 Artem Kustikov

SPDX-License-Identifier: MIT
*/

package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	doclingv1alpha1 "github.com/artiz/docling-rs-operator/api/v1alpha1"
)

const (
	// Labels and annotations owned by the operator.
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelComponent = "app.kubernetes.io/component"
	labelServer    = "docling.artiz.dev/server"
	labelJobKind   = "docling.artiz.dev/job"

	annotationModelsDir  = "docling.artiz.dev/models-dir"
	annotationSpecHash   = "docling.artiz.dev/spec-hash"
	annotationCleanupSet = "docling.artiz.dev/cleanup-dirs"

	appName         = "docling-serve"
	managedBy       = "docling-rs-operator"
	finalizerName   = "docling.artiz.dev/finalizer"
	jobKindDownload = "models-download"
	jobKindCleanup  = "models-cleanup"

	defaultImageCPU  = "ghcr.io/docling-project/docling-rs-serve"
	defaultImageCUDA = "ghcr.io/docling-project/docling-rs-serve-cuda"

	defaultReleaseBase = "https://github.com/docling-project/docling.rs/releases/download/"

	// Mount points.
	modelsRootMount    = "/models"
	modelsMountLinux   = "/app/.models"
	modelsMountWindows = `C:\models`

	// Prometheus metric exposed by docling-serve on /metrics.
	metricInFlight = "docling_serve_requests_in_flight"

	apiKeySecretKey = "api-key"
	apiKeyEnv       = "DOCLING_SERVE_API_KEY"
)

// resolved holds the effective configuration after defaults are applied.
// Defaults are also declared on the CRD, but the reconciler must not rely on
// the API server having applied them (envtest, old CRD versions, unit tests).
type resolved struct {
	Image             string
	PullPolicy        corev1.PullPolicy
	Accelerator       doclingv1alpha1.AcceleratorType
	GPUResource       corev1.ResourceName
	GPUCount          int64
	ExecutionProvider string
	Windows           bool

	ModelsSource    doclingv1alpha1.ModelsSource
	ModelsVersion   string
	ReleaseBaseURL  string
	ScriptURL       string
	ScriptArgs      []string
	DownloaderImage string
	ClaimName       string
	StorageSize     resource.Quantity
	AccessModes     []corev1.PersistentVolumeAccessMode
	ModelsMount     string
	RecoveryEnabled bool
	RecoveryMax     int32

	Port                 int32
	Command              []string
	Args                 []string
	LogLevel             string
	Warmup               bool
	StartupProbeFailures int32

	AuthEnabled bool
	APIKeyRef   *corev1.SecretKeySelector

	ServiceType corev1.ServiceType
	ServicePort int32

	ScalingMode doclingv1alpha1.ScalingMode
	MinReplicas int32
	MaxReplicas int32
}

// trueUnless treats an unset *bool as true (the CRD default for these flags).
func trueUnless(v *bool) bool {
	return v == nil || *v
}

func int32Default(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

// resolve applies defaults and validates the spec. The returned error is a
// user error (InvalidSpec) and must not be retried.
func resolve(ds *doclingv1alpha1.DoclingServer) (*resolved, error) {
	r := &resolved{}
	for _, step := range []func(*doclingv1alpha1.DoclingServer, *resolved) error{
		resolveAccelerator,
		resolveImage,
		resolveModels,
		resolveServer,
		resolveAuth,
		resolveService,
		resolveScaling,
	} {
		if err := step(ds, r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func resolveAccelerator(ds *doclingv1alpha1.DoclingServer, r *resolved) error {
	s := ds.Spec.Accelerator
	r.Accelerator = s.Type
	if r.Accelerator == "" {
		r.Accelerator = doclingv1alpha1.AcceleratorCPU
	}
	r.GPUCount = s.Count
	if r.GPUCount <= 0 {
		r.GPUCount = 1
	}
	switch r.Accelerator {
	case doclingv1alpha1.AcceleratorCPU:
		r.Image = defaultImageCPU
		r.ExecutionProvider = "cpu"
	case doclingv1alpha1.AcceleratorCUDA:
		r.Image = defaultImageCUDA
		r.ExecutionProvider = "cuda"
		r.GPUResource = "nvidia.com/gpu"
	case doclingv1alpha1.AcceleratorDirectML:
		r.ExecutionProvider = "auto"
		r.GPUResource = "microsoft.com/directx"
		r.Windows = true
		if ds.Spec.Image.Repository == "" {
			return fmt.Errorf("accelerator directml requires spec.image.repository: upstream publishes no Windows/DirectML image, build docling-serve with --features directml")
		}
	default:
		return fmt.Errorf("unknown accelerator type %q", r.Accelerator)
	}
	if s.ResourceName != "" {
		r.GPUResource = corev1.ResourceName(s.ResourceName)
	}
	if s.ExecutionProvider != "" {
		r.ExecutionProvider = s.ExecutionProvider
	}
	return nil
}

func resolveImage(ds *doclingv1alpha1.DoclingServer, r *resolved) error {
	s := ds.Spec.Image
	if s.Repository != "" {
		r.Image = s.Repository
	}
	tag := s.Tag
	if tag == "" {
		tag = "1"
	}
	if !strings.Contains(r.Image, "@") {
		r.Image = r.Image + ":" + tag
	}
	r.PullPolicy = s.PullPolicy
	if r.PullPolicy == "" {
		r.PullPolicy = corev1.PullIfNotPresent
	}
	return nil
}

func resolveModels(ds *doclingv1alpha1.DoclingServer, r *resolved) error {
	s := ds.Spec.Models
	r.ModelsSource = s.Source
	if r.ModelsSource == "" {
		r.ModelsSource = doclingv1alpha1.ModelsSourceRelease
	}
	r.ModelsVersion = s.Version
	if r.ModelsVersion == "" {
		r.ModelsVersion = "models-v1"
	}
	r.ReleaseBaseURL = s.ReleaseBaseURL
	if r.ReleaseBaseURL == "" {
		r.ReleaseBaseURL = defaultReleaseBase + r.ModelsVersion
	}
	r.ScriptURL = s.ScriptURL
	if r.ScriptURL == "" {
		r.ScriptURL = "https://raw.githubusercontent.com/docling-project/docling.rs/master/scripts/install/download_dependencies.sh"
	}
	r.ScriptArgs = scriptArgs(s.Options)
	r.DownloaderImage = s.Downloader.Image
	if r.DownloaderImage == "" {
		r.DownloaderImage = "docker.io/curlimages/curl:8.11.1"
	}
	r.ClaimName = s.Storage.ExistingClaim
	if r.ClaimName == "" {
		r.ClaimName = ds.Name + "-models"
	}
	r.StorageSize = s.Storage.Size
	if r.StorageSize.IsZero() {
		r.StorageSize = resource.MustParse("20Gi")
	}
	r.AccessModes = s.Storage.AccessModes
	if len(r.AccessModes) == 0 {
		r.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	}
	r.ModelsMount = modelsMountLinux
	if r.Windows {
		r.ModelsMount = modelsMountWindows
	}
	r.RecoveryEnabled = trueUnless(s.Recovery.Enabled)
	r.RecoveryMax = s.Recovery.MaxAttempts
	if r.RecoveryMax == 0 {
		r.RecoveryMax = 3
	}
	if !r.RecoveryEnabled {
		r.RecoveryMax = 0
	}
	return nil
}

func resolveServer(ds *doclingv1alpha1.DoclingServer, r *resolved) error {
	s := ds.Spec.Server
	r.Port = int32Default(s.Port, 5001)
	r.Command = s.Command
	if len(r.Command) == 0 {
		r.Command = []string{"docling-serve"}
	}
	r.LogLevel = s.LogLevel
	if r.LogLevel == "" {
		r.LogLevel = "info"
	}
	r.Warmup = trueUnless(s.Warmup)
	r.Args = serverArgs(s, r.Port, r.Warmup)
	startup := int32Default(s.StartupTimeoutSeconds, 600)
	r.StartupProbeFailures = (startup + 4) / 5
	if r.StartupProbeFailures < 1 {
		r.StartupProbeFailures = 1
	}
	return nil
}

func resolveAuth(ds *doclingv1alpha1.DoclingServer, r *resolved) error {
	s := ds.Spec.Auth
	r.AuthEnabled = trueUnless(s.Enabled)
	if !r.AuthEnabled {
		return nil
	}
	if s.SecretRef != nil {
		if s.SecretRef.Name == "" {
			return fmt.Errorf("spec.auth.secretRef.name is required")
		}
		ref := s.SecretRef.DeepCopy()
		if ref.Key == "" {
			ref.Key = apiKeySecretKey
		}
		r.APIKeyRef = ref
		return nil
	}
	r.APIKeyRef = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: ds.Name + "-api-key"},
		Key:                  apiKeySecretKey,
	}
	return nil
}

func resolveService(ds *doclingv1alpha1.DoclingServer, r *resolved) error {
	s := ds.Spec.Service
	r.ServiceType = s.Type
	if r.ServiceType == "" {
		r.ServiceType = corev1.ServiceTypeLoadBalancer
	}
	r.ServicePort = int32Default(s.Port, 80)
	return nil
}

func resolveScaling(ds *doclingv1alpha1.DoclingServer, r *resolved) error {
	s := ds.Spec.Scaling
	r.ScalingMode = s.Mode
	if r.ScalingMode == "" {
		r.ScalingMode = doclingv1alpha1.ScalingHPA
	}
	r.MinReplicas = int32Default(s.MinReplicas, 1)
	r.MaxReplicas = int32Default(s.MaxReplicas, 5)
	if r.MaxReplicas < r.MinReplicas {
		return fmt.Errorf("spec.scaling.maxReplicas (%d) must be >= minReplicas (%d)", r.MaxReplicas, r.MinReplicas)
	}
	if r.ScalingMode == doclingv1alpha1.ScalingKEDA {
		if s.KEDA == nil || s.KEDA.PrometheusAddress == "" {
			return fmt.Errorf("spec.scaling.keda.prometheusAddress is required in keda mode")
		}
		if s.TargetInFlightPerReplica == 0 && s.TargetCPUUtilization == 0 {
			return fmt.Errorf("keda mode needs targetInFlightPerReplica or targetCPUUtilization")
		}
	}
	return nil
}

// scriptArgs translates ModelOptions into download_dependencies.sh flags.
func scriptArgs(o doclingv1alpha1.ModelOptions) []string {
	args := make([]string, 0, 8+len(o.ASRPresets))
	if !trueUnless(o.ASR) {
		args = append(args, "--no-asr")
	}
	for _, p := range o.ASRPresets {
		args = append(args, "--asr-model="+p)
	}
	if !trueUnless(o.Int8) {
		args = append(args, "--no-int8")
	}
	if !trueUnless(o.Chunk) {
		args = append(args, "--no-chunk")
	}
	if o.Enrich {
		args = append(args, "--enrich")
	}
	if o.Embed {
		args = append(args, "--embed")
	}
	if o.NER {
		args = append(args, "--with-ner")
	}
	if o.Fonts {
		args = append(args, "--with-fonts")
	}
	return args
}

// serverArgs builds the docling-serve command line.
func serverArgs(s doclingv1alpha1.ServerSpec, port int32, warmup bool) []string {
	args := []string{
		"--addr", fmt.Sprintf("0.0.0.0:%d", port),
		"--concurrency", fmt.Sprint(int32Default(s.Concurrency, 2)),
		"--max-body-mb", fmt.Sprint(int32Default(s.MaxBodyMB, 256)),
		"--queue-size", fmt.Sprint(int32Default(s.QueueSize, 16)),
		"--result-ttl", fmt.Sprint(int32Default(s.ResultTTLSeconds, 600)),
	}
	if warmup {
		args = append(args, "--warmup")
	}
	if s.IdleUnloadSeconds > 0 {
		args = append(args, "--idle-unload-secs", fmt.Sprint(s.IdleUnloadSeconds))
	}
	if s.MaxMemoryMB != nil {
		args = append(args, "--max-memory-mb", fmt.Sprint(*s.MaxMemoryMB))
	}
	if s.AllowURLFetch {
		args = append(args, "--allow-url-fetch")
	}
	if s.AllowLocalImages {
		args = append(args, "--allow-local-images")
	}
	if s.Strict {
		args = append(args, "--strict")
	}
	if !trueUnless(s.EnableUI) {
		args = append(args, "--no-ui")
	}
	args = append(args, s.ExtraArgs...)
	return args
}

// selectorLabels are the immutable labels used by the Deployment selector,
// the Service and the pod watch.
func selectorLabels(ds *doclingv1alpha1.DoclingServer) map[string]string {
	return map[string]string{
		labelName:     appName,
		labelInstance: ds.Name,
		labelServer:   ds.Name,
	}
}

// commonLabels are applied to every object the operator creates.
func commonLabels(ds *doclingv1alpha1.DoclingServer, component string) map[string]string {
	l := selectorLabels(ds)
	l[labelManagedBy] = managedBy
	l[labelComponent] = component
	return l
}
