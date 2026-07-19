package idgen

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateRequestID(t *testing.T) {
	first, err := GenerateRequestID()
	if err != nil {
		t.Fatalf("GenerateRequestID() error = %v", err)
	}
	second, err := GenerateRequestID()
	if err != nil {
		t.Fatalf("GenerateRequestID() error = %v", err)
	}
	if first == second {
		t.Fatalf("expected unique request IDs, both were %q", first)
	}
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !pattern.MatchString(first) {
		t.Fatalf("request ID %q is not UUIDv4-compatible", first)
	}
}

func TestGenerateHashID(t *testing.T) {
	name := "test-sb"
	namespace := "default"
	timestamp := int64(1234567890123456789)

	id := GenerateHashID(name, namespace, timestamp)

	// Should be 32 character hex string (md5)
	assert.Len(t, id, 32, "MD5 hash should be 32 characters")

	// Same inputs should produce same output
	id2 := GenerateHashID(name, namespace, timestamp)
	assert.Equal(t, id, id2, "Same inputs should produce same hash")

	// Different timestamp should produce different output
	id3 := GenerateHashID(name, namespace, timestamp+1)
	assert.NotEqual(t, id, id3, "Different timestamp should produce different hash")

	// Different namespace should produce different output
	id4 := GenerateHashID(name, "other", timestamp)
	assert.NotEqual(t, id, id4, "Different namespace should produce different hash")
}
