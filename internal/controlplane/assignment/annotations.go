package assignment

import (
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const (
	// SnapshotCleanupFinalizer protects published OCI artifacts until deletion succeeds.
	SnapshotCleanupFinalizer = "sandbox.fast.io/snapshot-cleanup"
	// AnnotationSnapshotPoolRef pins the credential scope independently of the source Sandbox.
	AnnotationSnapshotPoolRef = "sandbox.fast.io/snapshot-pool-ref"
	// LabelCreatedBy records which component created the sandbox.
	LabelCreatedBy = "sandbox.fast.io/created-by"
	// AnnotationRequestID stores the FastPath Create idempotency key.
	AnnotationRequestID = "sandbox.fast.io/request-id"
	// AnnotationCreateSpecHash binds a request ID to its immutable create intent.
	AnnotationCreateSpecHash = "sandbox.fast.io/create-spec-hash"
	// AnnotationResourcePolicyHash identifies the Pool policy used to derive
	// the diagnostic request projection.
	AnnotationResourcePolicyHash    = "fast-sandbox.io/resource-policy-hash"
	AnnotationResolvedRequestCPU    = "fast-sandbox.io/resolved-request-cpu"
	AnnotationResolvedRequestMemory = "fast-sandbox.io/resolved-request-memory"
	AnnotationResolvedLimitCPU      = "fast-sandbox.io/resolved-limit-cpu"
	AnnotationResolvedLimitMemory   = "fast-sandbox.io/resolved-limit-memory"
)

// ResolvedResourceAnnotations returns the canonical diagnostic projection of
// one resolved ResourceAware request. The immutable assignment remains the
// execution contract; these annotations make the derived values inspectable.
func ResolvedResourceAnnotations(policyHash string, resources *fastletapi.ResolvedSandboxResources) map[string]string {
	if resources == nil {
		return nil
	}
	return map[string]string{
		AnnotationResourcePolicyHash:    policyHash,
		AnnotationResolvedRequestCPU:    resources.Requests.CPU,
		AnnotationResolvedRequestMemory: resources.Requests.Memory,
		AnnotationResolvedLimitCPU:      resources.Limits.CPU,
		AnnotationResolvedLimitMemory:   resources.Limits.Memory,
	}
}

// ProjectResolvedResourceAnnotations applies the canonical diagnostic
// projection before a new Sandbox is persisted.
func ProjectResolvedResourceAnnotations(sandbox *apiv1alpha2.Sandbox, policyHash string, resources *fastletapi.ResolvedSandboxResources) {
	if sandbox == nil {
		return
	}
	annotations := ResolvedResourceAnnotations(policyHash, resources)
	if len(annotations) == 0 {
		return
	}
	if sandbox.Annotations == nil {
		sandbox.Annotations = make(map[string]string, len(annotations))
	}
	for name, value := range annotations {
		sandbox.Annotations[name] = value
	}
}
