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
	"fast-sandbox/internal/artifacts"
)

// publish uploads the artifacts under a digest namespace and returns the
// manifest URI. Upload order guarantees consumers never observe a
// half-published artifact set: artifacts and SHA256SUMS first, then the
// manifest, and finally the image index that points at this build.
// OverlayBD layers keep their relative paths
// (overlaybd/rootfs/layer.lsmt, overlaybd/memory/layer.lsmt) — a flat
// basename would collide on the same S3 key.
func publish(ctx context.Context, spec apiv1alpha2.SandboxTemplateSpec, workdir string, manifestBytes []byte) (string, error) {
	aws, err := exec.LookPath(awsBin)
	if err != nil {
		return "", fmt.Errorf("aws CLI not found: %w", err)
	}
	base := strings.TrimRight(spec.Output.Publish, "/") + "/" + sha256Of(manifestBytes)[:16]
	entries := []struct{ local, key string }{
		{filepath.Join(workdir, "rootfs.ext4"), "rootfs.ext4"},
		{filepath.Join(workdir, "vmstate.snap"), "vmstate.snap"},
		{filepath.Join(workdir, "memory.snap"), "memory.snap"},
		// SHA256SUMS covers only the published artifact set; it belongs to
		// the immutable build directory and goes up with the artifacts.
		{filepath.Join(workdir, "SHA256SUMS"), "SHA256SUMS"},
	}
	if layers, err := filepath.Glob(filepath.Join(workdir, "overlaybd", "*", "layer.lsmt")); err == nil {
		for _, layer := range layers {
			relative, err := filepath.Rel(workdir, layer)
			if err != nil {
				return "", err
			}
			entries = append(entries, struct{ local, key string }{layer, relative})
		}
	}
	// --endpoint-url pins the S3-compatible target even with awscli v1
	// (AWS_ENDPOINT_URL is only honored by botocore >=1.29.16 / CLI v2).
	args := []string{"s3", "cp"}
	if endpoint := os.Getenv("AWS_ENDPOINT_URL"); endpoint != "" {
		args = append(args, "--endpoint-url", endpoint)
	}
	for _, entry := range entries {
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
	if err := publishImageIndex(ctx, aws, args, spec.Image, manifestURI, sha256Of(manifestBytes), spec.Output.Publish, spec.IndexKey); err != nil {
		return "", err
	}
	return manifestURI, nil
}

// imageIndexPayload builds the image index document pointing at the latest
// published manifest. The manifest reference is content-addressed, so an
// older build of the same image reference stays intact and only the index
// pointer moves. The format lives in internal/artifacts.
func imageIndexPayload(image, manifestURI, artifactDigest string) ([]byte, error) {
	return artifacts.ImageIndexPayload(image, manifestURI, artifactDigest, time.Now())
}

// publishImageIndex uploads the image index object(s) under the store root
// so consumers can resolve the published artifact set from the image
// reference alone. The index lives outside the per-build digest namespace,
// so a rebuild of the same image reference atomically moves the pointer.
//
// The index key is derived from the raw image reference string and must be
// byte-identical to the consumer-side reference (SandboxSpec.Image): no
// normalization, no default tags, no whitespace trimming. Any divergence
// breaks the addressing chain.
//
// When spec.indexKey is set, the same payload is additionally published
// under that key with the payload image field equal to the key, giving the
// build an exact, immutable identity: the OpenSandbox server sets it to the
// template ID, so two templates of the same source image never alias each
// other's artifact sets or node caches, while the default sha256(image) key
// stays last-writer-wins for warmImages and older clients.
//
// Concurrent builds of the same image reference against the same store
// root are last-writer-wins: every build is complete before its index is
// written, so no half-published state is ever observable, but the winner
// is not deterministic (publishers should serialize per image).
func publishImageIndex(ctx context.Context, aws string, args []string, image, manifestURI, artifactDigest, storeRoot string, indexKey string) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("publish image index: image reference is required (empty image would collide on the empty-hash index key)")
	}
	for _, key := range indexKeys(image, indexKey) {
		if err := publishOneImageIndex(ctx, aws, args, key, manifestURI, artifactDigest, storeRoot); err != nil {
			return err
		}
	}
	return nil
}

// indexKeys returns the image-index keys a build publishes under: the raw
// image reference (default, last-writer-wins) plus the optional exact
// spec.indexKey identity when set to something else.
func indexKeys(image, indexKey string) []string {
	keys := []string{image}
	if key := strings.TrimSpace(indexKey); key != "" && key != image {
		keys = append(keys, key)
	}
	return keys
}

func publishOneImageIndex(ctx context.Context, aws string, args []string, key, manifestURI, artifactDigest, storeRoot string) error {
	payload, err := imageIndexPayload(key, manifestURI, artifactDigest)
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
	objectKey := "index/" + artifacts.ImageIndexKey(key) + ".json"
	target := strings.TrimRight(storeRoot, "/") + "/" + objectKey
	return uploadWithRetry(ctx, aws, args, local.Name(), target, objectKey)
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
// touching only the two annotations to avoid racing kubelet's own status
// updates. Standalone runs (E2E, local debugging) simply skip the report.
func patchPodAnnotations(ctx context.Context, manifestRef, digest string) error {
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
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":%s,"%s":%s}}}`,
		manifestRefAnnotation, refJSON, digestAnnotation, digestJSON)
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
