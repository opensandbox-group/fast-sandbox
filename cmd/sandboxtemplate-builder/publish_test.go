package main

import (
	"fast-sandbox/internal/artifacts"

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
	if got, want := artifacts.ImageIndexKey(image), hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("ImageIndexKey(%q) = %s, want %s", image, got, want)
	}
	if got := artifacts.ImageIndexKey(image); len(got) != sha256.Size*2 {
		t.Fatalf("ImageIndexKey length = %d, want %d", len(got), sha256.Size*2)
	}
}

// TestImageIndexPayload verifies the index document shape: manifestRef,
// artifactDigest, and an RFC3339 updatedAt timestamp.
func TestImageIndexPayload(t *testing.T) {
	const (
		image          = "registry.example.com/sandbox:v1.0.21"
		manifestURI    = "s3://bucket/sandbox-images/0123456789abcdef/manifest.json"
		artifactDigest = "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	)
	payload, err := imageIndexPayload(image, manifestURI, artifactDigest)
	if err != nil {
		t.Fatalf("imageIndexPayload: %v", err)
	}
	var document struct {
		Image          string `json:"image"`
		ManifestRef    string `json:"manifestRef"`
		ArtifactDigest string `json:"artifactDigest"`
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
			"deadbeef", "s3://bucket/sandbox-images", "")
		if err == nil {
			t.Fatalf("expected an error for empty image reference %q", image)
		}
		if !strings.Contains(err.Error(), "image reference is required") {
			t.Fatalf("error %q does not mention the required guard", err)
		}
	}
}

// TestIndexKeys verifies which index keys a build publishes under: the raw
// image reference always, plus the exact spec.indexKey identity when set
// (deduplicated and whitespace-trimmed).
func TestIndexKeys(t *testing.T) {
	const image = "alpine:3.19"
	if got := indexKeys(image, ""); len(got) != 1 || got[0] != image {
		t.Fatalf("indexKeys(%q, empty) = %v, want [%q]", image, got, image)
	}
	if got := indexKeys(image, "   "); len(got) != 1 || got[0] != image {
		t.Fatalf("indexKeys(%q, blank) = %v, want [%q]", image, got, image)
	}
	if got := indexKeys(image, image); len(got) != 1 {
		t.Fatalf("indexKeys with key == image must deduplicate, got %v", got)
	}
	got := indexKeys(image, " tpl-796f5f33-1183 ")
	if len(got) != 2 || got[0] != image || got[1] != "tpl-796f5f33-1183" {
		t.Fatalf("indexKeys = %v, want [%q tpl-796f5f33-1183]", got, image)
	}
}

// TestImageIndexPayloadMatchesKey verifies the per-key identity contract:
// the payload written under a key carries that key in its image field, so
// pull-side byte matching accepts requests by either the source image or
// the exact template key.
func TestImageIndexPayloadMatchesKey(t *testing.T) {
	for _, key := range []string{"alpine:3.19", "tpl-796f5f33-1183"} {
		payload, err := imageIndexPayload(key, "s3://b/m", "digest")
		if err != nil {
			t.Fatalf("imageIndexPayload(%q): %v", key, err)
		}
		var document struct {
			Image string `json:"image"`
		}
		if err := json.Unmarshal(payload, &document); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if document.Image != key {
			t.Fatalf("payload image = %q, want the index key %q", document.Image, key)
		}
	}
}
