package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifactstore"
)

const (
	workdirEnvName                       = "SANDBOX_TEMPLATE_WORKDIR"
	sandboxTemplateBuildDir              = "/build"
	sandboxTemplateBuilderServiceAccount = "sandbox-template-builder"
	// sandboxTemplateFinalizer is a legacy cleanup finalizer from the era
	// when build Pods ran in the platform namespace and could not carry an
	// owner reference. Build Pods now live in the template's namespace and
	// are owned by the template (cascading GC), so the finalizer is only
	// stripped from templates created before the owner-reference change;
	// nothing new ever sets it.
	sandboxTemplateFinalizer = "sandbox.fast.io/sandboxtemplate-cleanup"
	// buildDeadlineSeconds bounds a single build run; podPendingTimeout
	// fails a build whose Pod never leaves Pending (e.g. no KVM node).
	buildDeadlineSeconds = int64(2 * 60 * 60)
	podPendingTimeout    = 10 * time.Minute
	// artifactStoreRetryInterval retries a build that failed because no
	// platform artifact store was configured: the store is read live, so a
	// ConfigMap created afterwards must heal the template without a spec
	// change. reasonArtifactStoreUnconfigured marks that retryable failure;
	// a publish mismatch keeps the terminal InvalidOutput reason.
	artifactStoreRetryInterval      = time.Minute
	reasonArtifactStoreUnconfigured = "ArtifactStoreUnconfigured"
)

// builderImageEnv is the environment variable carrying the serialized
// SandboxTemplate spec into the build Pod.
const builderImageEnv = "SANDBOX_TEMPLATE_SPEC"

// sandboxTemplateBuildLabel links a build Pod to its SandboxTemplate (name
// and namespace; the Pod also carries an owner reference — the labels let
// the controller adopt, watch and prune builds cheaply);
// sandboxTemplateGenerationLabel carries the template generation the Pod
// was created for so a mid-build spec change is never adopted.
const (
	sandboxTemplateBuildLabel        = "sandbox.fast.io/sandboxtemplate"
	sandboxTemplateNamespaceLabel    = "sandbox.fast.io/template-namespace"
	sandboxTemplateGenerationLabel   = "sandbox.fast.io/generation"
	sandboxTemplateManifestRefAnnot  = "sandbox.fast.io/manifest-ref"
	sandboxTemplateArtifactDigestAnn = "sandbox.fast.io/artifact-digest"
	// sandboxTemplateKVMNodeLabel selects nodes that expose /dev/kvm and
	// /dev/net/tun; the build Pod is pinned to them via nodeSelector.
	sandboxTemplateKVMNodeLabel = "sandbox.fast.io/kvm"
)

// sandboxTemplatePublishSecretDir is where the build Pod mounts the
// template's publishSecretRef; the builder reads the credential keys as files
// (secret values are never injected as env). publishCredentialsDirEnv carries
// the path to the builder and is only set when the reference exists; unset
// means the build relies on ambient credentials (IRSA / node metadata).
const (
	sandboxTemplatePublishSecretDir = "/etc/fast-sandbox/publish-credentials"
	publishCredentialsDirEnv        = "SANDBOX_TEMPLATE_PUBLISH_SECRET_DIR"
)

// SandboxTemplateReconciler reconciles SandboxTemplate resources by driving
// golden-image builds as Pods (design: SandboxTemplate — declarative
// golden-image builds). The Pod executes the build pipeline (convert →
// validate-boot → snapshot → package) and publishes the content-addressed
// artifacts; this controller tracks the Pod and records the outcome (phase,
// conditions, manifestRef, artifactDigest) in status. `output.prime` is
// reserved but not implemented (see the PrimeSpec doc comment). Build Pods
// run in the template's own namespace and carry a controller owner
// reference, so deleting a template cascades to its build Pods via the
// garbage collector; finished Pods are additionally reaped after BuildTTL.
type SandboxTemplateReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// BuilderImage is the image that executes the build pipeline stages.
	BuilderImage string
	// BuildTTL is how long a finished build Pod is retained before cleanup.
	BuildTTL time.Duration
	// ArtifactStore reads the platform artifact store (s3://bucket/prefix)
	// from the mounted fast-sandbox-artifact-store ConfigMap on every
	// reconcile, so a ConfigMap edit applies to the next build without a
	// restart. It defaults an empty spec.output.publish and is enforced
	// against a non-empty one, so the builder and the node agents cannot
	// silently diverge; empty disables both behaviors.
	ArtifactStore artifactstore.Loader
}

// Reconcile drives one SandboxTemplate towards its desired build state.
func (r *SandboxTemplateReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) { //nolint:gocognit,maintidx // pre-existing reconcile state machine; refactor tracked separately
	logger := klog.FromContext(ctx)
	var template apiv1alpha2.SandboxTemplate
	if err := r.Get(ctx, request.NamespacedName, &template); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Deletion: build Pods are owned by the template (same namespace, owner
	// reference), so the garbage collector cascades the delete. The only
	// remaining job here is stripping the legacy cleanup finalizer from
	// templates created before the owner-reference change.
	if !template.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&template, sandboxTemplateFinalizer) {
			controllerutil.RemoveFinalizer(&template, sandboxTemplateFinalizer)
			return ctrl.Result{}, r.Update(ctx, &template)
		}
		return ctrl.Result{}, nil
	}

	// A build already applied to the current generation is terminal. The
	// finished Pods are kept around for BuildTTL (so their annotations remain
	// inspectable) and cleaned up opportunistically afterwards; requeue at
	// the earliest retention expiry so a quiet cluster still reaps them. The
	// one exception is a failure caused by the platform artifact store not
	// being configured yet: the store is read live, so that state is retried
	// and can heal without a template change.
	if template.Status.ObservedGeneration == template.Generation &&
		(template.Status.Phase == apiv1alpha2.SandboxTemplatePhaseSucceeded ||
			template.Status.Phase == apiv1alpha2.SandboxTemplatePhaseFailed) &&
		!failedArtifactStoreUnconfigured(&template) {
		if err := r.cleanupFinishedPods(ctx, &template); err != nil {
			return ctrl.Result{}, err
		}
		if requeueAfter, err := r.earliestRetention(ctx, &template); err != nil {
			return ctrl.Result{}, err
		} else if requeueAfter > 0 {
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		return ctrl.Result{}, nil
	}

	if template.Status.ObservedGeneration != template.Generation {
		template.Status = apiv1alpha2.SandboxTemplateStatus{
			Phase:              apiv1alpha2.SandboxTemplatePhasePending,
			ObservedGeneration: template.Generation,
		}
		if err := r.Status().Update(ctx, &template); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Resolve the effective publish target before any build state is
	// touched: the platform artifact store is the single source of truth,
	// so a template that omits output.publish inherits the platform value,
	// and a template that contradicts it fails here (visible on the CR)
	// instead of publishing into a store no node agent reads. The store is
	// resolved per reconcile, so a ConfigMap edit applies to the next build
	// without a controller restart.
	store, err := r.ArtifactStore.Load()
	if err != nil {
		return ctrl.Result{}, err
	}
	output, err := r.effectiveOutput(template.Spec.Output, store.Store)
	if err != nil {
		reason := "InvalidOutput"
		requeueAfter := time.Duration(0)
		if errors.Is(err, errArtifactStoreUnconfigured) {
			// Not a spec error: retain nothing terminal and retry, because
			// configuring the store (or creating the ConfigMap) is enough to
			// heal the template.
			reason = reasonArtifactStoreUnconfigured
			requeueAfter = artifactStoreRetryInterval
		}
		if failErr := r.failBuild(ctx, &template, reason, err); failErr != nil {
			return ctrl.Result{}, failErr
		}
		if requeueAfter > 0 {
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		return ctrl.Result{}, nil
	}

	pod, err := r.findBuildPod(ctx, &template)
	if err != nil {
		return ctrl.Result{}, err
	}
	// A new generation supersedes any in-flight build: delete stale Pods of
	// earlier generations immediately (running or not) so a tenant churning
	// generations cannot stack up concurrent privileged builds.
	if err := r.cleanupStalePods(ctx, &template); err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case pod == nil:
		// No build Pod yet: create one for this generation. Create errors
		// (apiserver flake, quota) requeue with backoff instead of failing
		// the generation permanently — only the Pod's own outcome decides
		// Succeeded/Failed.
		if err := r.createBuildPod(ctx, &template, output, store.Endpoint); err != nil {
			return ctrl.Result{}, err
		}
		template.Status.Phase = apiv1alpha2.SandboxTemplatePhaseBuilding
		return ctrl.Result{}, r.updatePhase(ctx, &template)
	case pod.Status.Phase == corev1.PodSucceeded:
		// Build Pod finished: consume the reported annotations, mark the
		// build succeeded, and clear the finished Pod.
		logger.Info("sandbox template build pod succeeded", "template", template.Name, "pod", pod.Name)
		template.Status.ManifestRef = pod.Annotations[sandboxTemplateManifestRefAnnot]
		template.Status.ArtifactDigest = pod.Annotations[sandboxTemplateArtifactDigestAnn]
		template.Status.Phase = apiv1alpha2.SandboxTemplatePhaseSucceeded
		now := metav1.Now()
		template.Status.LastBuildTime = &now
		upsertCondition(&template, apiv1alpha2.SandboxTemplateCondition{
			Type:    apiv1alpha2.SandboxTemplateConditionBuildSucceeded,
			Status:  corev1.ConditionTrue,
			Reason:  "BuildCompleted",
			Message: "golden image build completed",
		})
		if err := r.updatePhase(ctx, &template); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.cleanupPod(ctx, pod)
	case pod.Status.Phase == corev1.PodFailed:
		reason := "build pod failed"
		message := ""
		exitCode := int32(-1)
		for _, status := range pod.Status.ContainerStatuses {
			// Terminated first; fall back to Waiting (e.g.
			// CreateContainerConfigError for a missing secret) so the
			// condition carries the actionable reason.
			if status.State.Terminated != nil {
				if status.State.Terminated.Reason != "" {
					reason = status.State.Terminated.Reason
				}
				message = status.State.Terminated.Message
				exitCode = status.State.Terminated.ExitCode
				break
			}
			if status.State.Waiting != nil && status.State.Waiting.Reason != "" {
				reason = status.State.Waiting.Reason
				message = status.State.Waiting.Message
			}
		}
		failure := fmt.Errorf("%s%s%s", reason, exitCodeSuffix(exitCode), suffixIfNotEmpty(message))
		logger.Error(failure, "Sandbox template build pod failed", "template", template.Name, "pod", pod.Name)
		if err := r.failBuild(ctx, &template, "PodFailed", failure); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.cleanupPod(ctx, pod)
	case pod.Status.Phase == corev1.PodPending && time.Since(pod.CreationTimestamp.Time) > podPendingTimeout:
		// Never scheduled (e.g. no node with sandbox.fast.io/kvm=true):
		// fail instead of staying Building forever. The unschedulable Pod
		// is deleted here — activeDeadlineSeconds does not apply to Pods
		// that never left Pending, and the terminal cleanup only reaps
		// finished Pods.
		err := fmt.Errorf("build pod %s stuck in Pending for %s", pod.Name, podPendingTimeout)
		logger.Error(err, "Sandbox template build pod never left Pending", "template", template.Name)
		if failErr := r.failBuild(ctx, &template, "PodPendingTimeout", err); failErr != nil {
			return ctrl.Result{}, failErr
		}
		return ctrl.Result{}, r.Delete(ctx, pod, client.PropagationPolicy(metav1.DeletePropagationBackground))
	default:
		// Pod still running. Self-heal the phase: a restart between Pod
		// creation and the status update (or a failed update) would
		// otherwise leave the template stuck in Pending.
		if template.Status.Phase != apiv1alpha2.SandboxTemplatePhaseBuilding {
			template.Status.Phase = apiv1alpha2.SandboxTemplatePhaseBuilding
			if err := r.updatePhase(ctx, &template); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Pod events are watched; the poll only covers transient races.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
}

// SetupWithManager registers the controller for SandboxTemplate objects.
// Build Pods run in the template's namespace and carry an owner reference;
// they are also watched by label so the controller can drive the build
// lifecycle (phase tracking, stale-generation replacement, TTL reaping)
// that owner references alone do not cover.
func (r *SandboxTemplateReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&apiv1alpha2.SandboxTemplate{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapBuildPod),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				// Only builder Pods (tagged with the template build label)
				// trigger reconciles; drop the cluster-wide watch noise.
				return obj.GetLabels()[sandboxTemplateBuildLabel] != ""
			}))).
		Complete(r)
}

// mapBuildPod maps a build Pod event to its SandboxTemplate (identified by
// the template name and namespace labels), or to nothing when the Pod is not
// a builder.
func (r *SandboxTemplateReconciler) mapBuildPod(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	name := pod.Labels[sandboxTemplateBuildLabel]
	namespace := pod.Labels[sandboxTemplateNamespaceLabel]
	if name == "" || namespace == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: namespace, Name: name}}}
}

// findBuildPod returns the build Pod for the template's current generation,
// or nil. Build Pods run in the template's own namespace and are linked to
// the template by labels (and an owner reference). A Pod built for an older
// generation (spec changed mid-build) is never adopted: its outcome does not
// apply to the new spec. Neither is a Pod that is not owned by this template
// UID — a same-named Pod left over by a previously deleted template (its
// name is deterministic, so a recreated template collides on the name while
// the old Pod is still being garbage-collected) must not be adopted.
func (r *SandboxTemplateReconciler) findBuildPod(ctx context.Context, template *apiv1alpha2.SandboxTemplate) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(template.Namespace), client.MatchingLabels{
		sandboxTemplateBuildLabel:     templateLabelValue(template.Name),
		sandboxTemplateNamespaceLabel: template.Namespace,
	}); err != nil {
		return nil, err
	}
	generation := strconv.FormatInt(template.Generation, 10)
	var oldest *corev1.Pod
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Labels[sandboxTemplateGenerationLabel] != generation {
			continue
		}
		if !ownedByTemplate(pod, template) {
			continue
		}
		// Pick deterministically: if duplicates ever exist for the same
		// generation, adopt the oldest rather than an arbitrary one.
		if oldest == nil || pod.CreationTimestamp.Before(&oldest.CreationTimestamp) {
			oldest = pod
		}
	}
	return oldest, nil
}

// earliestRetention returns how long until the earliest retained finished
// Pod expires its BuildTTL (0 when nothing is retained), so the terminal
// reconcile can requeue and reap it without external events.
func (r *SandboxTemplateReconciler) earliestRetention(ctx context.Context, template *apiv1alpha2.SandboxTemplate) (time.Duration, error) {
	if r.BuildTTL <= 0 {
		return 0, nil
	}
	pods, err := r.listBuildPods(ctx, template)
	if err != nil {
		return 0, err
	}
	earliest := time.Duration(0)
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		completion := podCompletionTime(pod)
		if completion == nil {
			continue
		}
		remaining := r.BuildTTL - time.Since(*completion)
		if remaining <= 0 {
			continue
		}
		if earliest == 0 || remaining < earliest {
			earliest = remaining
		}
	}
	return earliest, nil
}

// cleanupStalePods deletes build Pods of earlier generations once a new
// generation arrives; a mid-build spec change replaces the in-flight builder
// instead of letting it run to completion. Pods not owned by this template's
// UID (leftovers of a deleted same-named template) are deleted the same way,
// so the deterministic name cannot trap the controller on a stale Pod.
func (r *SandboxTemplateReconciler) cleanupStalePods(ctx context.Context, template *apiv1alpha2.SandboxTemplate) error {
	pods, err := r.listBuildPods(ctx, template)
	if err != nil {
		return err
	}
	generation := strconv.FormatInt(template.Generation, 10)
	for index := range pods.Items {
		pod := &pods.Items[index]
		if !ownedByTemplate(pod, template) {
			// Leftover of a deleted same-named template (or a stray):
			// never adopt, delete immediately. If it is already being
			// garbage-collected, nothing to do.
			if pod.DeletionTimestamp == nil {
				klog.FromContext(ctx).V(1).Info("deleting un-owned stale template build pod", "template", template.Name, "pod", pod.Name, "podNamespace", pod.Namespace)
				if err := r.Delete(ctx, pod, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
			continue
		}
		if pod.Labels[sandboxTemplateGenerationLabel] == generation {
			continue
		}
		// Stale builders are replaced unconditionally — delete immediately,
		// without the BuildTTL retention (that only applies to the finished
		// Pod of the CURRENT generation).
		if pod.DeletionTimestamp != nil {
			continue
		}
		klog.FromContext(ctx).V(1).Info("deleting stale template build pod from a previous generation", "template", template.Name, "pod", pod.Name, "podNamespace", pod.Namespace)
		if err := r.Delete(ctx, pod, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// ownedByTemplate reports whether the Pod carries a controller owner
// reference to the given template. buildPodName is deterministic, so a
// recreated template collides on the Pod name while the old Pod is still
// being garbage-collected; the UID check is what keeps that leftover from
// being adopted.
func ownedByTemplate(pod *corev1.Pod, template *apiv1alpha2.SandboxTemplate) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.UID == template.UID && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// cleanupFinishedPods removes finished build Pods (any generation, including
// same-generation duplicates that were never adopted) once their BuildTTL
// expired, so a crash between create and adopt cannot leave builders running
// forever.
func (r *SandboxTemplateReconciler) cleanupFinishedPods(ctx context.Context, template *apiv1alpha2.SandboxTemplate) error {
	pods, err := r.listBuildPods(ctx, template)
	if err != nil {
		return err
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		if err := r.cleanupPod(ctx, pod); err != nil {
			return err
		}
	}
	return nil
}

func (r *SandboxTemplateReconciler) listBuildPods(ctx context.Context, template *apiv1alpha2.SandboxTemplate) (*corev1.PodList, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(template.Namespace), client.MatchingLabels{
		sandboxTemplateBuildLabel:     templateLabelValue(template.Name),
		sandboxTemplateNamespaceLabel: template.Namespace,
	}); err != nil {
		return nil, err
	}
	return &pods, nil
}

// errArtifactStoreUnconfigured is the retryable empty-publish/no-store state:
// unlike a publish mismatch it is fixed by configuring the platform store, so
// the reconciler must not treat it as terminal.
var errArtifactStoreUnconfigured = errors.New("no platform artifact store is configured")

// effectiveOutput resolves the publish target for a build. The platform
// artifact store (resolved from the shared fast-sandbox-artifact-store
// ConfigMap) defaults an empty publish target, and a non-empty target must
// match it: a template pointing at a different store is configuration drift,
// not a second publish destination, so it fails the build instead of
// publishing where no node agent reads. Trailing slashes are ignored in the
// comparison because the builder strips them when naming objects.
func (r *SandboxTemplateReconciler) effectiveOutput(output apiv1alpha2.OutputSpec, storeRoot string) (apiv1alpha2.OutputSpec, error) {
	if output.Publish == "" {
		if storeRoot == "" {
			return output, fmt.Errorf("%w: spec.output.publish is empty (set output.publish or configure the controller artifact store)", errArtifactStoreUnconfigured)
		}
		output.Publish = storeRoot
		return output, nil
	}
	if storeRoot != "" && strings.TrimRight(output.Publish, "/") != strings.TrimRight(storeRoot, "/") {
		return output, fmt.Errorf("spec.output.publish %q does not match the platform artifact store %q; omit output.publish to use the platform store", output.Publish, storeRoot)
	}
	return output, nil
}

// createBuildPod launches the builder Pod that executes the pipeline. The
// template spec — with the effective (platform-defaulted) output — is
// serialized into the environment; the builder image runs the stages
// (convert → validate-boot → snapshot → package) and publishes the
// artifacts. The Pod runs in the template's namespace (so it can carry
// an owner reference and the publish secret can be SecretKeyRef'd from the
// same namespace) and has a deterministic name (<template>-build-<gen>) so
// concurrent reconciles dedupe via AlreadyExists.
func (r *SandboxTemplateReconciler) createBuildPod(ctx context.Context, template *apiv1alpha2.SandboxTemplate, output apiv1alpha2.OutputSpec, endpoint string) error {
	// The build Pod self-reports its outcome by merge-patching its own
	// annotations; provision the minimal builder RBAC (SA + Role + RoleBinding,
	// pods/patch only) in the template's namespace, idempotently.
	if err := r.ensureBuilderRBAC(ctx, template.Namespace); err != nil {
		return err
	}
	spec := template.Spec
	spec.Output = output
	payload, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	env := []corev1.EnvVar{{
		Name: builderImageEnv, Value: string(payload),
	}, {
		Name: workdirEnvName, Value: sandboxTemplateBuildDir,
	}, {
		Name: envPodName, ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
		},
	}, {
		Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		},
	}}
	// The publish credentials are mounted, not injected as env: the secret
	// stays in the template's namespace and the builder reads the files. The
	// platform endpoint (when configured) wins over the mounted secret's
	// endpoint, so the build targets the store the node agents pull from.
	volumeMounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: sandboxTemplateBuildDir},
		{Name: "kvm", MountPath: "/dev/kvm"},
		{Name: "net-tun", MountPath: "/dev/net/tun"},
	}
	volumes := []corev1.Volume{
		{
			Name:         "workspace",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: "kvm",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: "/dev/kvm", Type: ptr(corev1.HostPathCharDev),
			}},
		},
		{
			Name: "net-tun",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: "/dev/net/tun", Type: ptr(corev1.HostPathCharDev),
			}},
		},
	}
	if ref := template.Spec.Output.PublishSecretRef; ref != nil {
		env = append(env, corev1.EnvVar{Name: publishCredentialsDirEnv, Value: sandboxTemplatePublishSecretDir})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: "publish-credentials", MountPath: sandboxTemplatePublishSecretDir, ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "publish-credentials",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: ref.Name,
			}},
		})
	}
	if endpoint != "" {
		env = append(env, corev1.EnvVar{Name: "AWS_ENDPOINT_URL", Value: endpoint})
	}

	// The build Pod runs the sandbox template builder: privileged for the
	// loop mount, with the host's KVM/tun devices passed through via hostPath
	// (privileged alone does not expose host devices on Kubernetes), pinned
	// to KVM-capable nodes, with the template spec carried in the environment
	// and a per-template workspace volume. The memory limit tracks the
	// guest's machine.memory (the VMM maps it 1:1) plus headroom, so a large
	// spec does not get OOM-killed by a fixed 4 Gi limit.
	memoryLimit := resource.MustParse("4Gi")
	if machineMemory, err := resource.ParseQuantity(template.Spec.Machine.Memory); err == nil {
		withHeadroom := machineMemory.DeepCopy()
		withHeadroom.Add(resource.MustParse("1Gi"))
		if withHeadroom.Cmp(memoryLimit) > 0 {
			memoryLimit = withHeadroom
		}
	}
	// The workspace emptyDir lives on the node's local disk and holds the
	// rootfs plus the 1:1 guest memory snapshot; size the ephemeral-storage
	// limit from the declared rootfsSize + machine.memory so a build cannot
	// fill a shared node's disk. The kubelet enforces this via eviction.
	storageLimit := resource.MustParse("10Gi")
	if rootfsSize, err := resource.ParseQuantity(template.Spec.Output.RootfsSize); err == nil {
		total := rootfsSize.DeepCopy()
		if machineMemory, err := resource.ParseQuantity(template.Spec.Machine.Memory); err == nil {
			total.Add(machineMemory)
		}
		total.Add(resource.MustParse("2Gi"))
		if total.Cmp(storageLimit) > 0 {
			storageLimit = total
		}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildPodName(template),
			Namespace: template.Namespace,
			Labels: map[string]string{
				sandboxTemplateBuildLabel:      templateLabelValue(template.Name),
				sandboxTemplateNamespaceLabel:  template.Namespace,
				sandboxTemplateGenerationLabel: strconv.FormatInt(template.Generation, 10),
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: sandboxTemplateBuilderServiceAccount,
			// The builder needs /dev/kvm and /dev/net/tun; label the
			// KVM-capable nodes accordingly.
			NodeSelector:          map[string]string{sandboxTemplateKVMNodeLabel: "true"},
			ActiveDeadlineSeconds: ptr(buildDeadlineSeconds),
			Containers: []corev1.Container{{
				Name:            "build",
				Image:           r.BuilderImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Env:             env,
				SecurityContext: &corev1.SecurityContext{Privileged: ptr(true)},
				// Multi-GiB snapshot work; requests guarantee scheduling
				// headroom, limits bound the privileged builder.
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("1"),
						corev1.ResourceMemory:           resource.MustParse("2Gi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("5Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("2"),
						corev1.ResourceMemory:           memoryLimit,
						corev1.ResourceEphemeralStorage: storageLimit,
					},
				},
				VolumeMounts: volumeMounts,
			}},
			Volumes: volumes,
		},
	}
	// Merge the platform-provided builder Pod template overlay (mounted
	// fast-sandbox-builder-pod-template ConfigMap) into the enforced spec
	// before the owner reference: only scheduling fields are mergeable, the
	// platform-owned shape above stays authoritative.
	overlayRaw, err := loadBuilderPodTemplateOverlay(builderPodTemplateDir)
	if err != nil {
		return err
	}
	if err := applyBuilderPodTemplateOverlay(&pod.Spec, overlayRaw); err != nil {
		return err
	}
	// The Pod is owned by the template, so deleting the template cascades
	// to it via the garbage collector.
	if err := ctrl.SetControllerReference(template, pod, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// ensureBuilderRBAC converges the builder ServiceAccount, Role and
// RoleBinding in the given namespace onto exactly the minimal builder
// privileges (pods/patch). The build Pod runs in the template's (tenant)
// namespace and self-reports its outcome by merge-patching its own
// annotations, so it needs pods/patch there. The RBAC is provisioned per
// namespace on demand because a privileged platform SA must not be
// statically bound into arbitrary tenant namespaces. Existing objects are
// not trusted: a same-named Role or RoleBinding with broader content (e.g.
// pre-created by a tenant) is converged back onto the enforced shape, so
// the boundary holds by construction rather than by assumption. The
// ServiceAccount itself is only created, never rewritten, so platform
// additions such as imagePullSecrets for private registry pulls survive.
func (r *SandboxTemplateReconciler) ensureBuilderRBAC(ctx context.Context, namespace string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxTemplateBuilderServiceAccount, Namespace: namespace},
	}
	if err := r.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxTemplateBuilderServiceAccount, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"patch"},
		}},
	}
	if err := r.ensureRole(ctx, role); err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxTemplateBuilderServiceAccount, Namespace: namespace},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     sandboxTemplateBuilderServiceAccount,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      sandboxTemplateBuilderServiceAccount,
			Namespace: namespace,
		}},
	}
	if err := r.ensureRoleBinding(ctx, binding); err != nil {
		return err
	}
	return nil
}

// ensureRole creates the Role or, when a same-named one already exists,
// rewrites its rules onto the desired set. The Role name is reserved for
// the builder, so any existing content (broader or unrelated rules) is
// treated as drift to be corrected, never as something to trust.
func (r *SandboxTemplateReconciler) ensureRole(ctx context.Context, desired *rbacv1.Role) error {
	if err := r.Create(ctx, desired); err == nil || !apierrors.IsAlreadyExists(err) {
		return err
	}
	current := &rbacv1.Role{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		return err
	}
	if reflect.DeepEqual(current.Rules, desired.Rules) {
		return nil
	}
	current.Rules = desired.Rules
	return r.Update(ctx, current)
}

// ensureRoleBinding creates the RoleBinding or, when a same-named one
// already exists, rewrites its roleRef and subjects onto the desired ones
// (same rationale as ensureRole: bindings are drift-corrected, not trusted).
func (r *SandboxTemplateReconciler) ensureRoleBinding(ctx context.Context, desired *rbacv1.RoleBinding) error {
	if err := r.Create(ctx, desired); err == nil || !apierrors.IsAlreadyExists(err) {
		return err
	}
	current := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		return err
	}
	if reflect.DeepEqual(current.RoleRef, desired.RoleRef) && reflect.DeepEqual(current.Subjects, desired.Subjects) {
		return nil
	}
	current.RoleRef = desired.RoleRef
	current.Subjects = desired.Subjects
	return r.Update(ctx, current)
}

// updatePhase persists the phase and its transition condition.
func (r *SandboxTemplateReconciler) updatePhase(ctx context.Context, template *apiv1alpha2.SandboxTemplate) error {
	return r.Status().Update(ctx, template)
}

// failedArtifactStoreUnconfigured reports whether a terminal Failed phase is
// the retryable "platform artifact store not configured" state rather than a
// spec error.
func failedArtifactStoreUnconfigured(template *apiv1alpha2.SandboxTemplate) bool {
	if template.Status.Phase != apiv1alpha2.SandboxTemplatePhaseFailed {
		return false
	}
	for _, condition := range template.Status.Conditions {
		if condition.Type == apiv1alpha2.SandboxTemplateConditionBuildSucceeded {
			return condition.Reason == reasonArtifactStoreUnconfigured
		}
	}
	return false
}

// failBuild marks the template as Failed with a condition. The condition is
// upserted by type so repeated failures do not accumulate duplicates.
func (r *SandboxTemplateReconciler) failBuild(ctx context.Context, template *apiv1alpha2.SandboxTemplate, reason string, err error) error {
	template.Status.Phase = apiv1alpha2.SandboxTemplatePhaseFailed
	upsertCondition(template, apiv1alpha2.SandboxTemplateCondition{
		Type:    apiv1alpha2.SandboxTemplateConditionBuildSucceeded,
		Status:  corev1.ConditionFalse,
		Reason:  reason,
		Message: err.Error(),
	})
	return r.updatePhase(ctx, template)
}

// upsertCondition replaces an existing condition of the same type (or appends
// a new one). LastTransitionTime is only stamped when the condition actually
// transitions (Status or Reason changes); repeated reconciles keep the
// original timestamp.
func upsertCondition(template *apiv1alpha2.SandboxTemplate, condition apiv1alpha2.SandboxTemplateCondition) {
	for index := range template.Status.Conditions {
		if template.Status.Conditions[index].Type == condition.Type {
			if template.Status.Conditions[index].Status == condition.Status &&
				template.Status.Conditions[index].Reason == condition.Reason {
				template.Status.Conditions[index].Message = condition.Message
				return
			}
			now := metav1.Now()
			condition.LastTransitionTime = &now
			template.Status.Conditions[index] = condition
			return
		}
	}
	now := metav1.Now()
	condition.LastTransitionTime = &now
	template.Status.Conditions = append(template.Status.Conditions, condition)
}

// cleanupPod removes a finished build Pod so future generations start from a
// clean slate. When BuildTTL is set, the Pod is kept that long (so its
// annotations remain inspectable) before being deleted.
func (r *SandboxTemplateReconciler) cleanupPod(ctx context.Context, pod *corev1.Pod) error {
	if pod.DeletionTimestamp != nil {
		return nil
	}
	if r.BuildTTL > 0 {
		if completion := podCompletionTime(pod); completion != nil && time.Since(*completion) < r.BuildTTL {
			return nil
		}
	}
	return r.Delete(ctx, pod, client.PropagationPolicy(metav1.DeletePropagationBackground))
}

// podCompletionTime returns when the Pod finished (kubelet marks the Pod
// Ready condition with reason PodCompleted once all containers exit).
func podCompletionTime(pod *corev1.Pod) *time.Time {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Reason == "PodCompleted" {
			return &condition.LastTransitionTime.Time
		}
	}
	return nil
}

// buildPodName derives the deterministic build Pod name from the template
// name (Pods run in the template's own namespace, so names cannot collide
// across tenants). Long names are truncated with a sha256 prefix; the
// generation suffix keeps rebuilds distinct per revision, and the result is
// bounded by the Pod-name (DNS subdomain) limit rather than the 63-char
// label budget.
func buildPodName(template *apiv1alpha2.SandboxTemplate) string {
	seed := template.Name
	if len(seed) > 40 {
		sum := sha256.Sum256([]byte(seed))
		seed = fmt.Sprintf("%s-%x", seed[:40], sum[:8])
	}
	return fmt.Sprintf("%s-build-%d", seed, template.Generation)
}

// templateLabelValue maps a template name onto a Kubernetes label value
// (<=63 chars): short names pass through, long ones are truncated with a
// sha256 prefix (64 bits) so collisions between long names are negligible.
func templateLabelValue(name string) string {
	if len(name) <= 40 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s-%x", name[:40], sum[:8])
}

// suffixIfNotEmpty returns ": <suffix>" when suffix is non-empty, or "".
func suffixIfNotEmpty(suffix string) string {
	if suffix == "" {
		return ""
	}
	return ": " + suffix
}

// exitCodeSuffix renders " (exit code N)" only when a real exit code is
// known; waiting-only failures (CreateContainerConfigError etc.) have none.
func exitCodeSuffix(exitCode int32) string {
	if exitCode < 0 {
		return ""
	}
	return fmt.Sprintf(" (exit code %d)", exitCode)
}

// ptr returns a pointer to the given value.
func ptr[T any](value T) *T {
	return &value
}
