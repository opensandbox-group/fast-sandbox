package common

const (
	// LabelCreatedBy 标识 sandbox 的创建方式
	LabelCreatedBy = "sandbox.fast.io/created-by"
	// AnnotationRequestID stores the FastPath Create idempotency key.
	AnnotationRequestID = "sandbox.fast.io/request-id"
	// AnnotationCreateSpecHash binds a request ID to its immutable create intent.
	AnnotationCreateSpecHash = "sandbox.fast.io/create-spec-hash"
)
