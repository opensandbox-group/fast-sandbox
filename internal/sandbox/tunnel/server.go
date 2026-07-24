package tunnel

import (
	"context"
	"errors"
	dataplane "fast-sandbox/internal/dataplane/contract"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const DefaultHandshakeTimeout = 5 * time.Second

type DialContextFunc func(context.Context, string, string) (net.Conn, error)

// Server exposes one fixed guest port and dispatches each connection to the
// loopback target port carried by the Fastlet LocalForward preamble.
type Server struct {
	Listener         net.Listener
	DialContext      DialContextFunc
	HandshakeTimeout time.Duration
	ReservedPort     uint32
	Credential       string
}

func (s *Server) Serve(ctx context.Context) error {
	if s.Listener == nil {
		return errors.New("sandbox tunnel listener is required")
	}
	if err := dataplane.ValidateLocalForwardCredential(s.Credential); err != nil {
		return fmt.Errorf("sandbox tunnel credential: %w", err)
	}
	dial := s.DialContext
	if dial == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	timeout := s.HandshakeTimeout
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}

	var connections sync.WaitGroup
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Listener.Close()
		case <-done:
		}
	}()
	defer close(done)
	defer connections.Wait()

	for {
		connection, err := s.Listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			_ = s.handle(ctx, connection, dial, timeout)
		}()
	}
}

func (s *Server) handle(ctx context.Context, downstream net.Conn, dial DialContextFunc, timeout time.Duration) error {
	defer downstream.Close()
	stopDownstream := context.AfterFunc(ctx, func() { _ = downstream.Close() })
	defer stopDownstream()
	if err := downstream.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	targetPort, err := dataplane.DecodeLocalForwardPreamble(downstream, s.Credential)
	if err != nil {
		return fmt.Errorf("decode local-forward preamble: %w", err)
	}
	if targetPort == 0 {
		return nil
	}
	if s.ReservedPort != 0 && targetPort == s.ReservedPort {
		return errors.New("local-forward target port is reserved by sandbox-tunnel")
	}
	if err := downstream.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	upstream, err := dial(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(targetPort))))
	if err != nil {
		return fmt.Errorf("dial guest loopback target: %w", err)
	}
	defer upstream.Close()
	stopUpstream := context.AfterFunc(ctx, func() { _ = upstream.Close() })
	defer stopUpstream()
	return relay(downstream, upstream)
}

func relay(left, right net.Conn) error {
	errorsCh := make(chan error, 2)
	copyOne := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if closeWriter, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
		errorsCh <- err
	}
	go copyOne(left, right)
	go copyOne(right, left)
	first := <-errorsCh
	second := <-errorsCh
	return errors.Join(first, second)
}
