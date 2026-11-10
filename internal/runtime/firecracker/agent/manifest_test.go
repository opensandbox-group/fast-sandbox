package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
)

func TestObjectStoreRejectsOCIManifestWithoutNativeFiles(t *testing.T) {
	payload := testManifest(map[string][]byte{
		"rootfs.ext4": []byte("rootfs"), "vmstate.snap": []byte("vmstate"), "memory.snap": []byte("memory"),
	})
	var doc map[string]any
	require.NoError(t, json.Unmarshal(payload, &doc))
	delete(doc, "files")
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	_, err = parseManifest(raw)
	require.ErrorContains(t, err, "no native files")

	// A cached OCI manifest must not bypass the format check as a cache hit.
	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"rootfs.img": []byte("rootfs"), "vmstate.snap": []byte("vmstate"), "memory.snap": []byte("memory"), "manifest.json": raw,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0600))
	}
	ready, err := cacheComplete(dir)
	require.NoError(t, err)
	require.False(t, ready)
}

func TestParseManifest(t *testing.T) {
	payload := testManifest(map[string][]byte{
		"rootfs.ext4":  []byte("rootfs"),
		"vmstate.snap": []byte("vmstate"),
		"memory.snap":  []byte("memory"),
	})
	document, err := parseManifest(payload)
	require.NoError(t, err)
	require.Len(t, document.Files, 3)
	require.Equal(t, sha256Hex([]byte("rootfs")), document.Files["rootfs.ext4"].SHA256)
	require.Equal(t, int64(6), document.Files["rootfs.ext4"].SizeBytes)
}

func TestParseManifestInvalidJSON(t *testing.T) {
	_, err := parseManifest([]byte("{nope"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode manifest")
}

func TestParseManifestEmptyFiles(t *testing.T) {
	var document map[string]any
	require.NoError(t, json.Unmarshal(testManifest(nil), &document))
	document["files"] = map[string]any{}
	payload, err := json.Marshal(document)
	require.NoError(t, err)
	_, err = parseManifest(payload)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no native files")
}

func TestNativeFiles(t *testing.T) {
	artifacts := map[string][]byte{
		"rootfs.ext4":  []byte("rootfs"),
		"vmstate.snap": []byte("vmstate"),
		"memory.snap":  []byte("memory"),
	}
	payload := testManifest(artifacts)
	document, err := parseManifest(payload)
	require.NoError(t, err)

	files, err := document.nativeFiles()
	require.NoError(t, err)
	require.Len(t, files, 3)
	// The mapping renames rootfs.ext4 to rootfs.img and keeps the rest;
	// every entry carries the expected digest.
	want := map[string]string{"rootfs.img": "rootfs.ext4", "vmstate.snap": "vmstate.snap", "memory.snap": "memory.snap"}
	for _, file := range files {
		publishName, ok := want[file.cache]
		require.True(t, ok, file.cache)
		require.Equal(t, publishName, file.publish)
		require.Equal(t, sha256Hex(artifacts[publishName]), file.sha256)
	}
}

func TestNativeFilesMissingArtifact(t *testing.T) {
	payload := testManifest(map[string][]byte{
		"rootfs.ext4": []byte("rootfs"),
		"memory.snap": []byte("memory"),
	})
	document, err := parseManifest(payload)
	require.NoError(t, err)
	_, err = document.nativeFiles()
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing the vmstate.snap artifact")
}

func TestNativeFilesInvalidDigest(t *testing.T) {
	payload := testManifest(map[string][]byte{
		"rootfs.ext4":  []byte("rootfs"),
		"vmstate.snap": []byte("vmstate"),
		"memory.snap":  []byte("memory"),
	})
	var document map[string]any
	require.NoError(t, json.Unmarshal(payload, &document))
	files := document["files"].(map[string]any)
	files["rootfs.ext4"].(map[string]any)["sha256"] = strings.Repeat("Z", 64)
	tampered, err := json.Marshal(document)
	require.NoError(t, err)

	parsed, err := parseManifest(tampered)
	require.NoError(t, err)
	_, err = parsed.nativeFiles()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid sha256")
}

func TestValidSHA256(t *testing.T) {
	require.True(t, validSHA256(strings.Repeat("a", 64)))
	require.False(t, validSHA256(strings.Repeat("a", 63)))
	require.False(t, validSHA256(strings.Repeat("A", 64)))
	require.False(t, validSHA256(strings.Repeat("g", 64)))
	require.False(t, validSHA256(""))
}

func TestNativeRootfsManifestModeBoundaries(t *testing.T) {
	for _, scenario := range []string{"valid", "legacy-full", "vmstate.snap", "memory.snap", "invalid-size", "invalid-digest"} {
		t.Run(scenario, func(t *testing.T) {
			dir := stageRootfsArtifactSet(t)
			document, err := artifacts.DecodeVMManifest(mustManifestBytes(t, dir))
			require.NoError(t, err)
			switch scenario {
			case "legacy-full":
				document.Platform.SnapshotMode = ""
			case "vmstate.snap", "memory.snap":
				document.Files[scenario] = artifacts.ManifestFile{SHA256: strings.Repeat("a", 64), SizeBytes: 1}
			case "invalid-size":
				entry := document.Files["rootfs.ext4"]
				entry.SizeBytes = -1
				document.Files["rootfs.ext4"] = entry
			case "invalid-digest":
				entry := document.Files["rootfs.ext4"]
				entry.SHA256 = "broken"
				document.Files["rootfs.ext4"] = entry
			}
			payload, err := json.Marshal(document)
			require.NoError(t, err)
			parsed, err := parseManifest(payload)
			if err == nil {
				var files []nativeFile
				files, err = parsed.nativeFiles()
				if scenario == "valid" {
					require.NoError(t, err)
					require.Len(t, files, 1)
					require.Equal(t, nativeRootfsCacheName, files[0].cache)
					return
				}
			}
			require.Error(t, err, "invalid or incomplete modes must not downgrade to RootfsOnly")
		})
	}
}
