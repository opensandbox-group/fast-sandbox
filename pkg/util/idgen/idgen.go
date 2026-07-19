package idgen

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// GenerateHashID creates a sandboxID from name, namespace, and timestamp
// Returns a 32-character md5 hex string
func GenerateHashID(name, namespace string, timestamp int64) string {
	data := fmt.Sprintf("%s:%s:%d", name, namespace, timestamp)
	hash := md5.Sum([]byte(data))
	return hex.EncodeToString(hash[:])
}

// GenerateRequestID creates a UUIDv4-compatible idempotency key without
// relying on timestamps or process-local counters.
func GenerateRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate request ID: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
