package contract

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
)

const (
	LocalForwardHeaderSize     = 8
	LocalForwardCredentialSize = 32
	LocalForwardPreambleSize   = LocalForwardHeaderSize + LocalForwardCredentialSize
	LocalForwardVersion        = byte(1)
	LocalForwardProtocolTCP    = byte(1)
)

var localForwardMagic = [4]byte{'F', 'S', 'B', 'F'}

func GenerateLocalForwardCredential() (string, error) {
	credential := make([]byte, LocalForwardCredentialSize)
	if _, err := rand.Read(credential); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(credential), nil
}

func ValidateLocalForwardCredential(encoded string) error {
	credential, err := decodeLocalForwardCredential(encoded)
	if err != nil {
		return err
	}
	if len(credential) != LocalForwardCredentialSize {
		return errors.New("local-forward credential must contain exactly 32 bytes")
	}
	return nil
}

func EncodeLocalForwardPreamble(targetPort uint32, encodedCredential string) ([]byte, error) {
	if targetPort == 0 || targetPort > 65535 {
		return nil, errors.New("local-forward target port must be between 1 and 65535")
	}
	return encodeLocalForwardPreamble(targetPort, encodedCredential)
}

func EncodeLocalForwardHealthPreamble(encodedCredential string) ([]byte, error) {
	return encodeLocalForwardPreamble(0, encodedCredential)
}

func encodeLocalForwardPreamble(targetPort uint32, encodedCredential string) ([]byte, error) {
	credential, err := decodeLocalForwardCredential(encodedCredential)
	if err != nil {
		return nil, err
	}
	if len(credential) != LocalForwardCredentialSize {
		return nil, errors.New("local-forward credential must contain exactly 32 bytes")
	}
	preamble := make([]byte, LocalForwardPreambleSize)
	copy(preamble[:4], localForwardMagic[:])
	preamble[4] = LocalForwardVersion
	preamble[5] = LocalForwardProtocolTCP
	binary.BigEndian.PutUint16(preamble[6:], uint16(targetPort))
	copy(preamble[LocalForwardHeaderSize:], credential)
	return preamble, nil
}

func DecodeLocalForwardPreamble(reader io.Reader, encodedCredential string) (uint32, error) {
	expected, err := decodeLocalForwardCredential(encodedCredential)
	if err != nil || len(expected) != LocalForwardCredentialSize {
		return 0, errors.New("invalid local-forward server credential")
	}
	preamble := make([]byte, LocalForwardPreambleSize)
	if _, err := io.ReadFull(reader, preamble); err != nil {
		return 0, err
	}
	if !bytes.Equal(preamble[:4], localForwardMagic[:]) {
		return 0, errors.New("invalid local-forward magic")
	}
	if preamble[4] != LocalForwardVersion {
		return 0, errors.New("unsupported local-forward version")
	}
	if preamble[5] != LocalForwardProtocolTCP {
		return 0, errors.New("unsupported local-forward protocol")
	}
	if subtle.ConstantTimeCompare(preamble[LocalForwardHeaderSize:], expected) != 1 {
		return 0, errors.New("local-forward credential rejected")
	}
	return uint32(binary.BigEndian.Uint16(preamble[6:])), nil
}

func WriteLocalForwardPreamble(writer io.Writer, preamble []byte) error {
	for len(preamble) > 0 {
		written, err := writer.Write(preamble)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		preamble = preamble[written:]
	}
	return nil
}

func decodeLocalForwardCredential(encoded string) ([]byte, error) {
	credential, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("local-forward credential is not valid base64url")
	}
	return credential, nil
}
