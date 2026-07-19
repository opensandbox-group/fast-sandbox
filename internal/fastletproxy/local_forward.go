package fastletproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
)

const localForwardPreambleSize = fastletnetwork.LocalForwardPreambleSize

type DialContextFunc func(context.Context, string, string) (net.Conn, error)

// EncodeLocalForwardPreamble creates the fixed-size handshake consumed by the
// guest sandbox-tunnel. The signed route credential has already constrained
// targetPort before this is called.
func EncodeLocalForwardPreamble(targetPort uint32, credential string) ([]byte, error) {
	return fastletnetwork.EncodeLocalForwardPreamble(targetPort, credential)
}

func newLocalForwardTransport(access fastletnetwork.AccessDescriptor, targetPort uint32, dial DialContextFunc) (*http.Transport, error) {
	endpoint := access.Address
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
	preamble, err := EncodeLocalForwardPreamble(targetPort, access.Credential)
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
			if err := fastletnetwork.WriteLocalForwardPreamble(connection, preamble); err != nil {
				_ = connection.Close()
				return nil, fmt.Errorf("write local-forward preamble: %w", err)
			}
			return connection, nil
		},
	}, nil
}
