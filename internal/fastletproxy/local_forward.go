package fastletproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

const (
	localForwardPreambleSize = 8
	localForwardVersion      = byte(1)
	localForwardProtocolTCP  = byte(1)
)

var localForwardMagic = [4]byte{'F', 'S', 'B', 'F'}

type DialContextFunc func(context.Context, string, string) (net.Conn, error)

// EncodeLocalForwardPreamble creates the fixed-size handshake consumed by the
// guest sandbox-tunnel. The signed route credential has already constrained
// targetPort before this is called.
func EncodeLocalForwardPreamble(targetPort uint32) ([]byte, error) {
	if targetPort == 0 || targetPort > 65535 {
		return nil, errors.New("local-forward target port must be between 1 and 65535")
	}
	preamble := make([]byte, localForwardPreambleSize)
	copy(preamble[:4], localForwardMagic[:])
	preamble[4] = localForwardVersion
	preamble[5] = localForwardProtocolTCP
	binary.BigEndian.PutUint16(preamble[6:], uint16(targetPort))
	return preamble, nil
}

func newLocalForwardTransport(endpoint string, targetPort uint32, dial DialContextFunc) (*http.Transport, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid local-forward endpoint: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("local-forward endpoint must use a loopback IP")
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort == 0 {
		return nil, errors.New("local-forward endpoint port must be between 1 and 65535")
	}
	preamble, err := EncodeLocalForwardPreamble(targetPort)
	if err != nil {
		return nil, err
	}
	if dial == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	return &http.Transport{
		Proxy: nil, ForceAttemptHTTP2: false, DisableCompression: true, DisableKeepAlives: true,
		MaxIdleConns: 256, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			connection, err := dial(ctx, "tcp", endpoint)
			if err != nil {
				return nil, err
			}
			if err := writeLocalForwardPreamble(connection, preamble); err != nil {
				_ = connection.Close()
				return nil, fmt.Errorf("write local-forward preamble: %w", err)
			}
			return connection, nil
		},
	}, nil
}

func writeLocalForwardPreamble(writer io.Writer, preamble []byte) error {
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
