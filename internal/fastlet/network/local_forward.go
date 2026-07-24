package network

import (
	"io"

	dataplane "fast-sandbox/internal/dataplane/contract"
)

const (
	LocalForwardHeaderSize     = dataplane.LocalForwardHeaderSize
	LocalForwardCredentialSize = dataplane.LocalForwardCredentialSize
	LocalForwardPreambleSize   = dataplane.LocalForwardPreambleSize
	LocalForwardVersion        = dataplane.LocalForwardVersion
	LocalForwardProtocolTCP    = dataplane.LocalForwardProtocolTCP
)

func GenerateLocalForwardCredential() (string, error) {
	return dataplane.GenerateLocalForwardCredential()
}

func ValidateLocalForwardCredential(encoded string) error {
	return dataplane.ValidateLocalForwardCredential(encoded)
}

func EncodeLocalForwardPreamble(targetPort uint32, encodedCredential string) ([]byte, error) {
	return dataplane.EncodeLocalForwardPreamble(targetPort, encodedCredential)
}

func EncodeLocalForwardHealthPreamble(encodedCredential string) ([]byte, error) {
	return dataplane.EncodeLocalForwardHealthPreamble(encodedCredential)
}

func DecodeLocalForwardPreamble(reader io.Reader, encodedCredential string) (uint32, error) {
	return dataplane.DecodeLocalForwardPreamble(reader, encodedCredential)
}

func WriteLocalForwardPreamble(writer io.Writer, preamble []byte) error {
	return dataplane.WriteLocalForwardPreamble(writer, preamble)
}
