/*
Copyright (c) 2026 Artem Kustikov

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types reported in DoclingServer.status.conditions.
const (
	// ConditionModelsReady is True when the model set referenced by spec.models
	// has been downloaded, checksum-verified and is the one served by the
	// running Deployment (or is bundled in the image).
	ConditionModelsReady = "ModelsReady"
	// ConditionAvailable mirrors the Deployment's Available condition.
	ConditionAvailable = "Available"
	// ConditionScalingReady is True when the HPA / KEDA ScaledObject is in place.
	ConditionScalingReady = "ScalingReady"
	// ConditionDegraded is True when the operator detected a failure it is
	// recovering from (download failure, crash-looping pods, corrupt models).
	ConditionDegraded = "Degraded"
	// ConditionReady is the summary condition: models ready, Deployment
	// available, not degraded.
	ConditionReady = "Ready"
)

// Condition reasons.
const (
	ReasonBundledInImage        = "BundledInImage"
	ReasonDownloading           = "Downloading"
	ReasonDownloadFailed        = "DownloadFailed"
	ReasonVerified              = "Verified"
	ReasonRollingOut            = "RollingOut"
	ReasonRecovering            = "Recovering"
	ReasonRecoveryExhausted     = "RecoveryExhausted"
	ReasonPodsCrashLooping      = "PodsCrashLooping"
	ReasonDeploymentAvailable   = "DeploymentAvailable"
	ReasonDeploymentProgressing = "DeploymentProgressing"
	ReasonWaitingForModels      = "WaitingForModels"
	ReasonHPAConfigured         = "HPAConfigured"
	ReasonKEDAConfigured        = "KEDAConfigured"
	ReasonKEDANotInstalled      = "KEDANotInstalled"
	ReasonScalingDisabled       = "ScalingDisabled"
	ReasonHealthy               = "Healthy"
	ReasonNotReady              = "NotReady"
	ReasonInvalidSpec           = "InvalidSpec"
)

// AcceleratorType selects the hardware the server runs on.
// +kubebuilder:validation:Enum=cpu;cuda;directml
type AcceleratorType string

const (
	AcceleratorCPU      AcceleratorType = "cpu"
	AcceleratorCUDA     AcceleratorType = "cuda"
	AcceleratorDirectML AcceleratorType = "directml"
)

// ModelsSource selects where ONNX models come from.
// +kubebuilder:validation:Enum=release;image
type ModelsSource string

const (
	// ModelsSourceRelease downloads models from the docling.rs GitHub release
	// into a shared PersistentVolumeClaim and verifies them against SHA256SUMS.
	ModelsSourceRelease ModelsSource = "release"
	// ModelsSourceImage uses the models bundled inside the serve container image.
	ModelsSourceImage ModelsSource = "image"
)

// ImageSpec configures the container image of the server.
type ImageSpec struct {
	// Repository of the serve image. Defaults to
	// ghcr.io/docling-project/docling-rs-serve (cpu) or
	// ghcr.io/docling-project/docling-rs-serve-cuda (cuda). DirectML requires a
	// user-built Windows image and has no default.
	// +optional
	Repository string `json:"repository,omitempty"`
	// Tag of the serve image.
	// +kubebuilder:default="1"
	// +optional
	Tag string `json:"tag,omitempty"`
	// +kubebuilder:default=IfNotPresent
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
	// +optional
	PullSecrets []corev1.LocalObjectReference `json:"pullSecrets,omitempty"`
}

// AcceleratorSpec configures hardware acceleration.
type AcceleratorSpec struct {
	// Type of accelerator. cpu (default), cuda (NVIDIA, Linux) or directml
	// (Windows nodes with the DirectX device plugin).
	// +kubebuilder:default=cpu
	// +optional
	Type AcceleratorType `json:"type,omitempty"`
	// Count of GPU devices requested per pod.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Count int64 `json:"count,omitempty"`
	// ResourceName overrides the extended resource requested for the GPU.
	// Defaults to nvidia.com/gpu for cuda and microsoft.com/directx for directml.
	// +optional
	ResourceName string `json:"resourceName,omitempty"`
	// ExecutionProvider overrides DOCLING_RS_EP. Defaults to cpu, cuda or auto
	// (directml) depending on Type.
	// +optional
	ExecutionProvider string `json:"executionProvider,omitempty"`
	// RuntimeClassName of the pods (e.g. "nvidia").
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`
}

// ModelOptions mirrors the opt-in flags of scripts/install/download_dependencies.sh.
type ModelOptions struct {
	// ASR fetches the default Whisper tiny models.
	// +kubebuilder:default=true
	// +optional
	ASR *bool `json:"asr,omitempty"`
	// ASRPresets are extra ASR presets (whisper_base_en, parakeet_tdt_0.6b_v3, ...).
	// +optional
	ASRPresets []string `json:"asrPresets,omitempty"`
	// Int8 fetches the INT8 quantized models (default on).
	// +kubebuilder:default=true
	// +optional
	Int8 *bool `json:"int8,omitempty"`
	// Chunk fetches the chunker tokenizer.
	// +kubebuilder:default=true
	// +optional
	Chunk *bool `json:"chunk,omitempty"`
	// Enrich fetches CodeFormulaV2 (~1.3 GB).
	// +optional
	Enrich bool `json:"enrich,omitempty"`
	// Embed fetches the bge-m3 embedder.
	// +optional
	Embed bool `json:"embed,omitempty"`
	// NER fetches the PII NER model.
	// +optional
	NER bool `json:"ner,omitempty"`
	// Fonts fetches Liberation and DejaVu fonts.
	// +optional
	Fonts bool `json:"fonts,omitempty"`
}

// ModelStorageSpec configures the shared PersistentVolumeClaim.
type ModelStorageSpec struct {
	// ExistingClaim uses an existing RWX PVC instead of creating one.
	// +optional
	ExistingClaim string `json:"existingClaim,omitempty"`
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// +kubebuilder:default="20Gi"
	// +optional
	Size resource.Quantity `json:"size,omitempty"`
	// AccessModes of the created PVC. Must allow concurrent readers across
	// nodes; defaults to ReadWriteMany.
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
}

// DownloaderSpec configures the Job that fetches models.
type DownloaderSpec struct {
	// Image with sh, curl, tar and sha256sum.
	// +kubebuilder:default="docker.io/curlimages/curl:8.11.1"
	// +optional
	Image string `json:"image,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +kubebuilder:default=3
	// +optional
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`
	// +kubebuilder:default=3600
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
	// Env adds extra variables, e.g. DOCLING_RS_MODELS_URL for a mirror or
	// HTTPS_PROXY.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// PodSecurityContext of the downloader pod. Defaults to running as root so
	// the PVC root is writable on any storage backend; override it (e.g.
	// runAsUser + fsGroup) for PodSecurity "restricted" namespaces.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`
}

// RecoverySpec configures self-healing.
type RecoverySpec struct {
	// Enabled turns on automatic re-download of models when the serve pods
	// crash-loop (e.g. a corrupted volume).
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// MaxAttempts bounds how many times models are re-downloaded per model
	// set before the operator gives up and only reports Degraded. Set
	// Enabled=false to turn recovery off.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxAttempts int32 `json:"maxAttempts,omitempty"`
}

// ModelsSpec describes the model set and its lifecycle.
type ModelsSpec struct {
	// Source of the models: release (download into shared PVC) or image.
	// +kubebuilder:default=release
	// +optional
	Source ModelsSource `json:"source,omitempty"`
	// Version is the GitHub release tag that hosts the models and SHA256SUMS.
	// +kubebuilder:default="models-v1"
	// +optional
	Version string `json:"version,omitempty"`
	// ReleaseBaseURL overrides the release download base URL
	// (DOCLING_RS_MODELS_URL). Defaults to
	// https://github.com/docling-project/docling.rs/releases/download/<version>.
	// +optional
	ReleaseBaseURL string `json:"releaseBaseURL,omitempty"`
	// ScriptURL is the download_dependencies.sh to run. Pin it to a commit
	// for reproducible model sets.
	// +kubebuilder:default="https://raw.githubusercontent.com/docling-project/docling.rs/master/scripts/install/download_dependencies.sh"
	// +optional
	ScriptURL string `json:"scriptURL,omitempty"`
	// +optional
	Options ModelOptions `json:"options,omitempty"`
	// +optional
	Storage ModelStorageSpec `json:"storage,omitempty"`
	// +optional
	Downloader DownloaderSpec `json:"downloader,omitempty"`
	// +optional
	Recovery RecoverySpec `json:"recovery,omitempty"`
	// KeepPreviousVersions keeps that many superseded model directories on the
	// PVC after a successful switch (for fast rollback). Default 0.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	KeepPreviousVersions int32 `json:"keepPreviousVersions,omitempty"`
}

// ServerSpec maps to docling-serve flags and environment.
type ServerSpec struct {
	// Command overrides the container entrypoint. Defaults to ["docling-serve"].
	// +optional
	Command []string `json:"command,omitempty"`
	// Port the server listens on.
	// +kubebuilder:default=5001
	// +optional
	Port int32 `json:"port,omitempty"`
	// Concurrency is the number of conversions in flight per replica
	// (--concurrency).
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	Concurrency int32 `json:"concurrency,omitempty"`
	// +kubebuilder:default=256
	// +optional
	MaxBodyMB int32 `json:"maxBodyMB,omitempty"`
	// +kubebuilder:default=16
	// +optional
	QueueSize int32 `json:"queueSize,omitempty"`
	// +kubebuilder:default=600
	// +optional
	ResultTTLSeconds int32 `json:"resultTTLSeconds,omitempty"`
	// Warmup loads the models at startup; /ready returns 503 until done.
	// +kubebuilder:default=true
	// +optional
	Warmup *bool `json:"warmup,omitempty"`
	// +optional
	IdleUnloadSeconds int32 `json:"idleUnloadSeconds,omitempty"`
	// +optional
	MaxMemoryMB *int32 `json:"maxMemoryMB,omitempty"`
	// +optional
	AllowURLFetch bool `json:"allowURLFetch,omitempty"`
	// +optional
	AllowLocalImages bool `json:"allowLocalImages,omitempty"`
	// +optional
	Strict bool `json:"strict,omitempty"`
	// EnableUI serves the test form at /.
	// +kubebuilder:default=true
	// +optional
	EnableUI *bool `json:"enableUI,omitempty"`
	// LogLevel sets RUST_LOG.
	// +kubebuilder:default="info"
	// +optional
	LogLevel string `json:"logLevel,omitempty"`
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// StartupTimeoutSeconds bounds model warmup before the pod is restarted.
	// +kubebuilder:default=600
	// +optional
	StartupTimeoutSeconds int32 `json:"startupTimeoutSeconds,omitempty"`
}

// AuthSpec configures API-key authentication (X-Api-Key header).
type AuthSpec struct {
	// Enabled requires X-Api-Key on all /v1 routes. When true and SecretRef is
	// empty the operator generates a Secret named <name>-api-key.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// SecretRef points to an existing Secret holding the API key.
	// +optional
	SecretRef *corev1.SecretKeySelector `json:"secretRef,omitempty"`
}

// ServiceSpec configures the Service in front of the replicas.
type ServiceSpec struct {
	// +kubebuilder:default=LoadBalancer
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`
	// +kubebuilder:default=80
	// +optional
	Port int32 `json:"port,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// +optional
	LoadBalancerClass *string `json:"loadBalancerClass,omitempty"`
	// +optional
	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`
	// +optional
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`
	// SessionAffinity defaults to None so load from a single client address
	// spreads across replicas. Set ClientIP when clients use the async flow
	// (/v1/convert/async + /v1/result), whose results live in the memory of
	// the replica that accepted the job.
	// +kubebuilder:default=None
	// +optional
	SessionAffinity corev1.ServiceAffinity `json:"sessionAffinity,omitempty"`
}

// ScalingMode selects the autoscaler.
// +kubebuilder:validation:Enum=none;hpa;keda
type ScalingMode string

const (
	ScalingNone ScalingMode = "none"
	ScalingHPA  ScalingMode = "hpa"
	ScalingKEDA ScalingMode = "keda"
)

// KEDASpec configures a KEDA ScaledObject with a Prometheus trigger.
type KEDASpec struct {
	// PrometheusAddress, e.g. http://prometheus-operated.monitoring:9090.
	PrometheusAddress string `json:"prometheusAddress"`
	// Query overrides the PromQL. Defaults to the sum of
	// docling_serve_requests_in_flight over the pods of this server.
	// +optional
	Query string `json:"query,omitempty"`
	// +kubebuilder:default=15
	// +optional
	PollingInterval int32 `json:"pollingInterval,omitempty"`
	// +kubebuilder:default=120
	// +optional
	CooldownPeriod int32 `json:"cooldownPeriod,omitempty"`
}

// ScalingSpec configures autoscaling by load.
type ScalingSpec struct {
	// Mode: hpa (default, CPU + optional in-flight pods metric via
	// prometheus-adapter), keda (Prometheus trigger) or none.
	// +kubebuilder:default=hpa
	// +optional
	Mode ScalingMode `json:"mode,omitempty"`
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxReplicas int32 `json:"maxReplicas,omitempty"`
	// TargetCPUUtilization adds a CPU utilization target (hpa mode). 0 disables.
	// +kubebuilder:default=70
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	TargetCPUUtilization int32 `json:"targetCPUUtilization,omitempty"`
	// TargetInFlightPerReplica scales on docling_serve_requests_in_flight. In
	// hpa mode this needs prometheus-adapter exposing the metric as a pods
	// metric; in keda mode it is the trigger threshold. 0 disables.
	// A good value is spec.server.concurrency.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TargetInFlightPerReplica int32 `json:"targetInFlightPerReplica,omitempty"`
	// +optional
	KEDA *KEDASpec `json:"keda,omitempty"`
	// +optional
	Behavior *autoscalingv2.HorizontalPodAutoscalerBehavior `json:"behavior,omitempty"`
	// MinAvailable creates a PodDisruptionBudget. Unset disables it.
	// +optional
	MinAvailable *int32 `json:"minAvailable,omitempty"`
}

// PodSpecOverrides are passed through to the pod template.
type PodSpecOverrides struct {
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// +optional
	SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`
	// ExtraVolumes and ExtraVolumeMounts allow e.g. extra tessdata.
	// +optional
	ExtraVolumes []corev1.Volume `json:"extraVolumes,omitempty"`
	// +optional
	ExtraVolumeMounts []corev1.VolumeMount `json:"extraVolumeMounts,omitempty"`
}

// DoclingServerSpec defines the desired state of DoclingServer.
type DoclingServerSpec struct {
	// +optional
	Image ImageSpec `json:"image,omitempty"`
	// +optional
	Accelerator AcceleratorSpec `json:"accelerator,omitempty"`
	// +optional
	Models ModelsSpec `json:"models,omitempty"`
	// +optional
	Server ServerSpec `json:"server,omitempty"`
	// +optional
	Auth AuthSpec `json:"auth,omitempty"`
	// +optional
	Service ServiceSpec `json:"service,omitempty"`
	// +optional
	Scaling ScalingSpec `json:"scaling,omitempty"`
	// +optional
	Pod PodSpecOverrides `json:"pod,omitempty"`
	// Paused stops reconciliation (the operator only reports status).
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// ModelsStatus reports the model lifecycle.
type ModelsStatus struct {
	// +optional
	Source ModelsSource `json:"source,omitempty"`
	// ActiveDir is the directory on the PVC served by the running Deployment.
	// +optional
	ActiveDir string `json:"activeDir,omitempty"`
	// ActiveVersion is the release tag of ActiveDir.
	// +optional
	ActiveVersion string `json:"activeVersion,omitempty"`
	// DesiredDir is the directory derived from the current spec.
	// +optional
	DesiredDir string `json:"desiredDir,omitempty"`
	// PreviousDirs are superseded directories still present on the PVC.
	// +optional
	PreviousDirs []string `json:"previousDirs,omitempty"`
	// DownloadJob is the Job currently (or last) downloading DesiredDir.
	// +optional
	DownloadJob string `json:"downloadJob,omitempty"`
	// RecoveryAttempts counts crash-loop triggered re-downloads for the
	// current spec.
	// +optional
	RecoveryAttempts int32 `json:"recoveryAttempts,omitempty"`
	// LastRecoveryTime is when the last recovery was triggered.
	// +optional
	LastRecoveryTime *metav1.Time `json:"lastRecoveryTime,omitempty"`
	// ClaimName is the PVC holding the models.
	// +optional
	ClaimName string `json:"claimName,omitempty"`
}

// DoclingServerStatus defines the observed state of DoclingServer.
type DoclingServerStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Models ModelsStatus `json:"models,omitempty"`
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Endpoint is the external address of the LoadBalancer, once assigned.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// APIKeySecret names the Secret (and key) clients need for X-Api-Key.
	// +optional
	APIKeySecret *corev1.SecretKeySelector `json:"apiKeySecret,omitempty"`
	// Scaler is the name of the HPA or ScaledObject.
	// +optional
	Scaler string `json:"scaler,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dsrv
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Models",type=string,JSONPath=`.status.conditions[?(@.type=="ModelsReady")].reason`
// +kubebuilder:printcolumn:name="Replicas",type=string,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DoclingServer is the Schema for the doclingservers API. One DoclingServer
// is one HTTP conversion API (docling-rs serve) with its own model set,
// scaling policy and endpoint.
type DoclingServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DoclingServerSpec   `json:"spec,omitempty"`
	Status DoclingServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DoclingServerList contains a list of DoclingServer.
type DoclingServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DoclingServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DoclingServer{}, &DoclingServerList{})
}
