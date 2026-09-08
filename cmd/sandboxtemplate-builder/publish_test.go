package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestImageIndexKeyMatchesConsumerCacheKey verifies the index key derivation
// matches the consumer-side cache key (sha256 of the image reference string),
// so the addressing chain Image -> index works without control-plane help.
func TestImageIndexKeyMatchesConsumerCacheKey(t *testing.T) {
	const image = "registry.example.com/sandbox:v1.0.21"
	digest := sha256.Sum256([]byte(image))
	if got, want := imageIndexKey(image), hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("imageIndexKey(%q) = %s, want %s", image, got, want)
	}
	if got := imageIndexKey(image); len(got) != sha256.Size*2 {
		t.Fatalf("imageIndexKey length = %d, want %d", len(got), sha256.Size*2)
	}
}

// TestImageIndexPayload verifies the index document shape: manifestRef,
// artifactDigest, the optional OCI image refs, and an RFC3339 updatedAt
// timestamp.
func TestImageIndexPayload(t *testing.T) {
	const (
		image          = "registry.example.com/sandbox:v1.0.21"
		manifestURI    = "s3://bucket/sandbox-images/0123456789abcdef/manifest.json"
		artifactDigest = "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	)
	refs := ociImageRefs{
		Rootfs: "registry.example.com/fs-templates/t1-rootfs:abc@sha256:aaaa",
		Memory: "registry.example.com/fs-templates/t1-mem:abc@sha256:bbbb",
	}
	payload, err := imageIndexPayload(image, manifestURI, artifactDigest, refs)
	if err != nil {
		t.Fatalf("imageIndexPayload: %v", err)
	}
	var document struct {
		Image          string `json:"image"`
		ManifestRef    string `json:"manifestRef"`
		ArtifactDigest string `json:"artifactDigest"`
		RootfsImageRef string `json:"rootfsImageRef"`
		MemoryImageRef string `json:"memoryImageRef"`
		UpdatedAt      string `json:"updatedAt"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	if document.Image != image {
		t.Fatalf("image = %q, want %q", document.Image, image)
	}
	if document.ManifestRef != manifestURI {
		t.Fatalf("manifestRef = %q, want %q", document.ManifestRef, manifestURI)
	}
	if document.ArtifactDigest != artifactDigest {
		t.Fatalf("artifactDigest = %q, want %q", document.ArtifactDigest, artifactDigest)
	}
	if document.RootfsImageRef != refs.Rootfs {
		t.Fatalf("rootfsImageRef = %q, want %q", document.RootfsImageRef, refs.Rootfs)
	}
	if document.MemoryImageRef != refs.Memory {
		t.Fatalf("memoryImageRef = %q, want %q", document.MemoryImageRef, refs.Memory)
	}
	if _, err := time.Parse(time.RFC3339, document.UpdatedAt); err != nil {
		t.Fatalf("updatedAt %q is not RFC3339: %v", document.UpdatedAt, err)
	}
}

// TestPublishImageIndexRejectsEmptyImage verifies the empty-reference guard:
// an empty image must not silently hash into a valid index key (the
// consumer side rejects empty images the same way).
func TestPublishImageIndexRejectsEmptyImage(t *testing.T) {
	for _, image := range []string{"", "   ", "\t\n"} {
		err := publishImageIndex(context.Background(), "aws", []string{"s3", "cp"}, image,
			"s3://bucket/sandbox-images/0123456789abcdef/manifest.json",
			"deadbeef", "s3://bucket/sandbox-images", ociImageRefs{})
		if err == nil {
			t.Fatalf("expected an error for empty image reference %q", image)
		}
		if !strings.Contains(err.Error(), "image reference is required") {
			t.Fatalf("error %q does not mention the required guard", err)
		}
	}
}
