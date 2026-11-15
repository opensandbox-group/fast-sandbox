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
	// The canonical path is returned; on macOS the TempDir prefix itself is
	// a symlink (/var -> /private/var).
	canonical, err := filepath.EvalSymlinks(helper)
	require.NoError(t, err)
	require.Equal(t, canonical, resolved)

	// The basename is contractual: the jailer derives the jail root from
	// the exec-file basename, so any other name would silently move every
	// Fastlet jail path.
	misnamed := filepath.Join(dir, "vmm-capdrop")
	require.NoError(t, os.WriteFile(misnamed, []byte("x"), 0o755))
	t.Setenv(HelperPathEnv, misnamed)
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

	// The override is resolved through symlinks and the canonical path is
	// returned: the jailer canonicalizes the exec file before deriving the
	// jail basename, so a misnamed symlink target must be rejected even
	// though the link itself is named firecracker.
	link := filepath.Join(dir, "linked", HelperBasename)
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o750))
	foreign := filepath.Join(dir, "foreign", "vmm-capdrop")
	require.NoError(t, os.MkdirAll(filepath.Dir(foreign), 0o750))
	require.NoError(t, os.WriteFile(foreign, []byte("x"), 0o755))
	require.NoError(t, os.Symlink(foreign, link))
	t.Setenv(HelperPathEnv, link)
	_, err = ResolveHelperPath()
	require.ErrorContains(t, err, "basename")

	// A symlink whose canonical target carries the contract basename
	// resolves to the target path (which the jailer canonicalizes to).
	target := filepath.Join(dir, "real", HelperBasename)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o755))
	okLink := filepath.Join(dir, "ok", HelperBasename)
	require.NoError(t, os.MkdirAll(filepath.Dir(okLink), 0o750))
	require.NoError(t, os.Symlink(target, okLink))
	t.Setenv(HelperPathEnv, okLink)
	resolved, err = ResolveHelperPath()
	require.NoError(t, err)
	canonicalTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	require.Equal(t, canonicalTarget, resolved)

	binary := filepath.Join(dir, "noexec", HelperBasename)
	require.NoError(t, os.MkdirAll(filepath.Dir(binary), 0o750))
	require.NoError(t, os.WriteFile(binary, []byte("x"), 0o640))
	t.Setenv(HelperPathEnv, binary)
	_, err = ResolveHelperPath()
	require.ErrorContains(t, err, "not executable")
}
