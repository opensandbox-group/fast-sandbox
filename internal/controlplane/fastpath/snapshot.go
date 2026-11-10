package fastpath

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/observability"
	"fast-sandbox/internal/ocitemplate"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// templateNamePattern mirrors the SandboxSnapshot CRD templateName validation
// (itself the SandboxTemplate indexKey pattern).
var templateNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(/[a-zA-Z0-9._-]+)*(:[a-zA-Z0-9._-]{1,127})?$`)

func validateTemplateName(name string) error {
	if name == "" {
		return errors.New("template_name is required")
	}
	if len(name) > 255 || !templateNamePattern.MatchString(name) {
		return fmt.Errorf("template_name %q is invalid", name)
	}
	return nil
}

// validateSnapshotMode accepts the defined proto enum values only. proto3
// preserves unrecognized enum numbers, so an out-of-band value must be
// rejected before it reaches the hash or the persisted intent.
func validateSnapshotMode(mode fastpathv2.SnapshotMode) error {
	switch mode {
	case fastpathv2.SnapshotMode_SNAPSHOT_MODE_UNSPECIFIED,
		fastpathv2.SnapshotMode_SNAPSHOT_MODE_FULL,
		fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY:
		return nil
	default:
		return fmt.Errorf("unknown snapshot mode %d", int32(mode))
	}
}

// snapshotSpecMode resolves the proto mode onto the CRD spec value: only
// ROOT_FS_ONLY changes the persisted intent; UNSPECIFIED and FULL both mean
// Full.
func snapshotSpecMode(mode fastpathv2.SnapshotMode) apiv1alpha2.SandboxSnapshotMode {
	if mode == fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY {
		return apiv1alpha2.SandboxSnapshotModeRootfsOnly
	}
	return apiv1alpha2.SandboxSnapshotModeFull
}

// protoSnapshotMode maps the CRD mode back onto the proto enum. The status
// mode is the fastlet-confirmed artifact class; an absent value is a legacy
// Full snapshot.
func protoSnapshotMode(mode apiv1alpha2.SandboxSnapshotMode) fastpathv2.SnapshotMode {
	if apiv1alpha2.NormalizeSandboxSnapshotMode(mode) == apiv1alpha2.SandboxSnapshotModeRootfsOnly {
		return fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY
	}
	return fastpathv2.SnapshotMode_SNAPSHOT_MODE_FULL
}

// SnapshotSpecHash returns a deterministic digest of the immutable snapshot
// intent. The transport-only request_id is excluded from the identity. The
// caller must have normalized the namespace (CreateSandboxSnapshot rewrites
// an omitted namespace to the server default before hashing, mirroring
// CreateSandbox), so an omitted-namespace request and its explicit replay
// hash identically. Mode is normalized the same way: UNSPECIFIED (absent)
// and explicit FULL are the same effective intent, so both collapse to
// UNSPECIFIED before hashing — the field is then omitted from the
// marshaled bytes and every Full request, including replays of pre-mode
// requests, keeps its historical hash.
func SnapshotSpecHash(request *fastpathv2.CreateSandboxSnapshotRequest) (string, error) {
	if request == nil {
		return "", errors.New("snapshot request is required")
	}
	normalized := proto.Clone(request).(*fastpathv2.CreateSandboxSnapshotRequest)
	normalized.RequestId = ""
	if normalized.Mode == fastpathv2.SnapshotMode_SNAPSHOT_MODE_FULL {
		normalized.Mode = fastpathv2.SnapshotMode_SNAPSHOT_MODE_UNSPECIFIED
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(normalized)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func snapshotFromCreateRequest(request *fastpathv2.CreateSandboxSnapshotRequest, sandbox *apiv1alpha2.Sandbox, specHash string) *apiv1alpha2.SandboxSnapshot {
	labels := map[string]string{assignment.LabelCreatedBy: "fastpath"}
	for name, value := range request.Metadata {
		labels[metadataLabelKey(name)] = value
	}
	return &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: request.RequestId, Namespace: sandbox.Namespace, Labels: labels,
			Finalizers:  []string{assignment.SnapshotCleanupFinalizer},
			Annotations: map[string]string{assignment.AnnotationRequestID: request.RequestId, assignment.AnnotationCreateSpecHash: specHash, assignment.AnnotationSnapshotPoolRef: sandbox.Spec.PoolRef},
		},
		Spec: apiv1alpha2.SandboxSnapshotSpec{
			SandboxRef: apiv1alpha2.SandboxRef{
				Name: sandbox.Name, Namespace: sandbox.Namespace, UID: sandbox.UID,
			},
			TemplateName: request.TemplateName,
			Mode:         snapshotSpecMode(request.Mode),
		},
	}
}

func (s *Server) CreateSandboxSnapshot(ctx context.Context, request *fastpathv2.CreateSandboxSnapshotRequest) (*fastpathv2.CreateSandboxSnapshotResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := ValidateRequestID(request.RequestId); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if request.TemplateName != "" {
		if err := validateTemplateName(request.TemplateName); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	if err := validateSnapshotMode(request.Mode); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateMetadata(request.Metadata); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// Normalize the namespace on the request itself (CreateSandbox pattern)
	// so the spec hash of an omitted-namespace request matches its explicit
	// replay under a configured default namespace.
	if request.Sandbox != nil && request.Sandbox.NamespacedName != nil && request.Sandbox.NamespacedName.Namespace == "" {
		request.Sandbox.NamespacedName.Namespace = s.defaultNamespace()
	}
	sandbox, err := s.sandboxFromReference(ctx, request.Sandbox)
	if err != nil {
		return nil, err
	}
	if request.TemplateName == "" && !strings.HasPrefix(sandbox.Spec.Image, ocitemplate.Prefix) {
		return nil, status.Error(codes.InvalidArgument, "template_name is required for a native snapshot")
	}
	// Rootfs-only artifacts need the OverlayBD path: reject before any
	// intent is persisted, matching the fail-fast style of the checks
	// above. The Fastlet re-checks the effective source authoritatively.
	if request.Mode == fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY && !strings.HasPrefix(sandbox.Spec.Image, ocitemplate.Prefix) {
		return nil, status.Error(codes.InvalidArgument, "mode ROOT_FS_ONLY requires a Sandbox on the OCI artifact path")
	}
	ctx = observability.WithIdentity(ctx, observability.Identity{
		RequestID: request.RequestId, Namespace: sandbox.Namespace, SandboxName: sandbox.Name, SandboxUID: string(sandbox.UID),
	})
	if sandbox.Status.Runtime.State != apiv1alpha2.RuntimeReady {
		// The CR status is a lagging projection: a Sandbox created with
		// completion=READY can be serving while the reconciler has not
		// written Ready yet. Confirm against the assigned fastlet (the
		// authoritative runtime observation) before rejecting.
		live, _, _, inspectErr := s.inspectAssignedSandbox(ctx, sandbox)
		if inspectErr != nil || live == nil || live.Runtime == nil || live.Runtime.State != fastpathv2.RuntimeState_RUNTIME_STATE_READY {
			return nil, status.Errorf(codes.FailedPrecondition, "Sandbox runtime is %q; snapshot requires a Ready Sandbox", sandbox.Status.Runtime.State)
		}
	}
	specHash, err := SnapshotSpecHash(request)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "hash snapshot request: %v", err)
	}
	if err := s.checkSnapshotReentrancy(ctx, sandbox, request.TemplateName, snapshotSelfKey(sandbox.Namespace, request.RequestId)); err != nil {
		return nil, err
	}
	snapshot := snapshotFromCreateRequest(request, sandbox, specHash)
	snapshot, err = s.acceptSnapshotIntent(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	return s.triggerSnapshot(ctx, snapshot, sandbox)
}

// checkSnapshotReentrancy rejects the request while any non-terminal snapshot
// holds the same Sandbox or the same template name. The template-name check
// is cluster-wide because the published image index is a global key
// (index/<sha256(templateName)>.json): two namespaces snapshotting to one
// name would silently overwrite each other's artifacts. This is a UX
// pre-check only — the authoritative Sandbox fence lives on the Fastlet and
// the cache may lag a concurrent create — so a residual cross-namespace race
// degrades to last-writer-wins at publish time rather than breaking
// correctness of a single snapshot.
func (s *Server) checkSnapshotReentrancy(ctx context.Context, sandbox *apiv1alpha2.Sandbox, templateName, selfKey string) error {
	reader := s.K8sClient
	if s.RouteCache != nil {
		reader = s.RouteCache
	}
	var list apiv1alpha2.SandboxSnapshotList
	err := reader.List(ctx, &list)
	if err != nil && reader != s.K8sClient {
		err = s.K8sClient.List(ctx, &list)
	}
	if err != nil {
		return grpcKubernetesError(err)
	}
	for index := range list.Items {
		item := &list.Items[index]
		if snapshotSelfKey(item.Namespace, item.Name) == selfKey || item.Status.Phase.Terminal() {
			continue
		}
		sandboxMatch := item.Spec.SandboxRef.Namespace == sandbox.Namespace &&
			item.Spec.SandboxRef.Name == sandbox.Name && item.Spec.SandboxRef.UID == sandbox.UID
		// The sandbox fence covers only the pause window: a snapshot in
		// Publishing no longer touches the VM and does not block the next
		// snapshot of this Sandbox (the template check below still applies
		// to it).
		if sandboxMatch && item.Status.Phase != apiv1alpha2.SandboxSnapshotPhasePublishing {
			return status.Errorf(codes.FailedPrecondition, "Sandbox already has snapshot %q in phase %q; retry once it reaches Publishing", item.Name, item.Status.Phase)
		}
		// The template-name fence holds to terminal: concurrent publishers
		// of one index key would race last-writer-wins on the store.
		if templateName != "" && item.Spec.TemplateName == templateName {
			return status.Errorf(codes.FailedPrecondition, "template name %q is held by snapshot %s/%s in phase %q; retry after it terminates", templateName, item.Namespace, item.Name, item.Status.Phase)
		}
	}
	return nil
}

func snapshotSelfKey(namespace, name string) string { return namespace + "/" + name }

// acceptSnapshotIntent persists the snapshot intent idempotently: an
// AlreadyExists with the same request-id and spec hash replays the persisted
// object, anything else is a conflict.
func (s *Server) acceptSnapshotIntent(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot) (*apiv1alpha2.SandboxSnapshot, error) {
	createErr := s.K8sClient.Create(ctx, snapshot)
	if createErr == nil {
		// A durable intent write: operators must be able to see that a
		// snapshot object now exists even if the trigger later fails.
		klog.FromContext(ctx).Info("snapshot intent created", "snapshot", snapshot.Name)
		return snapshot, nil
	} else if !apierrors.IsAlreadyExists(createErr) {
		return nil, grpcKubernetesError(createErr)
	}
	var existing apiv1alpha2.SandboxSnapshot
	getErr := s.K8sClient.Get(ctx, client.ObjectKeyFromObject(snapshot), &existing)
	if getErr != nil {
		return nil, grpcKubernetesError(errors.Join(createErr, getErr))
	}
	if existing.Annotations[assignment.AnnotationRequestID] != snapshot.Annotations[assignment.AnnotationRequestID] ||
		existing.Annotations[assignment.AnnotationCreateSpecHash] != snapshot.Annotations[assignment.AnnotationCreateSpecHash] {
		return nil, status.Errorf(codes.AlreadyExists, "SandboxSnapshot name %q belongs to another snapshot intent", snapshot.Name)
	}
	return existing.DeepCopy(), nil
}

// triggerSnapshot performs the direct Fastlet call after the intent is
// persisted. Deterministic rejections mark the object Failed; unknown or
// unavailable outcomes keep the intent Pending for the Controller.
func (s *Server) triggerSnapshot(ctx context.Context, snapshot *apiv1alpha2.SandboxSnapshot, sandbox *apiv1alpha2.Sandbox) (*fastpathv2.CreateSandboxSnapshotResponse, error) {
	orchestrator, err := s.orchestrator()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	response := &fastpathv2.CreateSandboxSnapshotResponse{Snapshot: snapshotInfo(snapshot)}
	if snapshot.DeletionTimestamp != nil {
		return nil, status.Error(codes.FailedPrecondition, "SandboxSnapshot is being deleted")
	}
	if snapshot.Status.Phase.Terminal() {
		// One-shot semantics: a replayed request never re-triggers a
		// terminated snapshot.
		return response, nil
	}
	observed, callErr := orchestrator.CreateSnapshot(ctx, snapshot, sandbox)
	// A missing envelope only means the Sandbox has no durable placement
	// right now: the trigger failed transitively, and the response still
	// reports a meaningful Pending instead of an unspecified phase.
	envelope, envelopeErr := assignment.EffectiveAssignment(sandbox)
	if envelopeErr != nil {
		klog.FromContext(ctx).Error(envelopeErr, "Failed to read effective assignment while projecting snapshot trigger status", "sandbox", sandbox.Name)
	}
	updated, patchErr := s.patchSnapshotStatus(ctx, client.ObjectKeyFromObject(snapshot), func(status *apiv1alpha2.SandboxSnapshotStatus) {
		if envelope != nil {
			status.FastletName = envelope.FastletName
			status.FastletPodUID = types.UID(envelope.FastletPodUID)
			// Pin the trigger placement once so Controller observations stay
			// on this fastlet across a later Sandbox reassignment.
			if status.Triggered == nil {
				status.Triggered = &apiv1alpha2.SnapshotTrigger{
					FastletName: envelope.FastletName, FastletPodUID: envelope.FastletPodUID,
					RuntimeInstanceID:  envelope.RuntimeInstanceID,
					InstanceGeneration: envelope.InstanceGeneration, AssignmentAttempt: envelope.Attempt,
				}
			}
		}
		if observed != nil {
			orchestration.ProjectSnapshotStatus(status, snapshot.Spec.Mode, observed)
		} else if status.Phase == "" {
			status.Phase = apiv1alpha2.SandboxSnapshotPhasePending
			status.Message = "SandboxSnapshot intent is persisted and will be triggered by the Controller"
		}
	})
	if patchErr != nil {
		klog.FromContext(ctx).Error(patchErr, "Patch initial SandboxSnapshot status", "snapshot", snapshot.Name)
	} else {
		snapshot = updated
		response.Snapshot = snapshotInfo(snapshot)
	}
	if callErr != nil {
		if code, message, deterministic := snapshotRejection(callErr); deterministic {
			if _, patchErr := s.patchSnapshotStatus(ctx, client.ObjectKeyFromObject(snapshot), func(status *apiv1alpha2.SandboxSnapshotStatus) {
				now := metav1.Now()
				status.Phase = apiv1alpha2.SandboxSnapshotPhaseFailed
				status.Message = message
				if status.CompletedAt == nil {
					status.CompletedAt = &now
				}
			}); patchErr != nil {
				klog.FromContext(ctx).Error(patchErr, "Mark SandboxSnapshot Failed", "snapshot", snapshot.Name)
			}
			return nil, status.Error(code, message)
		}
		klog.FromContext(ctx).Error(callErr, "SandboxSnapshot intent persisted; Controller will trigger", "snapshot", snapshot.Name)
	}
	return response, nil
}

// snapshotRejection classifies a Fastlet snapshot trigger failure. A
// deterministic rejection terminates the snapshot (Failed) — including
// ErrorSnapshotInProgress, the Fastlet's non-reentrancy guard, which the
// client is expected to surface rather than re-trigger blindly. Everything
// else — the transient ErrorDraining state a rolling or restarting Fastlet
// reports, transport failures, and unclassified errors — is retried by the
// Controller.
func snapshotRejection(err error) (codes.Code, string, bool) {
	var failure *fastletapi.FastletError
	if !errors.As(err, &failure) {
		return codes.Unavailable, err.Error(), false
	}
	switch failure.Code {
	case fastletapi.ErrorSnapshotInProgress:
		return codes.FailedPrecondition, failure.Error(), true
	case fastletapi.ErrorSnapshotUnsupported:
		return codes.Unimplemented, failure.Error(), true
	case fastletapi.ErrorNotFound:
		return codes.NotFound, failure.Error(), true
	case fastletapi.ErrorConflict, fastletapi.ErrorStaleAssignment, fastletapi.ErrorStaleGeneration, fastletapi.ErrorGenerationFenced:
		return codes.Aborted, failure.Error(), true
	default:
		return codes.Unavailable, failure.Error(), false
	}
}

func (s *Server) GetSandboxSnapshot(ctx context.Context, request *fastpathv2.GetSandboxSnapshotRequest) (*fastpathv2.GetSandboxSnapshotResponse, error) {
	if request == nil || request.Snapshot == nil || request.Snapshot.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot namespaced name is required")
	}
	key := client.ObjectKey{Namespace: request.Snapshot.Namespace, Name: request.Snapshot.Name}
	if key.Namespace == "" {
		key.Namespace = s.defaultNamespace()
	}
	snapshot, err := s.snapshotByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if request.ExpectedUid != "" && string(snapshot.UID) != request.ExpectedUid {
		return nil, status.Errorf(codes.Aborted, "SandboxSnapshot UID changed: expected %s, current %s", request.ExpectedUid, snapshot.UID)
	}
	return &fastpathv2.GetSandboxSnapshotResponse{Snapshot: snapshotInfo(snapshot)}, nil
}

func (s *Server) DeleteSandboxSnapshot(ctx context.Context, request *fastpathv2.DeleteSandboxSnapshotRequest) (*fastpathv2.DeleteSandboxSnapshotResponse, error) {
	if request == nil || request.Snapshot == nil || request.Snapshot.NamespacedName == nil || request.Snapshot.NamespacedName.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot reference is required")
	}
	key := client.ObjectKey{
		Namespace: request.Snapshot.NamespacedName.Namespace, Name: request.Snapshot.NamespacedName.Name,
	}
	if key.Namespace == "" {
		key.Namespace = s.defaultNamespace()
	}
	snapshot, err := s.snapshotByKey(ctx, key)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return &fastpathv2.DeleteSandboxSnapshotResponse{}, nil
		}
		return nil, err
	}
	if request.Snapshot.ExpectedUid != "" && string(snapshot.UID) != request.Snapshot.ExpectedUid {
		return nil, status.Errorf(codes.Aborted, "SandboxSnapshot UID changed: expected %s, current %s", request.Snapshot.ExpectedUid, snapshot.UID)
	}
	uid := snapshot.UID
	if err := s.K8sClient.Delete(ctx, snapshot, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return nil, grpcKubernetesError(err)
	}
	klog.FromContext(ctx).Info("fastpath sandbox snapshot deleted", "snapshot", snapshot.Name)
	return &fastpathv2.DeleteSandboxSnapshotResponse{}, nil
}

func (s *Server) snapshotByKey(ctx context.Context, key client.ObjectKey) (*apiv1alpha2.SandboxSnapshot, error) {
	if s.RouteCache != nil {
		var cached apiv1alpha2.SandboxSnapshot
		if err := s.RouteCache.Get(ctx, key, &cached); err == nil {
			return &cached, nil
		} else if !apierrors.IsNotFound(err) {
			return nil, grpcKubernetesError(err)
		}
	}
	var snapshot apiv1alpha2.SandboxSnapshot
	if err := s.K8sClient.Get(ctx, key, &snapshot); err != nil {
		return nil, grpcKubernetesError(err)
	}
	return &snapshot, nil
}

func (s *Server) patchSnapshotStatus(ctx context.Context, key client.ObjectKey, mutate func(*apiv1alpha2.SandboxSnapshotStatus)) (*apiv1alpha2.SandboxSnapshot, error) {
	var result *apiv1alpha2.SandboxSnapshot
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current apiv1alpha2.SandboxSnapshot
		if err := s.K8sClient.Get(ctx, key, &current); err != nil {
			return err
		}
		before := current.DeepCopy().Status
		// Terminal phases are monotonic: after a conflict-triggered re-read,
		// applying an older in-flight observation would roll a terminal
		// object back to Creating while the Controller has already stopped.
		if before.Phase.Terminal() {
			result = current.DeepCopy()
			return nil
		}
		mutate(&current.Status)
		if reflect.DeepEqual(before, current.Status) {
			result = current.DeepCopy()
			return nil
		}
		if err := s.K8sClient.Status().Update(ctx, &current); err != nil {
			return err
		}
		result = current.DeepCopy()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func snapshotInfo(snapshot *apiv1alpha2.SandboxSnapshot) *fastpathv2.SandboxSnapshotInfo {
	info := &fastpathv2.SandboxSnapshotInfo{
		Identity: &fastpathv2.SandboxIdentity{
			Uid: string(snapshot.UID), Name: snapshot.Name, Namespace: snapshot.Namespace,
		},
		SandboxName:    snapshot.Spec.SandboxRef.Name,
		SandboxUid:     string(snapshot.Spec.SandboxRef.UID),
		TemplateName:   snapshot.Spec.TemplateName,
		Mode:           protoSnapshotMode(snapshot.Status.Mode),
		Phase:          protoSnapshotPhase(snapshot.Status.Phase),
		Message:        snapshot.Status.Message,
		FastletName:    snapshot.Status.FastletName,
		ManifestRef:    snapshot.Status.ManifestRef,
		ArtifactDigest: snapshot.Status.ArtifactDigest,
		SizeBytes:      snapshot.Status.SizeBytes,
		SnapshotId:     snapshot.Status.SnapshotID,
	}
	if !snapshot.CreationTimestamp.IsZero() {
		info.CreatedUnixSeconds = snapshot.CreationTimestamp.Unix()
	}
	if snapshot.Status.CompletedAt != nil {
		info.CompletedUnixSeconds = snapshot.Status.CompletedAt.Unix()
	}
	return info
}

func protoSnapshotPhase(phase apiv1alpha2.SandboxSnapshotPhase) fastpathv2.SnapshotPhase {
	switch phase {
	case apiv1alpha2.SandboxSnapshotPhasePending:
		return fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_PENDING
	case apiv1alpha2.SandboxSnapshotPhaseCreating:
		return fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_CREATING
	case apiv1alpha2.SandboxSnapshotPhasePublishing:
		return fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_PUBLISHING
	case apiv1alpha2.SandboxSnapshotPhaseSucceeded:
		return fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_SUCCEEDED
	case apiv1alpha2.SandboxSnapshotPhaseFailed:
		return fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_FAILED
	default:
		return fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_UNSPECIFIED
	}
}
