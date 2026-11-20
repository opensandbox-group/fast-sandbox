package registryconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestRegistryConfigStrictYAMLAndNormalization(t *testing.T) {
	var config Config
	require.NoError(t, yaml.UnmarshalStrict([]byte(`
registries:
  - host: https://REGISTRY-1.DOCKER.IO/v1/
    repositoryPrefix: library
    secretRef:
      name: docker-hub
  - host: registry.example.com
    repositoryPrefix: team-a/tools
    secretRef:
      name: team-a-tools
`), &config))
	normalized, err := NormalizeAndValidate(config)
	require.NoError(t, err)
	require.Equal(t, "docker.io", normalized.Registries[0].Host)
	require.Equal(t, "library", normalized.Registries[0].RepositoryPrefix)
	content, err := yaml.Marshal(normalized)
	require.NoError(t, err)
	require.Contains(t, string(content), "repositoryPrefix:")
}

func TestCompiledRegistryUsesExactHostAndLongestRepositoryPrefix(t *testing.T) {
	compiled, err := NewCompiled([]Credential{
		{Host: "registry.example.com", Username: "host", Password: "host-secret"},
		{Host: "registry.example.com", RepositoryPrefix: "team-a", Username: "team", Password: "team-secret"},
		{Host: "registry.example.com", RepositoryPrefix: "team-a/tools", Username: "tools", Password: "tools-secret"},
		{Host: "docker.io", RepositoryPrefix: "library", Username: "hub", Password: "hub-secret"},
	})
	require.NoError(t, err)

	credential, found := compiled.Match("registry.example.com/team-a/tools/runner:v1")
	require.True(t, found)
	require.Equal(t, "tools", credential.Username)
	credential, found = compiled.Match("registry.example.com/team-a/application@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.True(t, found)
	require.Equal(t, "team", credential.Username)
	credential, found = compiled.Match("registry.example.com/other/application:v1")
	require.True(t, found)
	require.Equal(t, "host", credential.Username)
	credential, found = compiled.Match("alpine:latest")
	require.True(t, found)
	require.Equal(t, "hub", credential.Username)
	_, found = compiled.Match("other.example.com/team-a/application:v1")
	require.False(t, found)
}

func TestFileProviderReloadsAtomicRegistryProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	writeCompiled := func(credentials []Credential) Compiled {
		t.Helper()
		compiled, err := NewCompiled(credentials)
		require.NoError(t, err)
		content, err := compiled.Marshal()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, content, 0o600))
		return compiled
	}
	first := writeCompiled([]Credential{{Host: "registry.example.com", Username: "first", Password: "secret"}})
	provider := NewFileProvider(path)
	credential, found, err := provider.Credentials("registry.example.com/team/image:v1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "first", credential.Username)
	require.Equal(t, first.Revision, provider.Revision())

	second := writeCompiled([]Credential{{Host: "registry.example.com", Username: "second", Password: "rotated"}})
	require.NotEqual(t, first.Revision, second.Revision)
	credential, found, err = provider.Credentials("registry.example.com/team/image:v1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "second", credential.Username)
	require.Equal(t, second.Revision, provider.Revision())

	// The declared revision is informational: a stale or placeholder
	// revision still loads, and lookups simply reflect the configured
	// credentials.
	require.NoError(t, os.WriteFile(path, []byte(`{"revision":"wrong","credentials":[]}`), 0o600))
	_, found, err = provider.Credentials("registry.example.com/team/image:v1")
	require.NoError(t, err)
	require.False(t, found)
}

func TestRegistryConfigRejectsDuplicateMatch(t *testing.T) {
	_, err := NormalizeAndValidate(Config{Registries: []RegistryRule{
		{Host: "registry.example.com", RepositoryPrefix: "/team/", SecretRef: SecretRef{Name: "one"}},
		{Host: "REGISTRY.EXAMPLE.COM", RepositoryPrefix: "team", SecretRef: SecretRef{Name: "two"}},
	}})
	require.ErrorContains(t, err, "duplicate registry match")
}
