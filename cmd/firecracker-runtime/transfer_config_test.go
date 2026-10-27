package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadAgentConfigTransferDefaultsToLegacy(t *testing.T) {
	config, err := loadAgentConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	require.NoError(t, err)
	require.False(t, config.Transfer.transfer().Chunked(), "an absent transfer section is legacy")
}

func TestLoadAgentConfigParsesTransfer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	payload := `
transfer:
  format: chunked
  compression: zstd
  concurrency: 4
  chunkSizeMiB: 8
`
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))
	config, err := loadAgentConfig(path)
	require.NoError(t, err)
	transfer := config.Transfer.transfer()
	require.True(t, transfer.Chunked())
	require.Equal(t, "zstd", transfer.Compression)
	require.Equal(t, 4, transfer.Concurrency)
	require.Equal(t, int64(8<<20), transfer.ChunkSize)
}

func TestLoadAgentConfigRejectsInvalidTransfer(t *testing.T) {
	for name, payload := range map[string]string{
		"unknown format":       "transfer:\n  format: fast\n",
		"zstd without chunked": "transfer:\n  compression: zstd\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.yaml")
			require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))
			_, err := loadAgentConfig(path)
			require.ErrorContains(t, err, "transfer:")
		})
	}
}
