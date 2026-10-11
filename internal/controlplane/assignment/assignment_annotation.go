package assignment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

const (
	AnnotationAssignment      = "sandbox.fast.io/assignment"
	AssignmentEnvelopeVersion = "v1"
	// assignmentAnnotationJSONPath is AnnotationAssignment escaped for JSON
	// Patch bodies ("~1" encodes "/").
	assignmentAnnotationJSONPath = "/metadata/annotations/sandbox.fast.io~1assignment"
	// jsonPatchPathField is the JSON Patch (RFC 6902) member naming an operation's target path.
	jsonPatchPathField = "path"
	// jsonPatchTestOp is the JSON Patch precondition operation name.
	jsonPatchTestOp = "test"
	// jsonPatchValueField is the JSON Patch value member name.
	jsonPatchValueField = "value"
)

var (
	ErrAssignmentAnnotationMissing  = errors.New("assignment status exists without assignment annotation")
	ErrAssignmentProjectionConflict = errors.New("assignment annotation and status projection conflict")
	ErrAssignmentAnnotationChanged  = errors.New("assignment annotation changed")
)

// InitializeAssignmentAnnotation wins the first placement for an existing
// declarative Sandbox. FastPath does not use this helper because its initial
// assignment is included in the CRD Create itself.
func InitializeAssignmentAnnotation(
	ctx context.Context,
	k8sClient client.Client,
	key types.NamespacedName,
	desired AssignmentEnvelope,
) (*apiv1alpha2.Sandbox, bool, error) {
	value, err := EncodeAssignment(desired)
	if err != nil {
		return nil, false, err
	}
	var current apiv1alpha2.Sandbox
	if err := k8sClient.Get(ctx, key, &current); err != nil {
		return nil, false, err
	}
	existing, err := AssignmentFromAnnotation(&current)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return current.DeepCopy(), false, nil
	}
	patchBody, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"resourceVersion": current.ResourceVersion,
			"annotations":     map[string]string{AnnotationAssignment: value},
		},
	})
	if err != nil {
		return nil, false, err
	}
	target := &apiv1alpha2.Sandbox{}
	target.Namespace, target.Name = key.Namespace, key.Name
	if err := k8sClient.Patch(ctx, target, client.RawPatch(types.MergePatchType, patchBody)); err != nil {
		return nil, false, err
	}
	if err := k8sClient.Get(ctx, key, &current); err != nil {
		return nil, false, err
	}
	return current.DeepCopy(), true, nil
}

// AssignmentEnvelope is the durable assignment and runtime fence written in
// the same API-server Create as a FastPath Sandbox. It remains authoritative
// after the Controller projects placement fields into status.
type AssignmentEnvelope struct {
	Version             string `json:"version"`
	FastletName         string `json:"fastletName"`
	FastletPodUID       string `json:"fastletPodUID"`
	NodeName            string `json:"nodeName,omitempty"`
	Attempt             int64  `json:"attempt"`
	InstanceGeneration  int64  `json:"instanceGeneration"`
	RouteGeneration     int64  `json:"routeGeneration"`
	RuntimeInstanceID   string `json:"runtimeInstanceID"`
	RuntimeProfileHash  string `json:"runtimeProfileHash"`
	ResourceProfileHash string `json:"resourceProfileHash"`
	InfraRevision       string `json:"infraRevision"`
}

func (a AssignmentEnvelope) Validate() error {
	if a.Version != AssignmentEnvelopeVersion {
		return fmt.Errorf("unsupported assignment version %q", a.Version)
	}
	statusPlacement := a.StatusPlacement()
	if err := statusPlacement.Validate(); err != nil {
		return err
	}
	if a.InstanceGeneration < apiv1alpha2.InitialInstanceGeneration {
		return errors.New("instanceGeneration must be at least 1")
	}
	if a.RouteGeneration < 1 {
		return errors.New("routeGeneration must be at least 1")
	}
	if a.RuntimeInstanceID == "" {
		return errors.New("runtimeInstanceID is required")
	}
	if a.RuntimeProfileHash == "" {
		return errors.New("runtimeProfileHash is required")
	}
	if a.ResourceProfileHash == "" {
		return errors.New("resourceProfileHash is required")
	}
	if a.InfraRevision == "" {
		return errors.New("infraRevision is required")
	}
	return nil
}

func (a AssignmentEnvelope) StatusPlacement() apiv1alpha2.PlacementStatus {
	return apiv1alpha2.PlacementStatus{
		FastletName: a.FastletName, FastletPodUID: types.UID(a.FastletPodUID),
		Attempt: a.Attempt,
	}
}

func EncodeAssignment(envelope AssignmentEnvelope) (string, error) {
	if err := envelope.Validate(); err != nil {
		return "", fmt.Errorf("invalid assignment annotation: %w", err)
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("encode assignment annotation: %w", err)
	}
	return string(payload), nil
}

func ParseAssignment(value string) (*AssignmentEnvelope, error) {
	if value == "" {
		return nil, nil //nolint:nilnil // empty annotation means no assignment exists; absence is optional, not an error
	}
	var envelope AssignmentEnvelope
	if err := json.Unmarshal([]byte(value), &envelope); err != nil {
		return nil, fmt.Errorf("decode assignment annotation: %w", err)
	}
	if err := envelope.Validate(); err != nil {
		return nil, fmt.Errorf("invalid assignment annotation: %w", err)
	}
	return &envelope, nil
}

func SetAssignmentAnnotation(sandbox *apiv1alpha2.Sandbox, envelope AssignmentEnvelope) error {
	if sandbox == nil {
		return errors.New("sandbox is required")
	}
	value, err := EncodeAssignment(envelope)
	if err != nil {
		return err
	}
	if sandbox.Annotations == nil {
		sandbox.Annotations = make(map[string]string)
	}
	sandbox.Annotations[AnnotationAssignment] = value
	return nil
}

func AssignmentFromAnnotation(sandbox *apiv1alpha2.Sandbox) (*AssignmentEnvelope, error) {
	if sandbox == nil {
		return nil, errors.New("sandbox is required")
	}
	value := ""
	if sandbox.Annotations != nil {
		value = sandbox.Annotations[AnnotationAssignment]
	}
	return ParseAssignment(value)
}

// EffectiveAssignment validates that status is only a projection of the
// durable annotation. A mismatch fails closed instead of silently selecting a
// second Fastlet.
func EffectiveAssignment(sandbox *apiv1alpha2.Sandbox) (*AssignmentEnvelope, error) {
	if sandbox == nil {
		return nil, errors.New("sandbox is required")
	}
	envelope, err := AssignmentFromAnnotation(sandbox)
	if err != nil {
		return nil, err
	}
	if envelope == nil {
		if sandbox.Status.Placement.FastletName != "" {
			return nil, ErrAssignmentAnnotationMissing
		}
		return nil, nil //nolint:nilnil // a Sandbox with no assignment annotation and empty placement is a valid unassigned state
	}
	if sandbox.Status.Placement.FastletName == "" {
		return envelope, nil
	}
	want := envelope.StatusPlacement()
	if !placementsEqual(sandbox.Status.Placement, want) ||
		sandbox.Status.Runtime.Generation != envelope.InstanceGeneration ||
		sandbox.Status.DataPlane.RouteGeneration != envelope.RouteGeneration {
		return nil, ErrAssignmentProjectionConflict
	}
	return envelope, nil
}

func assignmentEnvelopeEqual(left, right AssignmentEnvelope) bool {
	return left == right
}

// CASAssignmentAnnotation replaces exactly the expected assignment. The
// annotation value is the placement CAS field. Unrelated metadata/status
// writes (for example the Controller adding its finalizer) must not make a
// FastPath Top-K retry fail.
func CASAssignmentAnnotation(
	ctx context.Context,
	k8sClient client.Client,
	key types.NamespacedName,
	expected AssignmentEnvelope,
	next AssignmentEnvelope,
) (*apiv1alpha2.Sandbox, error) {
	if err := expected.Validate(); err != nil {
		return nil, fmt.Errorf("invalid expected assignment: %w", err)
	}
	nextValue, err := EncodeAssignment(next)
	if err != nil {
		return nil, err
	}
	var current apiv1alpha2.Sandbox
	if err := k8sClient.Get(ctx, key, &current); err != nil {
		return nil, err
	}
	// A changed authoritative annotation is not a delayed status projection.
	// Check the complete fence before reporting a projection conflict so a
	// caller waiting for status never waits on another assignment.
	annotated, err := AssignmentFromAnnotation(&current)
	if err != nil {
		return nil, err
	}
	if annotated != nil && !assignmentEnvelopeEqual(*annotated, expected) {
		return nil, ErrAssignmentAnnotationChanged
	}
	currentEnvelope, err := EffectiveAssignment(&current)
	if err != nil {
		return nil, fmt.Errorf("%w: annotation attempt=%d instanceGeneration=%d routeGeneration=%d; status fastlet=%s podUID=%s attempt=%d instanceGeneration=%d routeGeneration=%d",
			err, expected.Attempt, expected.InstanceGeneration, expected.RouteGeneration,
			current.Status.Placement.FastletName, current.Status.Placement.FastletPodUID,
			current.Status.Placement.Attempt, current.Status.Runtime.Generation, current.Status.DataPlane.RouteGeneration)
	}
	if currentEnvelope == nil || !assignmentEnvelopeEqual(*currentEnvelope, expected) {
		return nil, ErrAssignmentAnnotationChanged
	}
	currentValue := current.Annotations[AnnotationAssignment]
	patch, err := json.Marshal([]map[string]any{
		{"op": jsonPatchTestOp, jsonPatchPathField: assignmentAnnotationJSONPath, jsonPatchValueField: currentValue},
		{"op": "replace", jsonPatchPathField: assignmentAnnotationJSONPath, jsonPatchValueField: nextValue},
	})
	if err != nil {
		return nil, err
	}
	target := &apiv1alpha2.Sandbox{}
	target.Namespace, target.Name = key.Namespace, key.Name
	if err := k8sClient.Patch(ctx, target, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		return nil, err
	}
	if err := k8sClient.Get(ctx, key, &current); err != nil {
		return nil, err
	}
	return current.DeepCopy(), nil
}

// RemoveAssignmentAnnotation records the durable intent to stop using an
// assignment. Status clearing follows asynchronously; retries tolerate the
// annotation already being absent.
func RemoveAssignmentAnnotation(
	ctx context.Context,
	k8sClient client.Client,
	key types.NamespacedName,
	expected AssignmentEnvelope,
) (*apiv1alpha2.Sandbox, bool, error) {
	if err := expected.Validate(); err != nil {
		return nil, false, fmt.Errorf("invalid expected assignment: %w", err)
	}
	var current apiv1alpha2.Sandbox
	if err := k8sClient.Get(ctx, key, &current); err != nil {
		return nil, false, err
	}
	currentEnvelope, err := AssignmentFromAnnotation(&current)
	if err != nil {
		return nil, false, err
	}
	if currentEnvelope == nil {
		return current.DeepCopy(), false, nil
	}
	if !assignmentEnvelopeEqual(*currentEnvelope, expected) {
		return nil, false, ErrAssignmentAnnotationChanged
	}
	currentValue := current.Annotations[AnnotationAssignment]
	patch, err := json.Marshal([]map[string]any{
		{"op": jsonPatchTestOp, jsonPatchPathField: "/metadata/resourceVersion", jsonPatchValueField: current.ResourceVersion},
		{"op": jsonPatchTestOp, jsonPatchPathField: assignmentAnnotationJSONPath, jsonPatchValueField: currentValue},
		{"op": "remove", jsonPatchPathField: assignmentAnnotationJSONPath},
	})
	if err != nil {
		return nil, false, err
	}
	target := &apiv1alpha2.Sandbox{}
	target.Namespace, target.Name = key.Namespace, key.Name
	if err := k8sClient.Patch(ctx, target, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		return nil, false, err
	}
	if err := k8sClient.Get(ctx, key, &current); err != nil {
		return nil, false, err
	}
	return current.DeepCopy(), true, nil
}
