package main

// oci_image.go implements the OCI image publishing stage for
// format=overlaybd builds with an output registry (see
// docs/design/sandboxtemplate-oci-distribution.md): rootfs.ext4 and
// memory.snap are packed as single-layer OverlayBD OCI images via an
// embedded streamingvolume service, while vmstate.snap and manifest.json
// keep using the S3 channel.
//
// The streamingvolume-dependent parts live in strmvold_linux.go; this file
// holds the portable pieces (reference derivation, the byte-exact device
// writer) so they build and test on every platform.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"k8s.io/klog/v2"
)

// ociImageRefs carries the digest-pinned references of the two published
// images through manifest assembly and Pod annotation reporting.
type ociImageRefs struct {
	Rootfs string
	Memory string
}

// ociImageTag derives the per-build tag for both images. The generation is
// not stable inside the builder (the controller owns it), so the tag uses
// the short artifact identity supplied by the caller (manifest digest short
// prefix); consumers always resolve via the digest-pinned refs.
func ociImageTag(shortID string) string {
	if shortID == "" {
		return "latest"
	}
	return shortID
}

// buildShortID derives a short, per-workspace identifier for image tags.
// It reuses the snapshot phase marker when present and falls back to the
// workspace basename.
func buildShortID(workdir string) string {
	if payload, err := os.ReadFile(filepath.Join(workdir, "snapshot-phases.json")); err == nil && len(payload) > 0 {
		if sum := sha256Of(payload); len(sum) >= 12 {
			return sum[:12]
		}
	}
	base := filepath.Base(workdir)
	if len(base) > 12 {
		return base[:12]
	}
	return base
}

// ociDerivedRefs returns the tag refs for the rootfs and memory images of
// one build: <registry>-rootfs:<tag> and <registry>-mem:<tag>.
func ociDerivedRefs(registry, tag string) (rootfsRef, memRef string) {
	base := strings.TrimRight(registry, "/")
	return fmt.Sprintf("%s-rootfs:%s", base, tag), fmt.Sprintf("%s-mem:%s", base, tag)
}

// ociDigestPin combines a tag ref with a sha256 digest into a digest-pinned
// reference (ref@sha256:...).
func ociDigestPin(ref, digest string) string {
	return ref + "@" + digest
}

// ociManifestDigest reads the manifest digest streamingvolume recorded for
// targetRef after a successful Commit. streamingvolume stores committed
// manifests under <root>/manifests/artifacts/<ref with '/' replaced by '+'>
// as a symlink whose target is ./sha256:<digest>; reading the symlink avoids
// re-fetching anything from the registry.
func ociManifestDigest(serviceRoot, targetRef string) (string, error) {
	symlink := filepath.Join(serviceRoot, "manifests", "artifacts", strings.ReplaceAll(targetRef, "/", "+"))
	target, err := os.Readlink(symlink)
	if err != nil {
		return "", fmt.Errorf("read committed manifest digest for %s: %w", targetRef, err)
	}
	digest := strings.TrimPrefix(filepath.Base(target), "./")
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("committed manifest digest for %s has unexpected form %q", targetRef, digest)
	}
	return digest, nil
}

// ociLocalArtifacts describes where a committed (not necessarily pushed)
// image lives in the strmvold session root — the kept-around build output
// for push-less runs.
type ociLocalArtifacts struct {
	ManifestPath string // manifests/artifacts/<digest>/manifest.json
	LayerDigest  string // the single LSMT layer's digest
	BlobPath     string // blobs/<layer-digest>
}

// ociResolveLocalArtifacts maps a committed targetRef to its on-disk
// manifest and layer blob under the strmvold session root. Layout per
// streamingvolume's saveManifest/blobPath: manifests/artifacts/<ref> is a
// symlink to ./<manifest-digest>, the manifest JSON lives beside it as
// manifest.json, and layer blobs sit in blobs/<digest>.
func ociResolveLocalArtifacts(serviceRoot, targetRef string) (ociLocalArtifacts, error) {
	digest, err := ociManifestDigest(serviceRoot, targetRef)
	if err != nil {
		return ociLocalArtifacts{}, err
	}
	manifestPath := filepath.Join(serviceRoot, "manifests", "artifacts", strings.TrimPrefix(digest, "./"), "manifest.json")
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		return ociLocalArtifacts{}, fmt.Errorf("read committed manifest %s: %w", manifestPath, err)
	}
	var manifest struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return ociLocalArtifacts{}, fmt.Errorf("parse committed manifest %s: %w", manifestPath, err)
	}
	if len(manifest.Layers) != 1 {
		return ociLocalArtifacts{}, fmt.Errorf("committed manifest %s has %d layers, want 1", manifestPath, len(manifest.Layers))
	}
	layerDigest := manifest.Layers[0].Digest
	return ociLocalArtifacts{
		ManifestPath: manifestPath,
		LayerDigest:  layerDigest,
		BlobPath:     filepath.Join(serviceRoot, "blobs", layerDigest),
	}, nil
}

// deviceWriteChunk is the unit size for zero-filling holes and copying data
// extents onto the block device.
const deviceWriteChunk = 4 * 1024 * 1024

// writeImageToDevice writes a raw disk image onto a block device so the
// device content is byte-identical to the image's logical content.
//
// Data extents are copied as-is. Hole extents are explicitly zero-filled:
// the device is handed over freshly mkfs-formatted by streamingvolume's
// empty-volume attach, and a plain sparse copy would leave that stale
// filesystem data in the holes — for memory.snap the holes are untouched
// guest RAM pages, which must read as zeros after restore. Zeroing is
// skipped for regions past the image's logical size: consumers read only
// sizeBytes (recorded in the manifest), so mkfs leftovers there are never
// observable.
//
// TODO: attempt blkdiscard on the device before writing (and then skip hole
// zeroing) to keep the ~30Gi zero pass off the rootfs volume; that requires
// confirming the ublk/overlaybd stack honors discard.
func writeImageToDevice(sourcePath, devicePath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open image %s: %w", sourcePath, err)
	}
	defer source.Close()

	size, err := source.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek image %s: %w", sourcePath, err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek image %s: %w", sourcePath, err)
	}

	device, err := os.OpenFile(devicePath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open device %s: %w", devicePath, err)
	}
	defer device.Close()

	zeros := make([]byte, deviceWriteChunk)
	written := int64(0)
	for written < size {
		dataStart, dataErr := seekData(source, written)
		if dataErr != nil {
			if dataErr == syscall.ENXIO {
				// No data until EOF: zero the remainder.
				if err := zeroDeviceRange(device, written, size-written, zeros); err != nil {
					return err
				}
				written = size
				break
			}
			// No SEEK_DATA support: fall back to a dense copy.
			if _, err := source.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if _, err := device.Seek(0, io.SeekStart); err != nil {
				return err
			}
			return copyDeviceRange(source, device, size)
		}
		if dataStart > written {
			if err := zeroDeviceRange(device, written, dataStart-written, zeros); err != nil {
				return err
			}
			written = dataStart
		}
		holeStart, holeErr := seekHole(source, dataStart)
		if holeErr != nil {
			holeStart = size
		}
		if _, err := source.Seek(dataStart, io.SeekStart); err != nil {
			return err
		}
		if err := copyTo(source, device, holeStart-dataStart); err != nil {
			return fmt.Errorf("copy data extent [%d,%d): %w", dataStart, holeStart, err)
		}
		written = holeStart
	}
	return device.Sync()
}

// zeroDeviceRange writes n zero bytes at the device's current offset.
func zeroDeviceRange(device *os.File, offset, n int64, zeros []byte) error {
	if _, err := device.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek device to %d: %w", offset, err)
	}
	return writeTo(device, n, zeros)
}

// copyTo copies exactly n bytes from source to the device at its current
// offset.
func copyTo(source io.Reader, device *os.File, n int64) error {
	buffer := make([]byte, deviceWriteChunk)
	for n > 0 {
		chunk := int64(len(buffer))
		if n < chunk {
			chunk = n
		}
		read, err := io.ReadFull(source, buffer[:chunk])
		if err != nil {
			return err
		}
		if _, err := device.Write(buffer[:read]); err != nil {
			return err
		}
		n -= int64(read)
	}
	return nil
}

// copyDeviceRange performs a dense full copy of size bytes (SEEK_DATA
// fallback path).
func copyDeviceRange(source *os.File, device *os.File, size int64) error {
	return copyTo(source, device, size)
}

// writeTo writes n bytes from buf (zero-filled) to w.
func writeTo(w io.Writer, n int64, buf []byte) error {
	for n > 0 {
		chunk := int64(len(buf))
		if n < chunk {
			chunk = n
		}
		if _, err := w.Write(buf[:chunk]); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

// logOCIPublish emits the per-image publish outcome for stage timing.
func logOCIPublish(name, ref string, elapsedMs int64) {
	klog.InfoS("oci image published", "image", name, "ref", ref, "elapsedMs", elapsedMs)
}
