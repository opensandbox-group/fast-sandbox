package agent

// The image reference index resolves a published
// build from the image reference alone: <store>/index/<sha256(image)>.json
// points at the latest complete artifact set.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// imageIndex is the published image reference index document.
type imageIndex struct {
	Image          string `json:"image"`
	ManifestRef    string `json:"manifestRef"`
	ArtifactDigest string `json:"artifactDigest"`
	UpdatedAt      string `json:"updatedAt"`
}

// indexKey returns the store-relative key of the image reference index. The
// derivation matches the publisher's imageIndexKey and the driver's cache
// key, so the addressing chain needs no control-plane coordination.
func indexKey(image string) string {
	return "index/" + imageKey(image) + ".json"
}

// maxIndexBytes caps the downloaded image index document. Indexes are tiny
// (four fields); a larger document is never legitimate, so it is rejected
// instead of being read unbounded.
const maxIndexBytes = 1 << 20

// fetchIndex downloads and validates the image reference index. A missing
// index surfaces as ErrObjectNotFound so the caller can map it to
// ErrImageNotReady.
func fetchIndex(ctx context.Context, s3 *s3Client, image string) (imageIndex, error) {
	body, err := s3.get(ctx, indexKey(image))
	if err != nil {
		return imageIndex{}, err
	}
	defer body.Close()
	payload, err := io.ReadAll(io.LimitReader(body, maxIndexBytes+1))
	if err != nil {
		return imageIndex{}, fmt.Errorf("read image index for %q: %w", image, err)
	}
	if len(payload) > maxIndexBytes {
		return imageIndex{}, fmt.Errorf("image index for %q exceeds the %d-byte limit", image, maxIndexBytes)
	}
	var index imageIndex
	if err := json.Unmarshal(payload, &index); err != nil {
		return imageIndex{}, fmt.Errorf("decode image index for %q: %w", image, err)
	}
	if index.ManifestRef == "" || index.ArtifactDigest == "" {
		return imageIndex{}, fmt.Errorf("image index for %q is incomplete: manifestRef and artifactDigest are required", image)
	}
	return index, nil
}
