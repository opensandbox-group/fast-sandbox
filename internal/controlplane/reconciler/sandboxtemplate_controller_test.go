package reconciler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifactstore"
)

func newSandboxTemplate(namespace, name string) *apiv1alpha2.SandboxTemplate {
	return &apiv1alpha2.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "template-uid", Generation: 1},
		Spec: apiv1alpha2.SandboxTemplateSpec{
			Image:  "registry.example.com/sandbox:v1.0.0",
			Kernel: "vmlinux.bin",
			Machine: apiv1alpha2.MachineSpec{
				VCPU:   "4",
				Memory: "8Gi",
			},
			Readiness: apiv1alpha2.ReadinessSpec{WarmupSeconds: 60},
			Output: apiv1alpha2.OutputSpec{
				RootfsSize: "30Gi",
				Format:     apiv1alpha2.ArtifactFormatOverlayBD,
				Publish:    "s3://bucket/sandbox-images/",
			},
		},
	}
}

func newPublishSecret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			"accessKeyId":     []byte("AKID"),
			"secretAccessKey": []byte("SKEY"),
			"endpoint":        []byte("https://s3.example.com"),
			"region":          []byte("cn-hangzhou"),
		},
	}
}

func newSandboxTemplateReconciler(t *testing.T, objects ...client.Object) *SandboxTemplateReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := apiv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha2 scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add rbac scheme: %v", err)
	}
	return &SandboxTemplateReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&apiv1alpha2.SandboxTemplate{}).
			WithObjects(objects...).Build(),
		Scheme:       scheme,
		BuilderImage: "fast-sandbox/sandboxtemplate-builder:dev",
		BuildTTL:     time.Hour,
	}
}

func reconcileOnce(t *testing.T, r *SandboxTemplateReconciler, key client.ObjectKey) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v (%T) isNotFound=%v", err, err, apierrors.IsNotFound(err))
	}
	return result
}

func listBuilderPods(t *testing.T, r *SandboxTemplateReconciler, namespace string) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := r.List(context.Background(), &pods, client.InNamespace(namespace)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	return pods.Items
}

// builderPodOwnerRefs returns the controller owner reference the reconcile
// loop stamps on build Pods (same shape as ctrl.SetControllerReference).
func builderPodOwnerRefs(template *apiv1alpha2.SandboxTemplate) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion: apiv1alpha2.GroupVersion.String(),
		Kind:       "SandboxTemplate",
		Name:       template.Name,
		UID:        template.UID,
		Controller: ptr(true),
	}}
}

func TestSandboxTemplateReconcileCreatesPodAndTracksPhase(t *testing.T) {
	namespace, name := "tenant-a", "golden"
	template := newSandboxTemplate(namespace, name)
	template.Spec.Output.PublishSecretRef = &corev1.LocalObjectReference{Name: "publish-creds"}
	secret := newPublishSecret(namespace, "publish-creds")
	reconciler := newSandboxTemplateReconciler(t, template, secret)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	// First reconcile: Pending → Building, and a build Pod is created in
	// the template's namespace with the publish credentials injected.
	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseBuilding {
		t.Fatalf("expected Building phase, got %s", updated.Status.Phase)
	}
	pods := listBuilderPods(t, reconciler, namespace)
	if len(pods) != 1 {
		t.Fatalf("expected one build pod in %s, got %d", namespace, len(pods))
	}
	pod := &pods[0]
	if pod.Namespace != namespace {
		t.Fatalf("expected the build pod in the template namespace, got %q", pod.Namespace)
	}
	if pod.Name != buildPodName(&updated) {
		t.Fatalf("expected deterministic pod name, got %q", pod.Name)
	}
	if pod.Labels[sandboxTemplateBuildLabel] != name {
		t.Fatalf("build pod missing template label")
	}
	if pod.Labels[sandboxTemplateNamespaceLabel] != namespace {
		t.Fatalf("build pod missing template namespace label")
	}
	if pod.Labels[sandboxTemplateGenerationLabel] != "1" {
		t.Fatalf("build pod missing generation label, got %q", pod.Labels[sandboxTemplateGenerationLabel])
	}
	// The Pod carries a controller owner reference to the template: deleting
	// the template cascades to it via the garbage collector.
	if len(pod.OwnerReferences) != 1 ||
		pod.OwnerReferences[0].UID != updated.UID ||
		pod.OwnerReferences[0].Controller == nil || !*pod.OwnerReferences[0].Controller {
		t.Fatalf("expected the build pod to be owned by the template, got %+v", pod.OwnerReferences)
	}
	// The builder RBAC is provisioned in the template's namespace.
	sa := &corev1.ServiceAccount{}
	if err := reconciler.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: sandboxTemplateBuilderServiceAccount}, sa); err != nil {
		t.Fatalf("expected builder serviceaccount in %s: %v", namespace, err)
	}
	binding := &rbacv1.RoleBinding{}
	if err := reconciler.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: sandboxTemplateBuilderServiceAccount}, binding); err != nil {
		t.Fatalf("expected builder rolebinding in %s: %v", namespace, err)
	}
	if binding.RoleRef.Name != sandboxTemplateBuilderServiceAccount || len(binding.Subjects) != 1 {
		t.Fatalf("unexpected builder rolebinding: %+v", binding)
	}

	// Protocol contract: the serialized SANDBOX_TEMPLATE_SPEC round-trips to
	// exactly the template's spec — the builder parses it as
	// SandboxTemplateSpec (not the wrapping CR).
	specJSON := ""
	for _, entry := range pod.Spec.Containers[0].Env {
		if entry.Name == builderImageEnv {
			specJSON = entry.Value
		}
	}
	if specJSON == "" {
		t.Fatalf("build pod missing %s env", builderImageEnv)
	}
	var specInPod apiv1alpha2.SandboxTemplateSpec
	if err := json.Unmarshal([]byte(specJSON), &specInPod); err != nil {
		t.Fatalf("SANDBOX_TEMPLATE_SPEC is not valid SandboxTemplateSpec JSON: %v", err)
	}
	if !reflect.DeepEqual(specInPod, template.Spec) {
		t.Fatalf("spec JSON round-trip mismatch:\n got %+v\nwant %+v", specInPod, template.Spec)
	}
	// The publish credentials are MOUNTED, not env-injected: the secret
	// volume carries the reference, and the builder is pointed at the mount.
	foundCredentialsVolume := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != "publish-credentials" {
			continue
		}
		if volume.Secret == nil || volume.Secret.SecretName != "publish-creds" {
			t.Fatalf("expected a publish-credentials volume for secret publish-creds, got %+v", volume.VolumeSource)
		}
		foundCredentialsVolume = true
	}
	if !foundCredentialsVolume {
		t.Fatalf("expected a publish-credentials volume, got %+v", pod.Spec.Volumes)
	}
	foundCredentialsMount := false
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.Name == "publish-credentials" && mount.MountPath == sandboxTemplatePublishSecretDir && mount.ReadOnly {
			foundCredentialsMount = true
		}
	}
	if !foundCredentialsMount {
		t.Fatalf("expected a read-only publish-credentials mount at %s, got %+v", sandboxTemplatePublishSecretDir, pod.Spec.Containers[0].VolumeMounts)
	}
	credentialsDirEnv := ""
	for _, entry := range pod.Spec.Containers[0].Env {
		if entry.ValueFrom != nil && entry.ValueFrom.SecretKeyRef != nil {
			t.Fatalf("secret %s/%s must not be env-injected", entry.ValueFrom.SecretKeyRef.Name, entry.ValueFrom.SecretKeyRef.Key)
		}
		if entry.Name == publishCredentialsDirEnv {
			credentialsDirEnv = entry.Value
		}
	}
	if credentialsDirEnv != sandboxTemplatePublishSecretDir {
		t.Fatalf("expected %s=%s, got %q", publishCredentialsDirEnv, sandboxTemplatePublishSecretDir, credentialsDirEnv)
	}

	// A duplicate reconcile must be a no-op (deterministic name + AlreadyExists).
	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler, namespace); len(pods) != 1 {
		t.Fatalf("expected exactly one build pod after duplicate reconcile, got %d", len(pods))
	}

	// Pod succeeds with reported annotations → phase becomes Succeeded and
	// manifestRef/artifactDigest are consumed before cleanup.
	pod.Annotations = map[string]string{
		sandboxTemplateManifestRefAnnot:  "s3://bucket/sandbox-images/abc123/manifest.json",
		sandboxTemplateArtifactDigestAnn: "deadbeef",
	}
	if err := reconciler.Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod metadata: %v", err)
	}
	pod.Status.Phase = corev1.PodSucceeded
	if err := reconciler.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	reconcileOnce(t, reconciler, key)
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseSucceeded {
		t.Fatalf("expected Succeeded phase, got %s", updated.Status.Phase)
	}
	if updated.Status.LastBuildTime == nil {
		t.Fatalf("expected LastBuildTime to be set")
	}
	if updated.Status.ManifestRef != "s3://bucket/sandbox-images/abc123/manifest.json" {
		t.Fatalf("expected ManifestRef to be consumed, got %q", updated.Status.ManifestRef)
	}
	if updated.Status.ArtifactDigest != "deadbeef" {
		t.Fatalf("expected ArtifactDigest to be consumed, got %q", updated.Status.ArtifactDigest)
	}
	foundSucceeded := false
	for _, condition := range updated.Status.Conditions {
		if condition.Type == apiv1alpha2.SandboxTemplateConditionBuildSucceeded && condition.Status == "True" {
			foundSucceeded = true
			if condition.LastTransitionTime == nil {
				t.Fatalf("expected LastTransitionTime on the success condition")
			}
		}
	}
	if !foundSucceeded {
		t.Fatalf("expected a True BuildSucceeded condition on success")
	}

	// Terminal builds are no-ops, and the finished Pod is cleaned up.
	result := reconcileOnce(t, reconciler, key)
	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for terminal build, got %v", result.RequeueAfter)
	}
	if pods := listBuilderPods(t, reconciler, namespace); len(pods) != 0 {
		t.Fatalf("expected the finished pod to be cleaned up, got %d", len(pods))
	}
}

func TestSandboxTemplateReconcileFailedPod(t *testing.T) {
	namespace, name := "tenant-a", "broken"
	template := newSandboxTemplate(namespace, name)
	reconciler := newSandboxTemplateReconciler(t, template)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler, namespace)
	pod := &pods[0]
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "build",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Message: "boom",
		}},
	}}
	if err := reconciler.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseFailed {
		t.Fatalf("expected Failed phase, got %s", updated.Status.Phase)
	}
	found := false
	for _, condition := range updated.Status.Conditions {
		if condition.Type == apiv1alpha2.SandboxTemplateConditionBuildSucceeded && condition.Status == "False" {
			found = true
			if condition.Message != "build pod failed (exit code 1): boom" {
				t.Fatalf("expected failure message with exit code, got %q", condition.Message)
			}
		}
	}
	if !found {
		t.Fatalf("expected a Failed BuildSucceeded condition")
	}
}

func TestSandboxTemplateReconcileIgnoresStaleGenerationPod(t *testing.T) {
	namespace, name := "tenant-a", "stale"
	template := newSandboxTemplate(namespace, name)
	reconciler := newSandboxTemplateReconciler(t, template)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	// First reconcile creates a Pod for generation 1.
	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler, namespace)
	if len(pods) != 1 {
		t.Fatalf("expected one build pod, got %d", len(pods))
	}

	// Spec changes: generation 2, status reset. The running Pod for
	// generation 1 is superseded and deleted immediately; a fresh Pod for
	// generation 2 is created instead (no concurrent stale builders).
	if err := reconciler.Get(context.Background(), key, template); err != nil {
		t.Fatalf("get template: %v", err)
	}
	template.Generation = 2
	if err := reconciler.Update(context.Background(), template); err != nil {
		t.Fatalf("update template: %v", err)
	}
	reconcileOnce(t, reconciler, key)
	pods = listBuilderPods(t, reconciler, namespace)
	if len(pods) != 1 {
		t.Fatalf("expected the stale pod to be deleted and one fresh pod created, got %d", len(pods))
	}
	if pods[0].Labels[sandboxTemplateGenerationLabel] != "2" {
		t.Fatalf("expected a generation-2 build pod, got %q", pods[0].Labels[sandboxTemplateGenerationLabel])
	}
	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseBuilding {
		t.Fatalf("expected Building phase after generation bump, got %s", updated.Status.Phase)
	}
}

func TestSandboxTemplateReconcileBuildTTLRetainsPod(t *testing.T) {
	namespace, name := "tenant-a", "ttl"
	template := newSandboxTemplate(namespace, name)
	reconciler := newSandboxTemplateReconciler(t, template)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler, namespace)
	pod := &pods[0]
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodReady,
		Status:             corev1.ConditionFalse,
		Reason:             "PodCompleted",
		LastTransitionTime: metav1.Now(),
	}}
	if err := reconciler.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Within BuildTTL the finished Pod is retained (annotations still
	// inspectable); beyond TTL it is cleaned up.
	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler, namespace); len(pods) != 1 {
		t.Fatalf("expected the finished pod to be retained within BuildTTL, got %d", len(pods))
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodReady,
		Status:             corev1.ConditionFalse,
		Reason:             "PodCompleted",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
	}}
	if err := reconciler.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler, namespace); len(pods) != 0 {
		t.Fatalf("expected the finished pod to be cleaned up after BuildTTL, got %d", len(pods))
	}
}

func TestSandboxTemplateReconcileFailedPodRetainedForTTL(t *testing.T) {
	namespace, name := "tenant-a", "failed-ttl"
	template := newSandboxTemplate(namespace, name)
	reconciler := newSandboxTemplateReconciler(t, template)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler, namespace)
	pod := &pods[0]
	// Real kubelet failure shape: failed pods never carry the PodCompleted
	// Ready condition (the kubelet marks it PodFailed), so the completion
	// time must come from the container's Terminated state — without that
	// fallback the failed Pod would be deleted immediately and its builder
	// logs would be unrecoverable.
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "build",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Message: "boom", FinishedAt: metav1.Now(),
		}},
	}}
	if err := reconciler.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler, namespace); len(pods) != 1 {
		t.Fatalf("expected the failed pod to be retained within BuildTTL, got %d", len(pods))
	}
	// The terminal reconcile requeues for the retention expiry so a quiet
	// cluster still reaps the retained pod.
	if result := reconcileOnce(t, reconciler, key); result.RequeueAfter <= 0 {
		t.Fatalf("expected a requeue for BuildTTL expiry, got %v", result.RequeueAfter)
	}

	pods = listBuilderPods(t, reconciler, namespace)
	pod = &pods[0]
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "build",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, FinishedAt: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
		}},
	}}
	if err := reconciler.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler, namespace); len(pods) != 0 {
		t.Fatalf("expected the failed pod to be cleaned up after BuildTTL, got %d", len(pods))
	}
}

func TestSandboxTemplateBuildPodOwnedByTemplate(t *testing.T) {
	namespace, name := "tenant-a", "gone"
	template := newSandboxTemplate(namespace, name)
	reconciler := newSandboxTemplateReconciler(t, template)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler, namespace)
	if len(pods) != 1 {
		t.Fatalf("expected one build pod, got %d", len(pods))
	}
	// The build Pod is owned by the template, so deleting the template
	// cascades to it via the garbage collector — no finalizer needed.
	foundOwner := false
	for _, ref := range pods[0].OwnerReferences {
		if ref.APIVersion == "sandbox.fast.io/v1alpha2" && ref.Kind == "SandboxTemplate" &&
			ref.UID == template.UID && ref.Controller != nil && *ref.Controller {
			foundOwner = true
		}
	}
	if !foundOwner {
		t.Fatalf("expected a controller owner reference to the template, got %+v", pods[0].OwnerReferences)
	}
	// Without a finalizer the template disappears immediately on delete.
	if err := reconciler.Delete(context.Background(), template); err != nil {
		t.Fatalf("delete template: %v", err)
	}
	reconcileOnce(t, reconciler, key)
	if err := reconciler.Get(context.Background(), key, &apiv1alpha2.SandboxTemplate{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected the template to be gone without a finalizer, got %v", err)
	}
}

func TestSandboxTemplateReconcileFailsStuckPendingPod(t *testing.T) {
	namespace, name := "tenant-a", "stuck"
	template := newSandboxTemplate(namespace, name)
	// Preset a build Pod that never got scheduled: old and still Pending.
	stale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildPodName(template),
			Namespace: namespace,
			Labels: map[string]string{
				sandboxTemplateBuildLabel:      name,
				sandboxTemplateNamespaceLabel:  namespace,
				sandboxTemplateGenerationLabel: "1",
			},
			OwnerReferences:   builderPodOwnerRefs(template),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-podPendingTimeout - time.Minute)),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "build"}}},
	}
	stale.Status.Phase = corev1.PodPending
	reconciler := newSandboxTemplateReconciler(t, template, stale)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseFailed {
		t.Fatalf("expected Failed phase for a stuck Pending pod, got %s", updated.Status.Phase)
	}
	found := false
	for _, condition := range updated.Status.Conditions {
		if condition.Type == apiv1alpha2.SandboxTemplateConditionBuildSucceeded &&
			condition.Status == "False" && condition.Reason == "PodPendingTimeout" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a PodPendingTimeout failure condition")
	}
}

func TestSandboxTemplateReconcileSelfHealsPendingPhase(t *testing.T) {
	namespace, name := "tenant-a", "recover"
	template := newSandboxTemplate(namespace, name)
	// Pod exists and runs, but the template status was never flipped to
	// Building (e.g. controller restarted between create and status update).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildPodName(template),
			Namespace: namespace,
			Labels: map[string]string{
				sandboxTemplateBuildLabel:      name,
				sandboxTemplateNamespaceLabel:  namespace,
				sandboxTemplateGenerationLabel: "1",
			},
			OwnerReferences: builderPodOwnerRefs(template),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "build"}}},
	}
	reconciler := newSandboxTemplateReconciler(t, template, pod)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseBuilding {
		t.Fatalf("expected the Pending phase to self-heal to Building, got %s", updated.Status.Phase)
	}
}

func TestSandboxTemplatePendingTimeoutDeletesPod(t *testing.T) {
	namespace, name := "tenant-a", "leak"
	template := newSandboxTemplate(namespace, name)
	stale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildPodName(template),
			Namespace: namespace,
			Labels: map[string]string{
				sandboxTemplateBuildLabel:      name,
				sandboxTemplateNamespaceLabel:  namespace,
				sandboxTemplateGenerationLabel: "1",
			},
			OwnerReferences:   builderPodOwnerRefs(template),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-podPendingTimeout - time.Minute)),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "build"}}},
	}
	stale.Status.Phase = corev1.PodPending
	reconciler := newSandboxTemplateReconciler(t, template, stale)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	if pods := listBuilderPods(t, reconciler, namespace); len(pods) != 0 {
		t.Fatalf("expected the stuck Pending pod to be deleted, got %d", len(pods))
	}
}

func TestSandboxTemplateEnsureBuilderRBACConvergesDrift(t *testing.T) {
	namespace, name := "tenant-a", "converge"
	// A tenant pre-created a same-named Role with broader rules and a
	// RoleBinding pointing at a different role/subject, plus a builder SA
	// carrying platform-managed imagePullSecrets.
	wideRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxTemplateBuilderServiceAccount, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"*"},
		}},
	}
	otherBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxTemplateBuilderServiceAccount, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "some-other-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "someone-else", Namespace: namespace}},
	}
	sa := &corev1.ServiceAccount{
		ObjectMeta:       metav1.ObjectMeta{Name: sandboxTemplateBuilderServiceAccount, Namespace: namespace},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}},
	}
	template := newSandboxTemplate(namespace, name)
	reconciler := newSandboxTemplateReconciler(t, template, wideRole, otherBinding, sa)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)

	// The Role is converged back onto pods/patch, the RoleBinding onto the
	// builder SA — drift is corrected, not trusted.
	var role rbacv1.Role
	if err := reconciler.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: sandboxTemplateBuilderServiceAccount}, &role); err != nil {
		t.Fatalf("get role: %v", err)
	}
	want := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"patch"}}}
	if !reflect.DeepEqual(role.Rules, want) {
		t.Fatalf("expected the role to be converged to pods/patch, got %+v", role.Rules)
	}
	var binding rbacv1.RoleBinding
	if err := reconciler.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: sandboxTemplateBuilderServiceAccount}, &binding); err != nil {
		t.Fatalf("get rolebinding: %v", err)
	}
	if binding.RoleRef.Name != sandboxTemplateBuilderServiceAccount ||
		len(binding.Subjects) != 1 || binding.Subjects[0].Name != sandboxTemplateBuilderServiceAccount {
		t.Fatalf("expected the rolebinding to be converged onto the builder SA, got %+v / %+v", binding.RoleRef, binding.Subjects)
	}
	// Platform-managed additions to the SA (imagePullSecrets) survive.
	var saAfter corev1.ServiceAccount
	if err := reconciler.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: sandboxTemplateBuilderServiceAccount}, &saAfter); err != nil {
		t.Fatalf("get serviceaccount: %v", err)
	}
	if len(saAfter.ImagePullSecrets) != 1 || saAfter.ImagePullSecrets[0].Name != "regcred" {
		t.Fatalf("expected SA imagePullSecrets to be preserved, got %+v", saAfter.ImagePullSecrets)
	}
}

func TestSandboxTemplateReconcileReplacesLeftoverPodOfDeletedTemplate(t *testing.T) {
	namespace, name := "tenant-a", "recreate"
	template := newSandboxTemplate(namespace, name)
	// A Pod left over by a previously deleted same-named template: same
	// deterministic name and labels, but a different owner UID. The
	// recreated template must not adopt it.
	leftover := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildPodName(template),
			Namespace: namespace,
			Labels: map[string]string{
				sandboxTemplateBuildLabel:      name,
				sandboxTemplateNamespaceLabel:  namespace,
				sandboxTemplateGenerationLabel: "1",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "sandbox.fast.io/v1alpha2",
				Kind:       "SandboxTemplate",
				Name:       name,
				UID:        "old-template-uid",
				Controller: ptr(true),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "build"}}},
	}
	reconciler := newSandboxTemplateReconciler(t, template, leftover)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler, namespace)
	if len(pods) != 1 {
		t.Fatalf("expected exactly one pod after replacing the leftover, got %d", len(pods))
	}
	// The leftover was deleted and replaced by a Pod owned by the new
	// template UID (fake client deletes synchronously, so one reconcile is
	// enough to observe the replacement).
	owned := false
	for _, ref := range pods[0].OwnerReferences {
		if ref.UID == template.UID && ref.Controller != nil && *ref.Controller {
			owned = true
		}
	}
	if !owned {
		t.Fatalf("expected the leftover to be replaced by a pod owned by the new template, got %+v", pods[0].OwnerReferences)
	}
}

func TestSandboxTemplateReconcileDefaultsPublishFromArtifactStore(t *testing.T) {
	namespace, name := "tenant-a", "platform-default"
	template := newSandboxTemplate(namespace, name)
	template.Spec.Output.Publish = ""
	template.Spec.Output.PublishSecretRef = &corev1.LocalObjectReference{Name: "publish-creds"}
	secret := newPublishSecret(namespace, "publish-creds")
	reconciler := newSandboxTemplateReconciler(t, template, secret)
	dir := t.TempDir()
	writeArtifactStoreFile(t, dir, artifactstore.StoreKey, "s3://platform/store\n")
	writeArtifactStoreFile(t, dir, artifactstore.EndpointKey, "http://minio.fast-sandbox-system.svc:9000")
	reconciler.ArtifactStore = artifactstore.Loader{Dir: dir}
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler, namespace)
	if len(pods) != 1 {
		t.Fatalf("expected one build pod, got %d", len(pods))
	}
	spec := builderSpecFromPod(t, &pods[0])
	if spec.Output.Publish != "s3://platform/store" {
		t.Fatalf("expected an empty publish to default to the platform store, got %q", spec.Output.Publish)
	}
	// The platform endpoint overrides the publish secret's endpoint so the
	// build targets the same address the node agents pull from.
	endpoint := builderEnvEntry(&pods[0], "AWS_ENDPOINT_URL")
	if endpoint == nil || endpoint.Value != "http://minio.fast-sandbox-system.svc:9000" || endpoint.ValueFrom != nil {
		t.Fatalf("expected AWS_ENDPOINT_URL to be the platform endpoint literal, got %+v", endpoint)
	}
	// The credential pair still comes from the publish secret, mounted as a
	// volume rather than env-injected.
	accessKey := builderEnvEntry(&pods[0], "AWS_ACCESS_KEY_ID")
	if accessKey != nil {
		t.Fatalf("expected no env-injected credential, got %+v", accessKey)
	}
	mounted := false
	for _, volume := range pods[0].Spec.Volumes {
		if volume.Name == "publish-credentials" && volume.Secret != nil && volume.Secret.SecretName == "publish-creds" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("expected the publish secret to be mounted, got %+v", pods[0].Spec.Volumes)
	}
}

func TestSandboxTemplateReconcileToleratesTrailingSlash(t *testing.T) {
	namespace, name := "tenant-a", "slash"
	template := newSandboxTemplate(namespace, name)
	template.Spec.Output.Publish = "s3://platform/store/"
	dir := t.TempDir()
	writeArtifactStoreFile(t, dir, artifactstore.StoreKey, "s3://platform/store")
	reconciler := newSandboxTemplateReconciler(t, template)
	reconciler.ArtifactStore = artifactstore.Loader{Dir: dir}
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	var updated apiv1alpha2.SandboxTemplate
	if err := reconciler.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseBuilding {
		t.Fatalf("expected a trailing slash to match the platform store, got phase %s", updated.Status.Phase)
	}
}

func TestSandboxTemplateReconcileRejectsPublishMismatch(t *testing.T) {
	namespace, name := "tenant-a", "drift"
	template := newSandboxTemplate(namespace, name)
	dir := t.TempDir()
	writeArtifactStoreFile(t, dir, artifactstore.StoreKey, "s3://platform/store")
	reconciler := newSandboxTemplateReconciler(t, template)
	reconciler.ArtifactStore = artifactstore.Loader{Dir: dir}
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	updated := getSandboxTemplate(t, reconciler, key)
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseFailed {
		t.Fatalf("expected a publish mismatch to fail the build, got phase %s", updated.Status.Phase)
	}
	condition := findBuildCondition(t, &updated)
	// The message must name both stores so the drift is fixable from the CR.
	if condition.Reason != "InvalidOutput" || !strings.Contains(condition.Message, "s3://bucket/sandbox-images/") ||
		!strings.Contains(condition.Message, "s3://platform/store") {
		t.Fatalf("expected an InvalidOutput condition naming both stores, got %+v", condition)
	}
	if pods := listBuilderPods(t, reconciler, namespace); len(pods) != 0 {
		t.Fatalf("expected no build pod for a mismatched publish, got %d", len(pods))
	}
}

func TestSandboxTemplateReconcileRequiresPublishWithoutPlatformStore(t *testing.T) {
	namespace, name := "tenant-a", "no-store"
	template := newSandboxTemplate(namespace, name)
	template.Spec.Output.Publish = ""
	reconciler := newSandboxTemplateReconciler(t, template)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	updated := getSandboxTemplate(t, reconciler, key)
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseFailed {
		t.Fatalf("expected a missing publish target to fail the build, got phase %s", updated.Status.Phase)
	}
	condition := findBuildCondition(t, &updated)
	if condition.Reason != reasonArtifactStoreUnconfigured ||
		!strings.Contains(condition.Message, "no platform artifact store") {
		t.Fatalf("expected a retryable ArtifactStoreUnconfigured condition, got %+v", condition)
	}
}

func TestSandboxTemplateReconcileHealsWhenStoreAppears(t *testing.T) {
	namespace, name := "tenant-a", "wait-for-store"
	dir := t.TempDir()
	template := newSandboxTemplate(namespace, name)
	template.Spec.Output.Publish = ""
	reconciler := newSandboxTemplateReconciler(t, template)
	reconciler.ArtifactStore = artifactstore.Loader{Dir: dir}
	key := client.ObjectKey{Namespace: namespace, Name: name}

	// The platform store is not configured yet: the build fails, but with a
	// retry so a ConfigMap appearing later heals the template without a spec
	// change (the store is read live per reconcile).
	result := reconcileOnce(t, reconciler, key)
	if result.RequeueAfter != artifactStoreRetryInterval {
		t.Fatalf("expected a retry when the store is unconfigured, got %v", result.RequeueAfter)
	}

	writeArtifactStoreFile(t, dir, artifactstore.StoreKey, "s3://platform/late\n")
	reconcileOnce(t, reconciler, key)
	updated := getSandboxTemplate(t, reconciler, key)
	if updated.Status.Phase != apiv1alpha2.SandboxTemplatePhaseBuilding {
		t.Fatalf("expected the template to heal once the store appears, got phase %s", updated.Status.Phase)
	}
	pods := listBuilderPods(t, reconciler, namespace)
	if len(pods) != 1 {
		t.Fatalf("expected one build pod after the store appeared, got %d", len(pods))
	}
	if got := builderSpecFromPod(t, &pods[0]).Output.Publish; got != "s3://platform/late" {
		t.Fatalf("expected the late store to be used, got %q", got)
	}
}

func TestSandboxTemplateReconcileFollowsMountedArtifactStore(t *testing.T) {
	namespace, name := "tenant-a", "live-config"
	dir := t.TempDir()
	writeArtifactStoreFile(t, dir, artifactstore.StoreKey, "s3://platform/first\n")
	writeArtifactStoreFile(t, dir, artifactstore.EndpointKey, "http://minio-1:9000")
	template := newSandboxTemplate(namespace, name)
	template.Spec.Output.Publish = ""
	reconciler := newSandboxTemplateReconciler(t, template)
	reconciler.ArtifactStore = artifactstore.Loader{Dir: dir}
	key := client.ObjectKey{Namespace: namespace, Name: name}

	reconcileOnce(t, reconciler, key)
	pods := listBuilderPods(t, reconciler, namespace)
	if len(pods) != 1 {
		t.Fatalf("expected one build pod, got %d", len(pods))
	}
	if got := builderSpecFromPod(t, &pods[0]).Output.Publish; got != "s3://platform/first" {
		t.Fatalf("expected the mounted store, got %q", got)
	}
	if got := builderEnvEntry(&pods[0], "AWS_ENDPOINT_URL"); got == nil || got.Value != "http://minio-1:9000" {
		t.Fatalf("expected the mounted endpoint, got %+v", got)
	}

	// A ConfigMap edit (projected file swap) must reach the next build: bump
	// the generation so a fresh pod is created without a controller restart.
	writeArtifactStoreFile(t, dir, artifactstore.StoreKey, "s3://platform/second")
	writeArtifactStoreFile(t, dir, artifactstore.EndpointKey, "http://minio-2:9000\n")
	current := getSandboxTemplate(t, reconciler, key)
	current.Generation = 2
	if err := reconciler.Update(context.Background(), &current); err != nil {
		t.Fatalf("update template: %v", err)
	}
	reconcileOnce(t, reconciler, key)
	pods = listBuilderPods(t, reconciler, namespace)
	if len(pods) != 1 {
		t.Fatalf("expected one generation-2 build pod, got %d", len(pods))
	}
	if got := builderSpecFromPod(t, &pods[0]).Output.Publish; got != "s3://platform/second" {
		t.Fatalf("expected the updated store to apply without a restart, got %q", got)
	}
	if got := builderEnvEntry(&pods[0], "AWS_ENDPOINT_URL"); got == nil || got.Value != "http://minio-2:9000" {
		t.Fatalf("expected the updated endpoint to apply without a restart, got %+v", got)
	}
}

func writeArtifactStoreFile(t *testing.T, dir, key, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, key), []byte(value), 0o644); err != nil {
		t.Fatalf("write artifact store %s: %v", key, err)
	}
}

func getSandboxTemplate(t *testing.T, r *SandboxTemplateReconciler, key client.ObjectKey) apiv1alpha2.SandboxTemplate {
	t.Helper()
	var template apiv1alpha2.SandboxTemplate
	if err := r.Get(context.Background(), key, &template); err != nil {
		t.Fatalf("get template: %v", err)
	}
	return template
}

func findBuildCondition(t *testing.T, template *apiv1alpha2.SandboxTemplate) apiv1alpha2.SandboxTemplateCondition {
	t.Helper()
	for _, condition := range template.Status.Conditions {
		if condition.Type == apiv1alpha2.SandboxTemplateConditionBuildSucceeded {
			return condition
		}
	}
	t.Fatalf("expected a BuildSucceeded condition, got %+v", template.Status.Conditions)
	return apiv1alpha2.SandboxTemplateCondition{}
}

func builderSpecFromPod(t *testing.T, pod *corev1.Pod) apiv1alpha2.SandboxTemplateSpec {
	t.Helper()
	entry := builderEnvEntry(pod, builderImageEnv)
	if entry == nil {
		t.Fatalf("build pod missing %s env", builderImageEnv)
	}
	var spec apiv1alpha2.SandboxTemplateSpec
	if err := json.Unmarshal([]byte(entry.Value), &spec); err != nil {
		t.Fatalf("SANDBOX_TEMPLATE_SPEC is not valid SandboxTemplateSpec JSON: %v", err)
	}
	return spec
}

func builderEnvEntry(pod *corev1.Pod, name string) *corev1.EnvVar {
	for index := range pod.Spec.Containers[0].Env {
		if pod.Spec.Containers[0].Env[index].Name == name {
			return &pod.Spec.Containers[0].Env[index]
		}
	}
	return nil
}
