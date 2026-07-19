package fastpath

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"google.golang.org/protobuf/proto"
)

const maxRequestIDLength = 128

// requestIDLabelValue returns a compact DNS-label-safe lookup key. The full
// request ID remains in the annotation and is always compared after listing.
func requestIDLabelValue(requestID string) string {
	digest := sha256.Sum256([]byte(requestID))
	return hex.EncodeToString(digest[:16])
}

// ValidateRequestID validates the stable idempotency key accepted from an SDK
// or fastctl. Generation of a missing key belongs to clients; the transitional
// server may still synthesize one until all clients are migrated.
func ValidateRequestID(requestID string) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return errors.New("request_id is required")
	}
	if len(requestID) > maxRequestIDLength {
		return errors.New("request_id exceeds 128 bytes")
	}
	for _, r := range requestID {
		if r <= 0x20 || r == 0x7f {
			return errors.New("request_id contains whitespace or control characters")
		}
	}
	return nil
}

// CreateSpecHash returns a deterministic digest of the immutable Create
// intent. Transport-only request_id and deprecated Fast/Strong/host-port fields
// are deliberately excluded from the identity.
func CreateSpecHash(req *fastpathv1.CreateRequest) (string, error) {
	if req == nil {
		return "", errors.New("create request is required")
	}
	normalized := proto.Clone(req).(*fastpathv1.CreateRequest)
	normalized.RequestId = ""
	normalized.ConsistencyMode = fastpathv1.ConsistencyMode_FAST
	normalized.ExposedPorts = nil
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
