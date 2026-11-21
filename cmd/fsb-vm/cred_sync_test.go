package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOverlaybdCredentialsReplaceRevokedAndRotatedRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cred.json")
	private := dockerConfig{Auths: map[string]authEntry{"registry.example/team": {Username: "old", Password: "old-secret"}}}
	refs := []string{"registry.example/team/template:rootfs", "registry.example/team/template:snapfiles"}
	handoff, err := overlaybdCredentials(private, refs...)
	require.NoError(t, err)
	handoff.Auths["registry.example/other"] = authEntry{Username: "other", Password: "keep"}
	require.NoError(t, mergeCredentials(path, handoff))
	// A projected Secret no longer matching this repository must overwrite its
	// old credential, even if a less-specific credential remains in the file.
	require.NoError(t, mergeCredentials(path, dockerConfig{Auths: map[string]authEntry{"registry.example": {Username: "broad", Password: "stale"}}}))
	handoff, err = overlaybdCredentials(dockerConfig{}, refs...)
	require.NoError(t, err)
	require.NoError(t, mergeCredentials(path, handoff))
	current, err := readCredentials(path)
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(":")), current.Auths["registry.example/team/template"].Auth)
	require.Equal(t, "other", current.Auths["registry.example/other"].Username)
	ref, err := parseRegistryReference(refs[0])
	require.NoError(t, err)
	credential, _, err := credentialFor(current, ref)
	require.NoError(t, err)
	require.Empty(t, credential.Username)
	require.Empty(t, credential.Password)
	handoff, err = overlaybdCredentials(dockerConfig{Auths: map[string]authEntry{"registry.example/team": {Username: "rotated", Password: "new-secret"}}}, refs...)
	require.NoError(t, err)
	require.NoError(t, mergeCredentials(path, handoff))
	current, err = readCredentials(path)
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("rotated:new-secret")), current.Auths["registry.example/team/template"].Auth)
}

func TestSharedDaemonRejectsCredentialConflictsAtomically(t *testing.T) {
	previous := global.rejectCredentialConflicts
	global.rejectCredentialConflicts = true
	t.Cleanup(func() { global.rejectCredentialConflicts = previous })
	path := filepath.Join(t.TempDir(), "cred.json")
	original := dockerConfig{Auths: map[string]authEntry{"registry.example/repo": {Auth: "YWxpY2U6c2VjcmV0"}}}
	require.NoError(t, mergeCredentials(path, original))
	require.NoError(t, mergeCredentials(path, original))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, replacement := range []authEntry{{Auth: "Ym9iOnNlY3JldA=="}, {Auth: "Og=="}} {
		err = mergeCredentials(path, dockerConfig{Auths: map[string]authEntry{"registry.example/repo": replacement, "another.example/repo": {Auth: "Og=="}}})
		require.ErrorContains(t, err, "credential conflict")
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
}

func TestMergeCredentialsSkipsRewriteAndLockWhenUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cred.json")
	original := dockerConfig{Auths: map[string]authEntry{"registry.example/repo": {Auth: "YWxpY2U6c2VjcmV0"}}}
	require.NoError(t, mergeCredentials(path, original))
	beforeInfo, err := os.Stat(path)
	require.NoError(t, err)

	// Hold the global exclusive lock to prove an identical merge takes the
	// lock-free fast path and does not rewrite cred.json.
	unlock, err := lockCredentials(path)
	require.NoError(t, err)
	defer unlock()

	require.NoError(t, mergeCredentials(path, original))
	afterInfo, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(beforeInfo, afterInfo), "unchanged credentials must not rewrite cred.json")
}
