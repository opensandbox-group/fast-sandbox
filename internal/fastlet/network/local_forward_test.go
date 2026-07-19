package network

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalForwardPreambleAuthenticatesPerBoxCredential(t *testing.T) {
	credential, err := GenerateLocalForwardCredential()
	require.NoError(t, err)
	require.NoError(t, ValidateLocalForwardCredential(credential))
	preamble, err := EncodeLocalForwardPreamble(18080, credential)
	require.NoError(t, err)
	require.Len(t, preamble, LocalForwardPreambleSize)
	port, err := DecodeLocalForwardPreamble(bytes.NewReader(preamble), credential)
	require.NoError(t, err)
	require.Equal(t, uint32(18080), port)

	otherCredential, err := GenerateLocalForwardCredential()
	require.NoError(t, err)
	_, err = DecodeLocalForwardPreamble(bytes.NewReader(preamble), otherCredential)
	require.ErrorContains(t, err, "credential rejected")
}

func TestLocalForwardCredentialAndHealthValidation(t *testing.T) {
	credential, err := GenerateLocalForwardCredential()
	require.NoError(t, err)
	health, err := EncodeLocalForwardHealthPreamble(credential)
	require.NoError(t, err)
	port, err := DecodeLocalForwardPreamble(bytes.NewReader(health), credential)
	require.NoError(t, err)
	require.Zero(t, port)

	require.Error(t, ValidateLocalForwardCredential(""))
	_, err = EncodeLocalForwardPreamble(8080, "not-base64!")
	require.Error(t, err)
}
