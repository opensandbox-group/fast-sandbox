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
	require.Equal(t, "disabled", config.p2pMode(), "p2p must default to disabled (direct S3)")
	require.False(t, config.NodeReadiness.Enabled)
	// Thresholds/interval resolve against the hostready package defaults
	// in the settings loader; the raw config stays empty.
	require.Empty(t, config.NodeReadiness.Interval)
	require.Empty(t, config.NodeReadiness.MinFree)
	require.Empty(t, config.NodeReadiness.MinMemory)
}

func TestLoadAgentConfigParsesP2PAndFillsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	payload := `
socket: /tmp/agent.sock
stateRoot: /tmp/state
p2p:
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
	require.Equal(t, "dart", config.p2pMode())
	require.Equal(t, "http://127.0.0.1:9000", config.P2P.Dart.Addr)
	require.Equal(t, "dart", config.P2P.Dart.Bin, "omitted p2p.dart fields must default")
	require.Equal(t, "127.0.0.1:8147", config.P2P.Dart.Admin)
	require.Equal(t, "4GiB", config.P2P.Dart.CacheSize)
	require.True(t, config.NodeReadiness.Enabled)
	require.Equal(t, "30s", config.NodeReadiness.Interval)
	require.Equal(t, "5GiB", config.NodeReadiness.MinFree)
}

func TestLoadAgentConfigExternalGatewayDefaultsRoutePrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	payload := `
p2p:
  gateway:
    addr: http://127.0.0.1:7500
`
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o644))
	config, err := loadAgentConfig(path)
	require.NoError(t, err)
	require.Equal(t, "external", config.p2pMode())
	require.Equal(t, "http://127.0.0.1:7500", config.P2P.Gateway.Addr)
	require.Equal(t, "/dart/", config.P2P.Gateway.RoutePrefix, "omitted routePrefix must default to the DART route")

	payload = `
p2p:
  gateway:
    addr: http://127.0.0.1:7500
    routePrefix: /peer-blocks
`
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o644))
	config, err = loadAgentConfig(path)
	require.NoError(t, err)
	require.Equal(t, "/peer-blocks", config.P2P.Gateway.RoutePrefix, "an explicit routePrefix is kept verbatim (the pull layer normalizes it)")
}

func TestLoadAgentConfigDeprecatedDartAliasMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	payload := `
dart:
  addr: http://127.0.0.1:9000
  cacheSize: 4GiB
`
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o644))
	config, err := loadAgentConfig(path)
	require.NoError(t, err)
	require.Equal(t, "dart", config.p2pMode(), "the deprecated dart section must still enable DART")
	require.Nil(t, config.Dart, "the deprecated section is merged into p2p")
	require.Equal(t, "http://127.0.0.1:9000", config.P2P.Dart.Addr)
	require.Equal(t, "dart", config.P2P.Dart.Bin, "omitted dart fields must default")
	require.Equal(t, "127.0.0.1:8147", config.P2P.Dart.Admin)
	require.Equal(t, "4GiB", config.P2P.Dart.CacheSize)
}

func TestLoadAgentConfigP2PAndDartMutuallyExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	payload := `
p2p:
  gateway:
    addr: http://127.0.0.1:7500
dart:
  addr: http://127.0.0.1:8145
`
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o644))
	_, err := loadAgentConfig(path)
	require.Error(t, err, "exactly one peer-distribution provider may be configured")
	require.Contains(t, err.Error(), "mutually exclusive")
}

func TestLoadAgentConfigInvalidYAMLFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte("socket: [unclosed"), 0o644))
	_, err := loadAgentConfig(path)
	require.Error(t, err, "a present-but-invalid config must fail startup")
}
