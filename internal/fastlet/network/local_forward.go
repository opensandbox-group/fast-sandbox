package network

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	LocalForwardPreambleSize = 8
	LocalForwardVersion      = byte(1)
	LocalForwardProtocolTCP  = byte(1)
)

var localForwardMagic = [4]byte{'F', 'S', 'B', 'F'}

// EncodeLocalForwardPreamble creates the fixed-size handshake used between
// Fastlet Proxy and a runtime-specific guest tunnel.
func EncodeLocalForwardPreamble(targetPort uint32) ([]byte, error) {
	if targetPort == 0 || targetPort > 65535 {
		return nil, errors.New("local-forward target port must be between 1 and 65535")
	}
	return encodeLocalForwardPreamble(targetPort), nil
}

// EncodeLocalForwardHealthPreamble uses reserved target port zero. It is only
// used by the runtime Sidecar to prove that the guest tunnel, rather than just
// the host forwarding socket, accepted the protocol handshake.
func EncodeLocalForwardHealthPreamble() []byte {
	return encodeLocalForwardPreamble(0)
}

func encodeLocalForwardPreamble(targetPort uint32) []byte {
	preamble := make([]byte, LocalForwardPreambleSize)
	copy(preamble[:4], localForwardMagic[:])
	preamble[4] = LocalForwardVersion
	preamble[5] = LocalForwardProtocolTCP
	binary.BigEndian.PutUint16(preamble[6:], uint16(targetPort))
	return preamble
}

// DecodeLocalForwardPreamble validates and decodes one complete tunnel
// handshake. Only TCP is supported in protocol v1.
func DecodeLocalForwardPreamble(reader io.Reader) (uint32, error) {
	preamble := make([]byte, LocalForwardPreambleSize)
	if _, err := io.ReadFull(reader, preamble); err != nil {
		return 0, err
	}
	if string(preamble[:4]) != string(localForwardMagic[:]) {
		return 0, errors.New("invalid local-forward magic")
	}
	if preamble[4] != LocalForwardVersion {
		return 0, errors.New("unsupported local-forward version")
	}
	if preamble[5] != LocalForwardProtocolTCP {
		return 0, errors.New("unsupported local-forward protocol")
	}
	port := binary.BigEndian.Uint16(preamble[6:])
	return uint32(port), nil
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
