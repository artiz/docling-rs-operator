/*
Copyright (c) 2026 Artem Kustikov

SPDX-License-Identifier: MIT
*/

package controller

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doclingv1alpha1 "github.com/artiz/docling-rs-operator/api/v1alpha1"
)

const (
	timeout  = 10 * time.Second
	interval = 100 * time.Millisecond
)

var nsCounter int

func newNamespace() string {
	nsCounter++
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("dsrv-test-%d", nsCounter)}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

func getServer(key types.NamespacedName) *doclingv1alpha1.DoclingServer {
	ds := &doclingv1alpha1.DoclingServer{}
	Expect(k8sClient.Get(ctx, key, ds)).To(Succeed())
	return ds
}

func condition(key types.NamespacedName, t string) *metav1.Condition {
	ds := &doclingv1alpha1.DoclingServer{}
	if err := k8sClient.Get(ctx, key, ds); err != nil {
		return nil
	}
	return meta.FindStatusCondition(ds.Status.Conditions, t)
}

func conditionIs(key types.NamespacedName, t string, status metav1.ConditionStatus, reason string) func() bool {
	return func() bool {
		c := condition(key, t)
		return c != nil && c.Status == status && c.Reason == reason
	}
}

// downloadJobFor waits for the download Job named in status and returns it.
func downloadJobFor(key types.NamespacedName) *batchv1.Job {
	job := &batchv1.Job{}
	Eventually(func() error {
		ds := getServer(key)
		if ds.Status.Models.DownloadJob == "" {
			return fmt.Errorf("no download job in status yet")
		}
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: ds.Status.Models.DownloadJob}, job)
	}, timeout, interval).Should(Succeed())
	return job
}

// finishJob marks a Job Complete or Failed through the status subresource,
// standing in for the Job controller that envtest does not run.
func finishJob(job *batchv1.Job, ok bool) {
	Eventually(func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
			return err
		}
		now := metav1.Now()
		if ok {
			job.Status.Succeeded = 1
			job.Status.StartTime = &now
			job.Status.CompletionTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
			}
		} else {
			job.Status.Failed = 1
			job.Status.StartTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", LastTransitionTime: now},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit", LastTransitionTime: now},
			}
		}
		return k8sClient.Status().Update(ctx, job)
	}, timeout, interval).Should(Succeed())
}

// markRolledOut simulates the Deployment controller finishing a rollout.
func markRolledOut(key types.NamespacedName) {
	Eventually(func() error {
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, key, dep); err != nil {
			return err
		}
		n := *dep.Spec.Replicas
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas = n
		dep.Status.UpdatedReplicas = n
		dep.Status.ReadyReplicas = n
		dep.Status.AvailableReplicas = n
		dep.Status.Conditions = []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable",
		}}
		return k8sClient.Status().Update(ctx, dep)
	}, timeout, interval).Should(Succeed())
}

func modelsDirOf(dep *appsv1.Deployment) string {
	return dep.Spec.Template.Annotations[annotationModelsDir]
}

var _ = Describe("DoclingServer controller", func() {
	var (
		ns  string
		key types.NamespacedName
	)

	BeforeEach(func() {
		ns = newNamespace()
		key = types.NamespacedName{Namespace: ns, Name: "docling"}
	})

	Context("model lifecycle: not ready -> ready -> version switch", func() {
		It("downloads models before creating the Deployment and switches versions safely", func() {
			ds := &doclingv1alpha1.DoclingServer{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
				Spec: doclingv1alpha1.DoclingServerSpec{
					Models: doclingv1alpha1.ModelsSpec{Version: "models-v1"},
					Scaling: doclingv1alpha1.ScalingSpec{
						MinReplicas: 2, MaxReplicas: 4, TargetInFlightPerReplica: 2, MinAvailable: ptr.To(int32(1)),
					},
				},
			}
			Expect(k8sClient.Create(ctx, ds)).To(Succeed())

			By("creating the PVC, the API key Secret and the download Job")
			pvc := &corev1.PersistentVolumeClaim{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "docling-models"}, pvc)
			}, timeout, interval).Should(Succeed())
			Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteMany))
			Expect(metav1.IsControlledBy(pvc, getServer(key))).To(BeTrue())

			secret := &corev1.Secret{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "docling-api-key"}, secret)
			}, timeout, interval).Should(Succeed())
			Expect(secret.Data["api-key"]).To(HaveLen(64))
			firstKey := string(secret.Data["api-key"])

			job := downloadJobFor(key)
			Expect(job.Labels[labelJobKind]).To(Equal(jobKindDownload))
			Expect(metav1.IsControlledBy(job, getServer(key))).To(BeTrue())
			c := job.Spec.Template.Spec.Containers[0]
			Expect(c.Env).To(ContainElement(corev1.EnvVar{Name: "DOCLING_RS_MODELS_VERIFY", Value: "require"}))
			Expect(c.Env).To(ContainElement(corev1.EnvVar{Name: "DOCLING_RS_MODELS_URL", Value: defaultReleaseBase + "models-v1"}))
			firstDir := job.Annotations[annotationModelsDir]
			Expect(firstDir).To(HavePrefix("models-v1-"))

			Eventually(conditionIs(key, doclingv1alpha1.ConditionModelsReady, metav1.ConditionFalse, doclingv1alpha1.ReasonDownloading), timeout, interval).Should(BeTrue())
			Eventually(conditionIs(key, doclingv1alpha1.ConditionReady, metav1.ConditionFalse, doclingv1alpha1.ReasonNotReady), timeout, interval).Should(BeTrue())

			By("not creating the Deployment while models are missing")
			Consistently(func() bool {
				err := k8sClient.Get(ctx, key, &appsv1.Deployment{})
				return client.IgnoreNotFound(err) == nil && err != nil
			}, time.Second, interval).Should(BeTrue())

			By("rolling out once the download Job succeeds")
			finishJob(job, true)
			Eventually(conditionIs(key, doclingv1alpha1.ConditionModelsReady, metav1.ConditionTrue, doclingv1alpha1.ReasonVerified), timeout, interval).Should(BeTrue())

			dep := &appsv1.Deployment{}
			Eventually(func() error { return k8sClient.Get(ctx, key, dep) }, timeout, interval).Should(Succeed())
			Expect(modelsDirOf(dep)).To(Equal(firstDir))
			serve := dep.Spec.Template.Spec.Containers[0]
			Expect(serve.Image).To(Equal(defaultImageCPU + ":1"))
			Expect(serve.Args).To(ContainElements("--addr", "0.0.0.0:5001", "--warmup"))
			Expect(serve.VolumeMounts).To(ContainElement(corev1.VolumeMount{
				Name: "models", MountPath: modelsMountLinux, SubPath: firstDir + "/.models", ReadOnly: true,
			}))
			Expect(serve.Env).To(ContainElement(corev1.EnvVar{Name: "DOCLING_RS_MODELS_DIR", Value: modelsMountLinux}))
			Expect(serve.Env).To(ContainElement(corev1.EnvVar{Name: "DOCLING_RS_EP", Value: "cpu"}))
			Expect(serve.Env).To(ContainElement(corev1.EnvVar{
				Name: apiKeyEnv,
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "docling-api-key"}, Key: "api-key",
				}},
			}))
			Expect(serve.StartupProbe.HTTPGet.Path).To(Equal("/ready"))
			Expect(serve.LivenessProbe.HTTPGet.Path).To(Equal("/health"))

			By("creating the LoadBalancer Service, the HPA and the PDB")
			svc := &corev1.Service{}
			Eventually(func() error { return k8sClient.Get(ctx, key, svc) }, timeout, interval).Should(Succeed())
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(80)))
			Expect(svc.Spec.Selector).To(Equal(selectorLabels(ds)))
			Expect(svc.Spec.SessionAffinity).To(Equal(corev1.ServiceAffinityNone))

			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			Eventually(func() error { return k8sClient.Get(ctx, key, hpa) }, timeout, interval).Should(Succeed())
			Expect(*hpa.Spec.MinReplicas).To(Equal(int32(2)))
			Expect(hpa.Spec.MaxReplicas).To(Equal(int32(4)))
			Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(key.Name))
			var names []string
			for _, m := range hpa.Spec.Metrics {
				if m.Pods != nil {
					names = append(names, m.Pods.Metric.Name)
				}
			}
			Expect(names).To(ConsistOf(metricInFlight))
			Eventually(conditionIs(key, doclingv1alpha1.ConditionScalingReady, metav1.ConditionTrue, doclingv1alpha1.ReasonHPAConfigured), timeout, interval).Should(BeTrue())

			By("becoming Ready when the Deployment is available")
			markRolledOut(key)
			Eventually(conditionIs(key, doclingv1alpha1.ConditionReady, metav1.ConditionTrue, doclingv1alpha1.ReasonHealthy), timeout, interval).Should(BeTrue())
			Expect(getServer(key).Status.Models.ActiveDir).To(Equal(firstDir))
			Expect(getServer(key).Status.APIKeySecret.Name).To(Equal("docling-api-key"))

			By("keeping the generated API key stable across reconciles")
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret)).To(Succeed())
			Expect(string(secret.Data["api-key"])).To(Equal(firstKey))

			By("switching the model version: download first, keep serving the old set")
			Eventually(func() error {
				cur := getServer(key)
				cur.Spec.Models.Version = "models-v2"
				return k8sClient.Update(ctx, cur)
			}, timeout, interval).Should(Succeed())

			var job2 *batchv1.Job
			Eventually(func() bool {
				job2 = downloadJobFor(key)
				return job2.Annotations[annotationModelsDir] != firstDir
			}, timeout, interval).Should(BeTrue())
			secondDir := job2.Annotations[annotationModelsDir]
			Expect(secondDir).To(HavePrefix("models-v2-"))
			Eventually(conditionIs(key, doclingv1alpha1.ConditionModelsReady, metav1.ConditionFalse, doclingv1alpha1.ReasonDownloading), timeout, interval).Should(BeTrue())
			Expect(condition(key, doclingv1alpha1.ConditionModelsReady).Message).To(ContainSubstring("serving " + firstDir))

			Consistently(func() string {
				Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
				return modelsDirOf(dep)
			}, time.Second, interval).Should(Equal(firstDir))

			By("rolling the Deployment onto the new set once downloaded")
			finishJob(job2, true)
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
				return modelsDirOf(dep)
			}, timeout, interval).Should(Equal(secondDir))
			Expect(getServer(key).Status.Models.PreviousDirs).To(ConsistOf(firstDir))

			By("not cleaning up until the rollout finished")
			Consistently(func() int {
				var jobs batchv1.JobList
				Expect(k8sClient.List(ctx, &jobs, client.InNamespace(ns), client.MatchingLabels{labelJobKind: jobKindCleanup})).To(Succeed())
				return len(jobs.Items)
			}, time.Second, interval).Should(Equal(0))

			By("removing the old directory after the rollout")
			markRolledOut(key)
			var cleanup batchv1.Job
			Eventually(func() bool {
				var jobs batchv1.JobList
				Expect(k8sClient.List(ctx, &jobs, client.InNamespace(ns), client.MatchingLabels{labelJobKind: jobKindCleanup})).To(Succeed())
				if len(jobs.Items) != 1 {
					return false
				}
				cleanup = jobs.Items[0]
				return true
			}, timeout, interval).Should(BeTrue())
			Expect(cleanup.Annotations[annotationCleanupSet]).To(Equal(firstDir))
			finishJob(&cleanup, true)
			Eventually(func() []string { return getServer(key).Status.Models.PreviousDirs }, timeout, interval).Should(BeEmpty())
			Eventually(conditionIs(key, doclingv1alpha1.ConditionReady, metav1.ConditionTrue, doclingv1alpha1.ReasonHealthy), timeout, interval).Should(BeTrue())
		})
	})

	Context("download failure", func() {
		It("reports Degraded and keeps the old Deployment", func() {
			ds := &doclingv1alpha1.DoclingServer{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
				Spec:       doclingv1alpha1.DoclingServerSpec{},
			}
			Expect(k8sClient.Create(ctx, ds)).To(Succeed())
			job := downloadJobFor(key)
			finishJob(job, false)
			Eventually(conditionIs(key, doclingv1alpha1.ConditionModelsReady, metav1.ConditionFalse, doclingv1alpha1.ReasonDownloadFailed), timeout, interval).Should(BeTrue())
			Eventually(conditionIs(key, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonDownloadFailed), timeout, interval).Should(BeTrue())
			Consistently(func() bool {
				err := k8sClient.Get(ctx, key, &appsv1.Deployment{})
				return err != nil
			}, time.Second, interval).Should(BeTrue())

			By("retrying when the failed Job is deleted")
			Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
			Eventually(func() bool {
				j := &batchv1.Job{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(job), j); err != nil {
					return false
				}
				return j.UID != job.UID
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("models bundled in the image", func() {
		It("skips the PVC and the Job and runs immediately", func() {
			ds := &doclingv1alpha1.DoclingServer{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
				Spec: doclingv1alpha1.DoclingServerSpec{
					Models:  doclingv1alpha1.ModelsSpec{Source: doclingv1alpha1.ModelsSourceImage},
					Auth:    doclingv1alpha1.AuthSpec{Enabled: ptr.To(false)},
					Service: doclingv1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 8080},
					Scaling: doclingv1alpha1.ScalingSpec{Mode: doclingv1alpha1.ScalingNone, MinReplicas: 3},
				},
			}
			Expect(k8sClient.Create(ctx, ds)).To(Succeed())
			Eventually(conditionIs(key, doclingv1alpha1.ConditionModelsReady, metav1.ConditionTrue, doclingv1alpha1.ReasonBundledInImage), timeout, interval).Should(BeTrue())
			dep := &appsv1.Deployment{}
			Eventually(func() error { return k8sClient.Get(ctx, key, dep) }, timeout, interval).Should(Succeed())
			Expect(dep.Spec.Template.Spec.Volumes).To(BeEmpty())
			Expect(*dep.Spec.Replicas).To(Equal(int32(3)))
			for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
				Expect(e.Name).NotTo(Equal(apiKeyEnv))
			}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "docling-models"}, &corev1.PersistentVolumeClaim{})).NotTo(Succeed())
			Expect(k8sClient.Get(ctx, key, &autoscalingv2.HorizontalPodAutoscaler{})).NotTo(Succeed())
			Eventually(func() string { return getServer(key).Status.Endpoint }, timeout, interval).Should(Equal("http://docling." + ns + ".svc:8080"))
		})
	})

	Context("CUDA accelerator", func() {
		It("uses the CUDA image, requests a GPU and sets DOCLING_RS_EP", func() {
			ds := &doclingv1alpha1.DoclingServer{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
				Spec: doclingv1alpha1.DoclingServerSpec{
					Accelerator: doclingv1alpha1.AcceleratorSpec{Type: doclingv1alpha1.AcceleratorCUDA, Count: 2, RuntimeClassName: ptr.To("nvidia")},
					Image:       doclingv1alpha1.ImageSpec{Tag: "1.28.0"},
					Models:      doclingv1alpha1.ModelsSpec{Source: doclingv1alpha1.ModelsSourceImage},
				},
			}
			Expect(k8sClient.Create(ctx, ds)).To(Succeed())
			dep := &appsv1.Deployment{}
			Eventually(func() error { return k8sClient.Get(ctx, key, dep) }, timeout, interval).Should(Succeed())
			c := dep.Spec.Template.Spec.Containers[0]
			Expect(c.Image).To(Equal(defaultImageCUDA + ":1.28.0"))
			Expect(c.Resources.Limits.Name("nvidia.com/gpu", "").Value()).To(Equal(int64(2)))
			Expect(c.Env).To(ContainElement(corev1.EnvVar{Name: "DOCLING_RS_EP", Value: "cuda"}))
			Expect(*dep.Spec.Template.Spec.RuntimeClassName).To(Equal("nvidia"))
			Expect(dep.Spec.Template.Spec.Tolerations).To(ContainElement(corev1.Toleration{
				Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
			}))
		})
	})

	Context("DirectML accelerator", func() {
		It("rejects a spec without an image and pins pods to Windows otherwise", func() {
			ds := &doclingv1alpha1.DoclingServer{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
				Spec: doclingv1alpha1.DoclingServerSpec{
					Accelerator: doclingv1alpha1.AcceleratorSpec{Type: doclingv1alpha1.AcceleratorDirectML},
					Models:      doclingv1alpha1.ModelsSpec{Source: doclingv1alpha1.ModelsSourceImage},
				},
			}
			Expect(k8sClient.Create(ctx, ds)).To(Succeed())
			Eventually(conditionIs(key, doclingv1alpha1.ConditionReady, metav1.ConditionFalse, doclingv1alpha1.ReasonInvalidSpec), timeout, interval).Should(BeTrue())

			Eventually(func() error {
				cur := getServer(key)
				cur.Spec.Image.Repository = "registry.example.com/docling-serve-dml"
				return k8sClient.Update(ctx, cur)
			}, timeout, interval).Should(Succeed())
			dep := &appsv1.Deployment{}
			Eventually(func() error { return k8sClient.Get(ctx, key, dep) }, timeout, interval).Should(Succeed())
			Expect(dep.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/os", "windows"))
			c := dep.Spec.Template.Spec.Containers[0]
			Expect(c.Resources.Limits.Name("microsoft.com/directx", "").Value()).To(Equal(int64(1)))
			Expect(c.Env).To(ContainElement(corev1.EnvVar{Name: "DOCLING_RS_EP", Value: "auto"}))
		})
	})

	Context("self-healing", func() {
		It("re-downloads models into a fresh directory when serve pods crash-loop, bounded by maxAttempts", func() {
			ds := &doclingv1alpha1.DoclingServer{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
				Spec: doclingv1alpha1.DoclingServerSpec{
					Models: doclingv1alpha1.ModelsSpec{Recovery: doclingv1alpha1.RecoverySpec{MaxAttempts: 1}},
				},
			}
			Expect(k8sClient.Create(ctx, ds)).To(Succeed())
			job := downloadJobFor(key)
			firstDir := job.Annotations[annotationModelsDir]
			finishJob(job, true)
			dep := &appsv1.Deployment{}
			Eventually(func() error { return k8sClient.Get(ctx, key, dep) }, timeout, interval).Should(Succeed())
			markRolledOut(key)

			By("creating a crash-looping serve pod")
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "docling-abc", Namespace: ns, Labels: dep.Spec.Template.Labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "serve", Image: "x"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			Eventually(func() error {
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
					return err
				}
				pod.Status.Phase = corev1.PodRunning
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "serve", RestartCount: 7,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				}}
				return k8sClient.Status().Update(ctx, pod)
			}, timeout, interval).Should(Succeed())

			By("starting a recovery download into a new directory")
			Eventually(conditionIs(key, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonRecovering), timeout, interval).Should(BeTrue())
			var job2 *batchv1.Job
			Eventually(func() bool {
				job2 = downloadJobFor(key)
				return job2.Annotations[annotationModelsDir] == firstDir+"-r1"
			}, timeout, interval).Should(BeTrue())
			Expect(getServer(key).Status.Models.RecoveryAttempts).To(Equal(int32(1)))

			By("rolling onto the recovered directory")
			finishJob(job2, true)
			Eventually(func() string {
				Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
				return modelsDirOf(dep)
			}, timeout, interval).Should(Equal(firstDir + "-r1"))

			By("giving up after maxAttempts while the pod still crash-loops")
			markRolledOut(key)
			Eventually(conditionIs(key, doclingv1alpha1.ConditionDegraded, metav1.ConditionTrue, doclingv1alpha1.ReasonRecoveryExhausted), timeout, interval).Should(BeTrue())
			Expect(getServer(key).Status.Models.RecoveryAttempts).To(Equal(int32(1)))
			Expect(condition(key, doclingv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionFalse))

			By("recovering when the pod becomes healthy")
			Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
			Eventually(conditionIs(key, doclingv1alpha1.ConditionDegraded, metav1.ConditionFalse, doclingv1alpha1.ReasonHealthy), timeout, interval).Should(BeTrue())
			Eventually(conditionIs(key, doclingv1alpha1.ConditionReady, metav1.ConditionTrue, doclingv1alpha1.ReasonHealthy), timeout, interval).Should(BeTrue())
		})
	})

	Context("KEDA mode without KEDA installed", func() {
		It("reports ScalingReady=False/KEDANotInstalled and creates no HPA", func() {
			ds := &doclingv1alpha1.DoclingServer{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
				Spec: doclingv1alpha1.DoclingServerSpec{
					Models: doclingv1alpha1.ModelsSpec{Source: doclingv1alpha1.ModelsSourceImage},
					Scaling: doclingv1alpha1.ScalingSpec{
						Mode: doclingv1alpha1.ScalingKEDA, TargetInFlightPerReplica: 2,
						KEDA: &doclingv1alpha1.KEDASpec{PrometheusAddress: "http://prometheus:9090"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, ds)).To(Succeed())
			Eventually(conditionIs(key, doclingv1alpha1.ConditionScalingReady, metav1.ConditionFalse, doclingv1alpha1.ReasonKEDANotInstalled), timeout, interval).Should(BeTrue())
			Expect(k8sClient.Get(ctx, key, &autoscalingv2.HorizontalPodAutoscaler{})).NotTo(Succeed())
		})
	})

	Context("deletion with an existing claim", func() {
		It("runs a cleanup Job for the model directories before releasing the finalizer", func() {
			ds := &doclingv1alpha1.DoclingServer{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
				Spec: doclingv1alpha1.DoclingServerSpec{
					Models: doclingv1alpha1.ModelsSpec{Storage: doclingv1alpha1.ModelStorageSpec{ExistingClaim: "shared-models"}},
				},
			}
			Expect(k8sClient.Create(ctx, ds)).To(Succeed())
			job := downloadJobFor(key)
			dir := job.Annotations[annotationModelsDir]
			Expect(job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("shared-models"))
			finishJob(job, true)
			Eventually(func() string { return getServer(key).Status.Models.ActiveDir }, timeout, interval).Should(Equal(dir))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "docling-models"}, &corev1.PersistentVolumeClaim{})).NotTo(Succeed())

			Expect(k8sClient.Delete(ctx, getServer(key))).To(Succeed())
			var cleanup batchv1.Job
			Eventually(func() bool {
				var jobs batchv1.JobList
				Expect(k8sClient.List(ctx, &jobs, client.InNamespace(ns), client.MatchingLabels{labelJobKind: jobKindCleanup})).To(Succeed())
				if len(jobs.Items) != 1 {
					return false
				}
				cleanup = jobs.Items[0]
				return true
			}, timeout, interval).Should(BeTrue())
			Expect(cleanup.Annotations[annotationCleanupSet]).To(Equal(dir))
			Consistently(func() error { return k8sClient.Get(ctx, key, &doclingv1alpha1.DoclingServer{}) }, time.Second, interval).Should(Succeed())

			finishJob(&cleanup, true)
			Eventually(func() bool {
				err := k8sClient.Get(ctx, key, &doclingv1alpha1.DoclingServer{})
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("idempotency", func() {
		It("does not touch owned objects on a no-op reconcile and respects autoscaler-owned replicas", func() {
			ds := &doclingv1alpha1.DoclingServer{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
				Spec: doclingv1alpha1.DoclingServerSpec{
					Models:  doclingv1alpha1.ModelsSpec{Source: doclingv1alpha1.ModelsSourceImage},
					Scaling: doclingv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 5},
				},
			}
			Expect(k8sClient.Create(ctx, ds)).To(Succeed())
			dep := &appsv1.Deployment{}
			Eventually(func() error { return k8sClient.Get(ctx, key, dep) }, timeout, interval).Should(Succeed())
			markRolledOut(key)
			Eventually(conditionIs(key, doclingv1alpha1.ConditionReady, metav1.ConditionTrue, doclingv1alpha1.ReasonHealthy), timeout, interval).Should(BeTrue())

			By("simulating the HPA scaling the Deployment")
			Eventually(func() error {
				if err := k8sClient.Get(ctx, key, dep); err != nil {
					return err
				}
				dep.Spec.Replicas = ptr.To(int32(4))
				return k8sClient.Update(ctx, dep)
			}, timeout, interval).Should(Succeed())
			rv := dep.ResourceVersion
			gen := dep.Generation

			By("poking the DoclingServer with an annotation")
			Eventually(func() error {
				cur := getServer(key)
				if cur.Annotations == nil {
					cur.Annotations = map[string]string{}
				}
				cur.Annotations["test/poke"] = "1"
				return k8sClient.Update(ctx, cur)
			}, timeout, interval).Should(Succeed())
			Eventually(func() int64 { return getServer(key).Status.ObservedGeneration }, timeout, interval).Should(Equal(getServer(key).Generation))

			Consistently(func() string {
				Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
				return dep.ResourceVersion
			}, 2*time.Second, interval).Should(Equal(rv))
			Expect(dep.Generation).To(Equal(gen))
			Expect(*dep.Spec.Replicas).To(Equal(int32(4)))
		})
	})
})
