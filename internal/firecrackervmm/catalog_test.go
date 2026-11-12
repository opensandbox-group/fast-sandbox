package firecrackervmm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCatalogValidation(t *testing.T) {
	_, err := ParseCatalog(Releases)
	require.NoError(t, err)
	for _, raw := range []string{
		`{"schemaVersion":2,"legacyVersion":"1.16.1","versions":["1.16.1"]}`,
		`{"schemaVersion":1,"legacyVersion":"1.16.1","versions":["1.16.2"]}`,
		`{"schemaVersion":1,"legacyVersion":"1.16.1","versions":["1.16.1","1.16.1"]}`,
		`{"schemaVersion":1,"legacyVersion":"../x","versions":["../x"]}`,
		`{"schemaVersion":1,"legacyVersion":"01.16.1","versions":["01.16.1"]}`,
		`{"schemaVersion":1,"legacyVersion":"1.16.1","versions":["1.16.1"]} {}`,
	} {
		_, err := ParseCatalog([]byte(raw))
		require.Error(t, err, raw)
	}
}

func TestResolveExactLatestAndIncomplete(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "catalog.json"), []byte(`{"schemaVersion":1,"legacyVersion":"1.9.0","versions":["1.10.0","1.9.0"]}`), 0644))
	for _, v := range []string{"1.9.0", "1.10.0"} {
		dir := filepath.Join(root, "versions", v)
		require.NoError(t, os.MkdirAll(dir, 0755))
		for _, name := range []string{"firecracker", "jailer"} {
			brand := "Firecracker"
			if name == "jailer" {
				brand = "Jailer"
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho '"+brand+" v"+v+"'\n"), 0755))
		}
	}
	latest, err := Resolve(t.Context(), root, "", true)
	require.NoError(t, err)
	require.Equal(t, "1.10.0", latest.Version)
	require.NotEmpty(t, latest.JailerPath)
	old, err := Resolve(t.Context(), root, "1.9.0", false)
	require.NoError(t, err)
	require.Equal(t, "1.9.0", old.Version)
	require.Empty(t, old.JailerPath)
	_, err = Resolve(t.Context(), root, "1.8.0", false)
	require.ErrorContains(t, err, "unavailable")
	require.NoError(t, os.Remove(latest.BinaryPath))
	_, err = Resolve(t.Context(), root, "", false)
	require.Error(t, err, "do not silently choose an older release when latest is incomplete")
}

func TestResolveRejectsInvalidPairs(t *testing.T) {
	for _, failure := range []string{"wrong-version", "symlink", "not-executable"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "catalog.json"), []byte(`{"schemaVersion":1,"legacyVersion":"1.17.0","versions":["1.17.0"]}`), 0644))
			dir := filepath.Join(root, "versions", "1.17.0")
			require.NoError(t, os.MkdirAll(dir, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "firecracker"), []byte("#!/bin/sh\necho 'Firecracker v1.17.0'\n"), 0755))
			jailer := filepath.Join(dir, "jailer")
			switch failure {
			case "wrong-version":
				require.NoError(t, os.WriteFile(jailer, []byte("#!/bin/sh\necho 'Jailer v1.16.1'\n"), 0755))
			case "symlink":
				require.NoError(t, os.Symlink(filepath.Join(dir, "firecracker"), jailer))
			case "not-executable":
				require.NoError(t, os.WriteFile(jailer, []byte("data"), 0644))
			}
			_, err := Resolve(t.Context(), root, "1.17.0", false)
			require.Error(t, err)
		})
	}
}
