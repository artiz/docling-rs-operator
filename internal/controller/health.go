/*
Copyright (c) 2026 Artem Kustikov

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doclingv1alpha1 "github.com/artiz/docling-rs-operator/api/v1alpha1"
)

// crashLoopRestartThreshold is the restart count after which a pod is
// considered crash-looping even if kubelet has not yet reported
// CrashLoopBackOff (e.g. the first restarts).
const crashLoopRestartThreshold int32 = 5

// podHealth summarises the serve pods of one DoclingServer.
type podHealth struct {
	Total        int
	Ready        int
	CrashLooping int
	// Pods lists the crash-looping pods for status messages.
	Pods []string
}

// inspectPods lists the serve pods and detects crash loops.
func (r *DoclingServerReconciler) inspectPods(ctx context.Context, ds *doclingv1alpha1.DoclingServer) (podHealth, error) {
	var h podHealth
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(ds.Namespace),
		client.MatchingLabels(selectorLabels(ds)),
	); err != nil {
		return h, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		h.Total++
		if isPodReady(p) {
			h.Ready++
		}
		if isCrashLooping(p) {
			h.CrashLooping++
			h.Pods = append(h.Pods, p.Name)
		}
	}
	return h, nil
}

func isPodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func isCrashLooping(p *corev1.Pod) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != "serve" {
			continue
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
		if cs.RestartCount >= crashLoopRestartThreshold && !cs.Ready {
			return true
		}
	}
	return false
}

func (h podHealth) message() string {
	if h.CrashLooping == 0 {
		return fmt.Sprintf("%d/%d pods ready", h.Ready, h.Total)
	}
	names := h.Pods
	if len(names) > 3 {
		names = append(append([]string(nil), names[:3]...), "...")
	}
	return fmt.Sprintf("%d of %d pods crash-looping: %s", h.CrashLooping, h.Total, strings.Join(names, ", "))
}
