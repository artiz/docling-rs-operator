/*
Copyright (c) 2026 Artem Kustikov

SPDX-License-Identifier: MIT
*/

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	doclingv1alpha1 "github.com/artiz/docling-rs-operator/api/v1alpha1"
)

// modelSetIdentity is everything that changes the content of a model
// directory. Changing any field yields a new directory on the PVC, so a
// switch is always download-then-rollout, never in-place.
type modelSetIdentity struct {
	Version    string   `json:"version"`
	BaseURL    string   `json:"baseURL"`
	ScriptURL  string   `json:"scriptURL"`
	ScriptArgs []string `json:"scriptArgs"`
}

// modelSetHash returns a short stable hash of the desired model set.
func modelSetHash(r *resolved) string {
	id := modelSetIdentity{
		Version:    r.ModelsVersion,
		BaseURL:    r.ReleaseBaseURL,
		ScriptURL:  r.ScriptURL,
		ScriptArgs: append([]string(nil), r.ScriptArgs...),
	}
	sort.Strings(id.ScriptArgs)
	b, _ := json.Marshal(id)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:10]
}

// modelsDirName is the directory (relative to the PVC root) for a model set
// hash and a recovery attempt. Attempt 0 has no suffix.
func modelsDirName(version, hash string, attempt int32) string {
	// Keep the release tag in the name so humans can read the PVC.
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, version)
	name := fmt.Sprintf("%s-%s", safe, hash)
	if attempt > 0 {
		name = fmt.Sprintf("%s-r%d", name, attempt)
	}
	return name
}

// jobNameFor returns a DNS-safe Job name for a directory.
func jobNameFor(ds *doclingv1alpha1.DoclingServer, kind, dir string) string {
	sum := sha256.Sum256([]byte(dir))
	suffix := hex.EncodeToString(sum[:])[:8]
	name := fmt.Sprintf("%s-%s-%s", ds.Name, kind, suffix)
	if len(name) > 63 {
		name = name[len(name)-63:]
		name = strings.TrimLeft(name, "-")
	}
	return name
}

const downloadScript = `set -eu
umask 022
export HOME=/tmp
DIR="$MODELS_ROOT/$MODELS_DIR"
mkdir -p "$DIR"
cd "$DIR"
if [ -f .docling-ready ] && [ "$(cat .docling-ready)" = "$MODELS_DIR" ]; then
  echo "models already present in $DIR"
  exit 0
fi
rm -f .docling-ready
echo "downloading models ($MODELS_VERSION) into $DIR"
curl -fsSL --retry 5 --retry-delay 5 "$SCRIPT_URL" -o /tmp/download_dependencies.sh
# shellcheck disable=SC2086
sh /tmp/download_dependencies.sh $SCRIPT_ARGS
test -f .models/layout_heron.onnx
echo "$MODELS_DIR" > .docling-ready
echo "models ready: $DIR"
`

const cleanupScript = `set -eu
for d in $CLEANUP_DIRS; do
  case "$d" in
    ""|*/*|*..*) echo "refusing to remove '$d'"; exit 1 ;;
  esac
  echo "removing $MODELS_ROOT/$d"
  rm -rf "$MODELS_ROOT/$d"
done
`

// buildDownloadJob creates the Job that fetches and verifies one model set.
func buildDownloadJob(ds *doclingv1alpha1.DoclingServer, r *resolved, dir string) *batchv1.Job {
	d := ds.Spec.Models.Downloader
	backoff := d.BackoffLimit
	if backoff == nil {
		backoff = ptr.To(int32(3))
	}
	deadline := d.ActiveDeadlineSeconds
	if deadline == nil {
		deadline = ptr.To(int64(3600))
	}
	podSC := d.PodSecurityContext
	if podSC == nil {
		podSC = &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0))}
	}
	env := []corev1.EnvVar{
		{Name: "MODELS_ROOT", Value: modelsRootMount},
		{Name: "MODELS_DIR", Value: dir},
		{Name: "MODELS_VERSION", Value: r.ModelsVersion},
		{Name: "SCRIPT_URL", Value: r.ScriptURL},
		{Name: "SCRIPT_ARGS", Value: strings.Join(r.ScriptArgs, " ")},
		{Name: "DOCLING_RS_MODELS_URL", Value: r.ReleaseBaseURL},
		{Name: "DOCLING_RS_MODELS_VERIFY", Value: "require"},
	}
	env = append(env, d.Env...)

	labels := commonLabels(ds, "models")
	labels[labelJobKind] = jobKindDownload
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	// Downloader pods must not match the serve Deployment selector.
	delete(podLabels, labelName)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobNameFor(ds, jobKindDownload, dir),
			Namespace: ds.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				annotationModelsDir: dir,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            backoff,
			ActiveDeadlineSeconds:   deadline,
			TTLSecondsAfterFinished: ptr.To(int32(24 * 3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					NodeSelector:     d.NodeSelector,
					Tolerations:      d.Tolerations,
					SecurityContext:  podSC,
					ImagePullSecrets: ds.Spec.Image.PullSecrets,
					Containers: []corev1.Container{{
						Name:            "download",
						Image:           r.DownloaderImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"sh", "-c", downloadScript},
						Env:             env,
						Resources:       d.Resources,
						SecurityContext: d.SecurityContext,
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "models",
							MountPath: modelsRootMount,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "models",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: r.ClaimName,
							},
						},
					}},
				},
			},
		},
	}
}

// buildCleanupJob creates the Job that deletes superseded directories.
func buildCleanupJob(ds *doclingv1alpha1.DoclingServer, r *resolved, dirs []string) *batchv1.Job {
	sorted := append([]string(nil), dirs...)
	sort.Strings(sorted)
	set := strings.Join(sorted, " ")
	d := ds.Spec.Models.Downloader
	podSC := d.PodSecurityContext
	if podSC == nil {
		podSC = &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0))}
	}
	labels := commonLabels(ds, "models")
	labels[labelJobKind] = jobKindCleanup
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	delete(podLabels, labelName)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobNameFor(ds, jobKindCleanup, set),
			Namespace: ds.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				annotationCleanupSet: set,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(2)),
			ActiveDeadlineSeconds:   ptr.To(int64(1800)),
			TTLSecondsAfterFinished: ptr.To(int32(600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					NodeSelector:     d.NodeSelector,
					Tolerations:      d.Tolerations,
					SecurityContext:  podSC,
					ImagePullSecrets: ds.Spec.Image.PullSecrets,
					Containers: []corev1.Container{{
						Name:            "cleanup",
						Image:           r.DownloaderImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"sh", "-c", cleanupScript},
						Env: []corev1.EnvVar{
							{Name: "MODELS_ROOT", Value: modelsRootMount},
							{Name: "CLEANUP_DIRS", Value: set},
						},
						SecurityContext: d.SecurityContext,
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "models",
							MountPath: modelsRootMount,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "models",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: r.ClaimName,
							},
						},
					}},
				},
			},
		},
	}
}

// jobState summarises a Job.
type jobState int

const (
	jobRunning jobState = iota
	jobSucceeded
	jobFailed
)

func jobStatus(job *batchv1.Job) (jobState, string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return jobSucceeded, c.Message
		case batchv1.JobFailed:
			msg := c.Message
			if msg == "" {
				msg = c.Reason
			}
			return jobFailed, msg
		}
	}
	if job.Status.Succeeded > 0 {
		return jobSucceeded, ""
	}
	return jobRunning, ""
}
