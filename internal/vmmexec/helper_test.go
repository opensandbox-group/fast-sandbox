package vmmexec

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveHelperPath(t *testing.T) {
	t.Setenv(HelperPathEnv, "")

	// The default image path fails loudly when the helper is not installed:
	// a VMM must never launch with the full Pod capability set because an
	// image forgot to ship the helper.
	_, err := ResolveHelperPath()
	require.ErrorContains(t, err, "capability-drop helper "+DefaultHelperPath+" is not installed")

	// An override pointing at a valid regular executable file resolves.
	dir := t.TempDir()
	helper := filepath.Join(dir, HelperBasename)
	require.NoError(t, os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755))
	t.Setenv(HelperPathEnv, helper)
	resolved, err := ResolveHelperPath()
	require.NoError(t, err)
	require.Equal(t, helper, resolved)

	// The basename is contractual: the jailer derives the jail root from
	// the exec-file basename, so any other name would silently move every
	// Fastlet jail path.
	t.Setenv(HelperPathEnv, filepath.Join(dir, "vmm-capdrop"))
	_, err = ResolveHelperPath()
	require.ErrorContains(t, err, "basename")

	t.Setenv(HelperPathEnv, "relative/helper")
	_, err = ResolveHelperPath()
	require.ErrorContains(t, err, "absolute")

	t.Setenv(HelperPathEnv, filepath.Join(dir, "missing", HelperBasename))
	_, err = ResolveHelperPath()
	require.ErrorContains(t, err, "not installed")

	asDir := filepath.Join(dir, "as-dir", HelperBasename)
	require.NoError(t, os.MkdirAll(asDir, 0o750))
	t.Setenv(HelperPathEnv, asDir) // a directory, not a regular file
	_, err = ResolveHelperPath()
	require.ErrorContains(t, err, "regular file")

	binary := filepath.Join(dir, "noexec", HelperBasename)
	require.NoError(t, os.MkdirAll(filepath.Dir(binary), 0o750))
	require.NoError(t, os.WriteFile(binary, []byte("x"), 0o640))
	t.Setenv(HelperPathEnv, binary)
	_, err = ResolveHelperPath()
	require.ErrorContains(t, err, "not executable")
}
