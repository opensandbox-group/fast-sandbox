package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteImageToDeviceByteExact verifies the restore-critical contract:
// after writing a sparse image onto a "device", the device's first N bytes
// are byte-identical to the image's logical content — including hole
// regions, which must read as zeros even when the device carried stale data
// there beforehand (the empty volume arrives mkfs-formatted).
func TestWriteImageToDeviceByteExact(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "image.bin")
	devicePath := filepath.Join(dir, "device.bin")

	// Build a sparse image: 1MiB data | 2MiB hole | 1MiB data | 1MiB hole.
	const miB = 1024 * 1024
	const imageSize = 5 * miB
	image, err := os.OpenFile(imagePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := image.Truncate(int64(imageSize)); err != nil {
		t.Fatal(err)
	}
	first := make([]byte, miB)
	if _, err := rand.Read(first); err != nil {
		t.Fatal(err)
	}
	second := make([]byte, miB)
	if _, err := rand.Read(second); err != nil {
		t.Fatal(err)
	}
	if _, err := image.WriteAt(first, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := image.WriteAt(second, 3*miB); err != nil {
		t.Fatal(err)
	}
	image.Close()

	// The "device" starts filled with junk: any hole that is not explicitly
	// zeroed would leak this data.
	junk := make([]byte, imageSize+2*int(miB))
	if _, err := rand.Read(junk); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(devicePath, junk, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeImageToDevice(imagePath, devicePath); err != nil {
		t.Fatalf("writeImageToDevice: %v", err)
	}

	device, err := os.ReadFile(devicePath)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(device)) < int64(imageSize) {
		t.Fatalf("device size %d < image size %d", len(device), imageSize)
	}
	if !bytes.Equal(device[:miB], first) {
		t.Fatal("first data extent mismatch")
	}
	if !bytes.Equal(device[3*miB:4*miB], second) {
		t.Fatal("second data extent mismatch")
	}
	for _, hole := range [][2]int{{miB, 3 * miB}, {4 * miB, imageSize}} {
		for i := hole[0]; i < hole[1]; i++ {
			if device[i] != 0 {
				t.Fatalf("byte %d in hole region is %d, want 0 (stale device data leaked)", i, device[i])
			}
		}
	}
}

// TestWriteImageToDeviceDense verifies a fully dense image copies through
// unchanged (the memory snapshot case).
func TestWriteImageToDeviceDense(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "memory.bin")
	devicePath := filepath.Join(dir, "device.bin")

	payload := make([]byte, 3*1024*1024+17)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(devicePath, make([]byte, 4*1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeImageToDevice(imagePath, devicePath); err != nil {
		t.Fatalf("writeImageToDevice: %v", err)
	}
	device, err := os.ReadFile(devicePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(device[:len(payload)], payload) {
		t.Fatal("dense image content mismatch")
	}
}

// TestOCIDerivedRefs checks the image reference derivation contract.
func TestOCIDerivedRefs(t *testing.T) {
	rootfsRef, memRef := ociDerivedRefs("registry.example.com/fs-templates/t1/", "abc123")
	if rootfsRef != "registry.example.com/fs-templates/t1-rootfs:abc123" {
		t.Fatalf("rootfsRef = %q", rootfsRef)
	}
	if memRef != "registry.example.com/fs-templates/t1-mem:abc123" {
		t.Fatalf("memRef = %q", memRef)
	}
	if pinned := ociDigestPin(rootfsRef, "sha256:deadbeef"); pinned != "registry.example.com/fs-templates/t1-rootfs:abc123@sha256:deadbeef" {
		t.Fatalf("pinned = %q", pinned)
	}
}
