/*
Copyright (c) 2026 Artem Kustikov

SPDX-License-Identifier: MIT
*/

package controller

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	doclingv1alpha1 "github.com/artiz/docling-rs-operator/api/v1alpha1"
)

// generateAPIKey returns 32 random bytes as hex.
func generateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// buildPVC returns the shared models claim. PVC spec is immutable, so the
// object is only ever created, never updated.
func buildPVC(ds *doclingv1alpha1.DoclingServer, r *resolved) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.ClaimName,
			Namespace: ds.Namespace,
			Labels:    commonLabels(ds, "models"),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      r.AccessModes,
			StorageClassName: ds.Spec.Models.Storage.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: r.StorageSize},
			},
		},
	}
}

// podTemplate builds the serve pod template. modelsDir is empty when the
// models are bundled in the image.
func podTemplate(ds *doclingv1alpha1.DoclingServer, r *resolved, modelsDir string) corev1.PodTemplateSpec {
	s := ds.Spec
	labels := commonLabels(ds, "server")
	for k, v := range s.Pod.Labels {
		if _, reserved := labels[k]; !reserved {
			labels[k] = v
		}
	}
	annotations := map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   fmt.Sprint(r.Port),
		"prometheus.io/path":   "/metrics",
		annotationModelsDir:    modelsDir,
	}
	for k, v := range s.Pod.Annotations {
		annotations[k] = v
	}

	env := []corev1.EnvVar{
		{Name: "RUST_LOG", Value: r.LogLevel},
		{Name: "DOCLING_RS_EP", Value: r.ExecutionProvider},
	}
	if modelsDir != "" {
		env = append(env, corev1.EnvVar{Name: "DOCLING_RS_MODELS_DIR", Value: r.ModelsMount})
	}
	if r.AuthEnabled && r.APIKeyRef != nil {
		env = append(env, corev1.EnvVar{
			Name:      apiKeyEnv,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: r.APIKeyRef},
		})
	}
	env = append(env, s.Server.Env...)

	resources := *s.Server.Resources.DeepCopy()
	if r.GPUResource != "" {
		if resources.Limits == nil {
			resources.Limits = corev1.ResourceList{}
		}
		if resources.Requests == nil {
			resources.Requests = corev1.ResourceList{}
		}
		q := resource.NewQuantity(r.GPUCount, resource.DecimalSI)
		resources.Limits[r.GPUResource] = *q
		resources.Requests[r.GPUResource] = *q
	}

	probe := func(path string, period, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromString("http")},
			},
			PeriodSeconds:    period,
			TimeoutSeconds:   5,
			FailureThreshold: failures,
		}
	}

	container := corev1.Container{
		Name:            "serve",
		Image:           r.Image,
		ImagePullPolicy: r.PullPolicy,
		Command:         r.Command,
		Args:            r.Args,
		Env:             env,
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: r.Port,
			Protocol:      corev1.ProtocolTCP,
		}},
		Resources:      resources,
		StartupProbe:   probe("/ready", 5, r.StartupProbeFailures),
		ReadinessProbe: probe("/ready", 10, 3),
		LivenessProbe:  probe("/health", 20, 3),
		VolumeMounts:   append([]corev1.VolumeMount(nil), s.Pod.ExtraVolumeMounts...),
	}

	volumes := append([]corev1.Volume(nil), s.Pod.ExtraVolumes...)
	if modelsDir != "" {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "models",
			MountPath: r.ModelsMount,
			SubPath:   modelsDir + "/.models",
			ReadOnly:  true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "models",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: r.ClaimName,
					ReadOnly:  true,
				},
			},
		})
	}

	nodeSelector := map[string]string{}
	for k, v := range s.Pod.NodeSelector {
		nodeSelector[k] = v
	}
	tolerations := append([]corev1.Toleration(nil), s.Pod.Tolerations...)
	switch r.Accelerator {
	case doclingv1alpha1.AcceleratorCUDA:
		if len(s.Pod.Tolerations) == 0 {
			tolerations = append(tolerations, corev1.Toleration{
				Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
			})
		}
	case doclingv1alpha1.AcceleratorDirectML:
		if _, ok := nodeSelector["kubernetes.io/os"]; !ok {
			nodeSelector["kubernetes.io/os"] = "windows"
		}
		if len(s.Pod.Tolerations) == 0 {
			tolerations = append(tolerations, corev1.Toleration{
				Key: "os", Operator: corev1.TolerationOpEqual, Value: "windows", Effect: corev1.TaintEffectNoSchedule,
			})
		}
	}
	if len(nodeSelector) == 0 {
		nodeSelector = nil
	}

	grace := s.Pod.TerminationGracePeriodSeconds
	if grace == nil {
		// Let in-flight conversions finish.
		grace = ptr.To(int64(120))
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
		Spec: corev1.PodSpec{
			Containers:                    []corev1.Container{container},
			Volumes:                       volumes,
			ImagePullSecrets:              s.Image.PullSecrets,
			NodeSelector:                  nodeSelector,
			Tolerations:                   tolerations,
			Affinity:                      s.Pod.Affinity,
			TopologySpreadConstraints:     s.Pod.TopologySpreadConstraints,
			PriorityClassName:             s.Pod.PriorityClassName,
			ServiceAccountName:            s.Pod.ServiceAccountName,
			SecurityContext:               s.Pod.SecurityContext,
			RuntimeClassName:              s.Accelerator.RuntimeClassName,
			TerminationGracePeriodSeconds: grace,
		},
	}
}

// buildDeployment returns the desired Deployment. It is applied with
// server-side apply, so only the fields set here are owned by the operator:
// in hpa/keda mode spec.replicas is left to the autoscaler.
func buildDeployment(ds *doclingv1alpha1.DoclingServer, r *resolved, modelsDir string) *appsv1.Deployment {
	maxSurge := intstr.FromInt32(1)
	maxUnavailable := intstr.FromInt32(0)
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.Name,
			Namespace: ds.Namespace,
			Labels:    commonLabels(ds, "server"),
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(ds)},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &maxSurge,
					MaxUnavailable: &maxUnavailable,
				},
			},
			RevisionHistoryLimit:    ptr.To(int32(3)),
			ProgressDeadlineSeconds: ptr.To(int32(1800)),
			Template:                podTemplate(ds, r, modelsDir),
		},
	}
	if r.ScalingMode == doclingv1alpha1.ScalingNone {
		dep.Spec.Replicas = ptr.To(r.MinReplicas)
	}
	return dep
}

// buildService returns the desired Service.
func buildService(ds *doclingv1alpha1.DoclingServer, r *resolved) *corev1.Service {
	s := ds.Spec.Service
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        ds.Name,
			Namespace:   ds.Namespace,
			Labels:      commonLabels(ds, "server"),
			Annotations: s.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     r.ServiceType,
			Selector: selectorLabels(ds),
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       r.ServicePort,
				TargetPort: intstr.FromString("http"),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	if r.ServiceType == corev1.ServiceTypeLoadBalancer {
		svc.Spec.LoadBalancerClass = s.LoadBalancerClass
		svc.Spec.LoadBalancerSourceRanges = s.LoadBalancerSourceRanges
		if s.ExternalTrafficPolicy != "" {
			svc.Spec.ExternalTrafficPolicy = s.ExternalTrafficPolicy
		}
	}
	affinity := s.SessionAffinity
	if affinity == "" {
		affinity = corev1.ServiceAffinityClientIP
	}
	svc.Spec.SessionAffinity = affinity
	return svc
}

// buildPDB returns the desired PodDisruptionBudget.
func buildPDB(ds *doclingv1alpha1.DoclingServer, minAvailable int32) *policyv1.PodDisruptionBudget {
	m := intstr.FromInt32(minAvailable)
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.Name,
			Namespace: ds.Namespace,
			Labels:    commonLabels(ds, "server"),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &m,
			Selector:     &metav1.LabelSelector{MatchLabels: selectorLabels(ds)},
		},
	}
}

// deploymentRolledOut reports whether all replicas run the latest template.
func deploymentRolledOut(dep *appsv1.Deployment) bool {
	if dep.Generation > dep.Status.ObservedGeneration {
		return false
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	return dep.Status.UpdatedReplicas == want &&
		dep.Status.Replicas == want &&
		dep.Status.AvailableReplicas == want
}

// deploymentAvailable mirrors the Deployment's Available condition.
func deploymentAvailable(dep *appsv1.Deployment) (bool, string) {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue, c.Message
		}
	}
	return false, "Deployment has no Available condition yet"
}
