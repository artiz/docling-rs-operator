/*
Copyright (c) 2026 Artem Kustikov

SPDX-License-Identifier: MIT
*/

package controller

import (
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	doclingv1alpha1 "github.com/artiz/docling-rs-operator/api/v1alpha1"
)

// kedaScaledObjectGVK is the KEDA ScaledObject kind, handled as unstructured
// so the operator does not depend on KEDA being installed.
var kedaScaledObjectGVK = schema.GroupVersionKind{
	Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject",
}

// buildHPA builds the HorizontalPodAutoscaler for hpa mode.
func buildHPA(ds *doclingv1alpha1.DoclingServer, r *resolved, deploymentName string) *autoscalingv2.HorizontalPodAutoscaler {
	s := ds.Spec.Scaling
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		TypeMeta: metav1.TypeMeta{APIVersion: "autoscaling/v2", Kind: "HorizontalPodAutoscaler"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ds.Name,
			Namespace: ds.Namespace,
			Labels:    commonLabels(ds, "server"),
		},
	}
	hpa.Spec.ScaleTargetRef = autoscalingv2.CrossVersionObjectReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deploymentName,
	}
	hpa.Spec.MinReplicas = ptr.To(r.MinReplicas)
	hpa.Spec.MaxReplicas = r.MaxReplicas

	var metrics []autoscalingv2.MetricSpec
	cpu := s.TargetCPUUtilization
	if s.TargetCPUUtilization == 0 && s.TargetInFlightPerReplica == 0 {
		// Nothing configured: CPU 70% is the sane default.
		cpu = 70
	}
	if cpu > 0 {
		metrics = append(metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: "cpu",
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: ptr.To(cpu),
				},
			},
		})
	}
	if s.TargetInFlightPerReplica > 0 {
		q := resource.NewQuantity(int64(s.TargetInFlightPerReplica), resource.DecimalSI)
		metrics = append(metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.PodsMetricSourceType,
			Pods: &autoscalingv2.PodsMetricSource{
				Metric: autoscalingv2.MetricIdentifier{Name: metricInFlight},
				Target: autoscalingv2.MetricTarget{
					Type:         autoscalingv2.AverageValueMetricType,
					AverageValue: q,
				},
			},
		})
	}
	hpa.Spec.Metrics = metrics
	hpa.Spec.Behavior = s.Behavior
	if hpa.Spec.Behavior == nil {
		// Conversions are bursty: scale up fast, scale down slowly so a queue
		// is not re-created by flapping.
		hpa.Spec.Behavior = &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To(int32(300)),
			},
		}
	}
	return hpa
}

// kedaQuery returns the PromQL used by the KEDA trigger.
func kedaQuery(ds *doclingv1alpha1.DoclingServer) string {
	if ds.Spec.Scaling.KEDA != nil && ds.Spec.Scaling.KEDA.Query != "" {
		return ds.Spec.Scaling.KEDA.Query
	}
	return fmt.Sprintf(`sum(%s{namespace="%s",pod=~"%s-.*"})`, metricInFlight, ds.Namespace, ds.Name)
}

// buildScaledObject builds the KEDA ScaledObject for keda mode.
func buildScaledObject(ds *doclingv1alpha1.DoclingServer, r *resolved, deploymentName string) *unstructured.Unstructured {
	s := ds.Spec.Scaling
	k := s.KEDA
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(kedaScaledObjectGVK)
	so.SetName(ds.Name)
	so.SetNamespace(ds.Namespace)
	so.SetLabels(commonLabels(ds, "server"))

	polling := int64(int32Default(k.PollingInterval, 15))
	cooldown := int64(int32Default(k.CooldownPeriod, 120))

	var triggers []interface{}
	if s.TargetInFlightPerReplica > 0 {
		triggers = append(triggers, map[string]interface{}{
			"type": "prometheus",
			"metadata": map[string]interface{}{
				"serverAddress": k.PrometheusAddress,
				"query":         kedaQuery(ds),
				"threshold":     fmt.Sprint(s.TargetInFlightPerReplica),
			},
		})
	}
	if s.TargetCPUUtilization > 0 {
		triggers = append(triggers, map[string]interface{}{
			"type":       "cpu",
			"metricType": "Utilization",
			"metadata": map[string]interface{}{
				"value": fmt.Sprint(s.TargetCPUUtilization),
			},
		})
	}
	spec := map[string]interface{}{
		"scaleTargetRef": map[string]interface{}{
			"name": deploymentName,
		},
		"minReplicaCount": int64(r.MinReplicas),
		"maxReplicaCount": int64(r.MaxReplicas),
		"pollingInterval": polling,
		"cooldownPeriod":  cooldown,
		"triggers":        triggers,
	}
	if s.Behavior != nil {
		behavior, err := toUnstructured(s.Behavior)
		if err == nil {
			spec["advanced"] = map[string]interface{}{
				"horizontalPodAutoscalerConfig": map[string]interface{}{
					"behavior": behavior,
				},
			}
		}
	}
	so.Object["spec"] = spec
	return so
}
