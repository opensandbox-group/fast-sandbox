package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// publish uploads the S3-side artifacts under a digest namespace and
// returns the manifest URI. Upload order guarantees consumers never observe
// a half-published artifact set: artifacts and SHA256SUMS first, then the
// manifest, and finally the image index that points at this build.
// On the OCI image path (imageRefs set) rootfs.ext4, memory.snap and the
// overlaybd/ layers live in the registry; only vmstate.snap and SHA256SUMS
// are uploaded. The legacy path keeps uploading the full artifact set with
// OverlayBD layers at their relative paths (overlaybd/rootfs/layer.lsmt,
// overlaybd/memory/layer.lsmt) — a flat basename would collide on the same
// S3 key.
func publish(ctx context.Context, spec apiv1alpha2.SandboxTemplateSpec, workdir string, manifestBytes []byte, imageRefs ociImageRefs) (string, error) {
	aws, err := exec.LookPath(awsBin)
	if err != nil {
		return "", fmt.Errorf("aws CLI not found: %w", err)
	}
	base := strings.TrimRight(spec.Output.Publish, "/") + "/" + sha256Of(manifestBytes)[:16]
	s3Files := []struct{ local, key string }{}
	if imageRefs.Rootfs == "" {
		s3Files = append(s3Files,
			struct{ local, key string }{filepath.Join(workdir, "rootfs.ext4"), "rootfs.ext4"},
			struct{ local, key string }{filepath.Join(workdir, "memory.snap"), "memory.snap"},
		)
		if layers, globErr := filepath.Glob(filepath.Join(workdir, "overlaybd", "*", "layer.lsmt")); globErr == nil {
			for _, layer := range layers {
				relative, relErr := filepath.Rel(workdir, layer)
				if relErr != nil {
					return "", relErr
				}
				s3Files = append(s3Files, struct{ local, key string }{layer, relative})
			}
		}
	}
	s3Files = append(s3Files,
		struct{ local, key string }{filepath.Join(workdir, "vmstate.snap"), "vmstate.snap"},
		// SHA256SUMS covers only the published artifact set; it belongs to
		// the immutable build directory and goes up with the artifacts.
		struct{ local, key string }{filepath.Join(workdir, "SHA256SUMS"), "SHA256SUMS"},
	)
	// --endpoint-url pins the S3-compatible target even with awscli v1
	// (AWS_ENDPOINT_URL is only honored by botocore >=1.29.16 / CLI v2).
	args := []string{"s3", "cp"}
	if endpoint := os.Getenv("AWS_ENDPOINT_URL"); endpoint != "" {
		args = append(args, "--endpoint-url", endpoint)
	}
	for _, entry := range s3Files {
		if err := uploadWithRetry(ctx, aws, args, entry.local, base+"/"+entry.key, entry.key); err != nil {
			return "", err
		}
	}
	manifestURI := base + "/manifest.json"
	if err := uploadWithRetry(ctx, aws, args, filepath.Join(workdir, "manifest.json"), manifestURI, "manifest.json"); err != nil {
		return "", err
	}
	// The image index is uploaded last: a consumer that can resolve the
	// index is guaranteed a complete artifact set (artifacts, checksums,
	// and manifest are all already in place).
	if err := publishImageIndex(ctx, aws, args, spec.Image, manifestURI, sha256Of(manifestBytes), spec.Output.Publish, imageRefs); err != nil {
		return "", err
	}
	return manifestURI, nil
}

// imageIndexKey derives the content-addressed index key of an image
// reference. It matches the consumer-side cache key, so a consumer can
// resolve the latest published manifest for an image reference without any
// control-plane coordination.
func imageIndexKey(image string) string {
	return sha256Of([]byte(image))
}

// imageIndexPayload builds the image index document pointing at the latest
// published manifest. The manifest reference is content-addressed, so an
// older build of the same image reference stays intact and only the index
// pointer moves. The optional OCI image refs ride along so warm pools can
// discover the registry channel without parsing the manifest.
func imageIndexPayload(image, manifestURI, artifactDigest string, imageRefs ociImageRefs) ([]byte, error) {
	document := struct {
		Image          string `json:"image"`
		ManifestRef    string `json:"manifestRef"`
		ArtifactDigest string `json:"artifactDigest"`
		RootfsImageRef string `json:"rootfsImageRef,omitempty"`
		MemoryImageRef string `json:"memoryImageRef,omitempty"`
		UpdatedAt      string `json:"updatedAt"`
	}{
		Image:          image,
		ManifestRef:    manifestURI,
		ArtifactDigest: artifactDigest,
		RootfsImageRef: imageRefs.Rootfs,
		MemoryImageRef: imageRefs.Memory,
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	return json.MarshalIndent(document, "", "  ")
}

// publishImageIndex uploads the image index object under the store root so
// consumers can resolve the published artifact set from the image reference
// alone. The index lives outside the per-build digest namespace, so a
// rebuild of the same image reference atomically moves the pointer.
//
// The index key is derived from the raw image reference string and must be
// byte-identical to the consumer-side reference (SandboxSpec.Image): no
// normalization, no default tags, no whitespace trimming. Any divergence
// breaks the addressing chain.
//
// Concurrent builds of the same image reference against the same store
// root are last-writer-wins: every build is complete before its index is
// written, so no half-published state is ever observable, but the winner
// is not deterministic (publishers should serialize per image).
func publishImageIndex(ctx context.Context, aws string, args []string, image, manifestURI, artifactDigest, storeRoot string, imageRefs ociImageRefs) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("publish image index: image reference is required (empty image would collide on the empty-hash index key)")
	}
	payload, err := imageIndexPayload(image, manifestURI, artifactDigest, imageRefs)
	if err != nil {
		return err
	}
	local, err := os.CreateTemp("", "fc-image-index-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(local.Name())
	if _, err := local.Write(payload); err != nil {
		return err
	}
	if err := local.Close(); err != nil {
		return err
	}
	key := "index/" + imageIndexKey(image) + ".json"
	target := strings.TrimRight(storeRoot, "/") + "/" + key
	return uploadWithRetry(ctx, aws, args, local.Name(), target, key)
}

// publishRetries is how many times a transient upload failure is retried.
const publishRetries = 3

// uploadWithRetry uploads one object, retrying transient failures with
// exponential backoff so a flaky network does not fail the whole build.
func uploadWithRetry(ctx context.Context, aws string, args []string, local, target, name string) error {
	backoff := 2 * time.Second
	var lastErr error
	for attempt := 0; attempt <= publishRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff *= 2
		}
		command := exec.CommandContext(ctx, aws, append(args, local, target)...)
		output, err := command.CombinedOutput()
		if err != nil {
			lastErr = fmt.Errorf("publish %s: %w: %s", name, err, output)
			// 4xx (auth/access denied etc.) will not succeed on retry;
			// only retry transient/5xx failures.
			if strings.Contains(string(output), "A client error") {
				return lastErr
			}
			klog.V(2).InfoS("publish attempt failed, retrying", "object", name, "attempt", attempt, "err", err)
			continue
		}
		return nil
	}
	return lastErr
}

// patchPodAnnotations records the build outcome on the builder Pod so the
// controller can surface it on the template status. It uses a merge Patch
// touching only the builder's own annotations to avoid racing kubelet's own
// status updates. Standalone runs (E2E, local debugging) simply skip the
// report. The OCI image refs are only included when the OCI path ran.
func patchPodAnnotations(ctx context.Context, manifestRef, digest string, imageRefs ociImageRefs) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.V(2).InfoS("skipping pod annotation update (not running in a cluster)", "err", err)
		return nil
	}
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = serviceAccountNamespace()
	}
	name := os.Getenv("POD_NAME")
	if name == "" {
		return errors.New("POD_NAME is required to report build results")
	}
	refJSON, err := json.Marshal(manifestRef)
	if err != nil {
		return err
	}
	digestJSON, err := json.Marshal(digest)
	if err != nil {
		return err
	}
	annotations := map[string]json.RawMessage{
		manifestRefAnnotation: refJSON,
		digestAnnotation:      digestJSON,
	}
	if imageRefs.Rootfs != "" {
		rootfsJSON, err := json.Marshal(imageRefs.Rootfs)
		if err != nil {
			return err
		}
		memoryJSON, err := json.Marshal(imageRefs.Memory)
		if err != nil {
			return err
		}
		annotations[rootfsImageRefAnnotation] = rootfsJSON
		annotations[memoryImageRefAnnotation] = memoryJSON
	}
	annotationBytes, err := json.Marshal(annotations)
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":%s}}`, annotationBytes)
	_, err = clientSet.CoreV1().Pods(namespace).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

func serviceAccountNamespace() string {
	payload, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "default"
	}
	return string(payload)
}
