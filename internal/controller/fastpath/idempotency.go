package fastpath

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidateRequestID validates the single create identity. request_id is also
// metadata.name, so idempotency is a direct namespace/name lookup.
func ValidateRequestID(requestID string) error {
	if requestID == "" {
		return errors.New("request_id is required")
	}
	if problems := validation.IsDNS1123Subdomain(requestID); len(problems) > 0 {
		return errors.New("request_id must be a valid Kubernetes DNS subdomain: " + problems[0])
	}
	return nil
}

// CreateSpecHash returns a deterministic digest of the immutable Create
// intent. The transport-only request_id is excluded from the identity.
func CreateSpecHash(req *fastpathv1.CreateRequest) (string, error) {
	if req == nil {
		return "", errors.New("create request is required")
	}
	normalized := proto.Clone(req).(*fastpathv1.CreateRequest)
	normalized.RequestId = ""
	if normalized.Namespace == "" {
		normalized.Namespace = "default"
	}

	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(normalized)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}
