package assignment

const (
	// LabelCreatedBy 标识 sandbox 的创建方式
	LabelCreatedBy = "sandbox.fast.io/created-by"
	// AnnotationRequestID stores the FastPath Create idempotency key.
	AnnotationRequestID = "sandbox.fast.io/request-id"
	// AnnotationCreateSpecHash binds a request ID to its immutable create intent.
	AnnotationCreateSpecHash = "sandbox.fast.io/create-spec-hash"
	// AnnotationSourceActionBindings records the source Sandbox's action
	// bindings on a SandboxSnapshot (verbatim JSON): creating a Sandbox
	// whose image resolves to that snapshot re-applies them, so checkpoint
	// and restore preserve runtime policy (e.g. egress network policy).
	// Explicit bindings on the create request win per handler.
	AnnotationSourceActionBindings = "sandbox.fast.io/source-action-bindings"
)
