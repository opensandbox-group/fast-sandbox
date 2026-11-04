package sandbox

// Internal Fastlet Sandbox lifecycle phases stored in SandboxMetadata.Phase.
// They are Fastlet-local state-machine states, distinct from the API-level
// states in api/v1alpha2.
const (
	sandboxStateCreating            = "creating"
	sandboxStateDeleting            = "deleting"
	sandboxStateActionPending       = "action-pending"
	sandboxStateActionUnavailable   = "action-unavailable"
	sandboxStateCreateFailed        = "create-failed"
	sandboxStateCreateCleanup       = "create-cleanup"
	sandboxStateCreateCleanupFailed = "create-cleanup-failed"
	sandboxStateDeleteFailed        = "delete-failed"
	// sandboxStateRuntimeFailureCleanup retains admission while an unexpected
	// exit is durably recorded but its physical resources are still reclaiming.
	sandboxStateRuntimeFailureCleanup = "runtime-failure-cleanup"
	// sandboxStateRuntimeFailed retains only the terminal identity; runtime
	// resources no longer consume admission capacity.
	sandboxStateRuntimeFailed = "runtime-failed"
)

const (
	// sandboxStateImagePending parks a cold Sandbox while its image or
	// checkpoint artifacts are delivered asynchronously.
	sandboxStateImagePending = "image-pending"
	// sandboxStateInfraPending parks a runtime-ready Sandbox before Infra
	// Component initialization starts.
	sandboxStateInfraPending = "infra-pending"
	// sandboxStateInitializingInfra is the transient Infra initialization attempt.
	sandboxStateInitializingInfra = "initializing-infra"
	// sandboxStateInfraUnavailable marks a failed Infra initialization or health probe.
	sandboxStateInfraUnavailable = "infra-unavailable"
	// sandboxStateRoutePending parks a Sandbox waiting for proxy route publication.
	sandboxStateRoutePending = "route-pending"
	// sandboxStatePublishingRoute is the transient route publication attempt.
	sandboxStatePublishingRoute = "publishing-route"
	// sandboxStateRouteUnavailable marks a failed route publication.
	sandboxStateRouteUnavailable = "route-unavailable"
	// sandboxStateRunning is the terminal ready phase of the lifecycle.
	sandboxStateRunning = "running"
	// sandboxStateTerminating fences new work after a delete is accepted.
	sandboxStateTerminating = "terminating"
	// sandboxStateUnknown covers recovered Sandboxes recorded without a phase.
	sandboxStateUnknown = "unknown"
)

// runtimeCreatedPhase is independent of Infra, route, and Action readiness.
// A confirmed runtime exit can take ownership of any of these phases.
func runtimeCreatedPhase(phase string) bool {
	switch phase {
	case sandboxStateInfraPending, sandboxStateInitializingInfra, sandboxStateInfraUnavailable,
		sandboxStateRoutePending, sandboxStatePublishingRoute, sandboxStateRouteUnavailable,
		sandboxStateActionPending, sandboxStateActionUnavailable, sandboxStateRunning:
		return true
	default:
		return false
	}
}
