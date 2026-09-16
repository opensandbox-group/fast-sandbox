package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadAgentConfigMissingFileYieldsDefaults(t *testing.T) {
	config, err := loadAgentConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	require.NoError(t, err)
	require.Equal(t, defaultSocketPath, config.Socket)
	require.Equal(t, defaultStateRoot, config.StateRoot)
	require.Equal(t, "/etc/hostname", config.HostnameFile)
	require.Empty(t, config.Dart.Addr, "dart must default to disabled (direct S3)")
	require.False(t, config.NodeReadiness.Enabled)
	require.Equal(t, "5m", config.NodeReadiness.Interval)
	require.Equal(t, "10GiB", config.NodeReadiness.MinFree)
}

func TestLoadAgentConfigParsesAndFillsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	payload := `
socket: /tmp/agent.sock
stateRoot: /tmp/state
dart:
  addr: http://127.0.0.1:9000
  cacheSize: 4GiB
nodeReadiness:
  enabled: true
  interval: 30s
  minFree: 5GiB
`
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o644))
	config, err := loadAgentConfig(path)
	require.NoError(t, err)
	require.Equal(t, "/tmp/agent.sock", config.Socket)
	require.Equal(t, "http://127.0.0.1:9000", config.Dart.Addr)
	require.Equal(t, "dart", config.Dart.Bin, "omitted dart fields must default")
	require.Equal(t, "127.0.0.1:8147", config.Dart.Admin)
	require.Equal(t, "4GiB", config.Dart.CacheSize)
	require.True(t, config.NodeReadiness.Enabled)
	require.Equal(t, "30s", config.NodeReadiness.Interval)
	require.Equal(t, "5GiB", config.NodeReadiness.MinFree)
	require.Equal(t, "2GiB", config.NodeReadiness.MinMemory, "omitted threshold must default")
}

func TestLoadAgentConfigInvalidYAMLFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte("socket: [unclosed"), 0o644))
	_, err := loadAgentConfig(path)
	require.Error(t, err, "a present-but-invalid config must fail startup")
}
