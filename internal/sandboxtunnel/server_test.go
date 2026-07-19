package sandboxtunnel

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"github.com/stretchr/testify/require"
)

func TestServerRelaysToSignedTargetPort(t *testing.T) {
	credential := testCredential(t)
	backend := listenTCP(t)
	defer backend.Close()
	go func() {
		connection, err := backend.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()

	tunnel := listenTCP(t)
	defer tunnel.Close()
	_, tunnelPortText, err := net.SplitHostPort(tunnel.Addr().String())
	require.NoError(t, err)
	tunnelPort, err := strconv.ParseUint(tunnelPortText, 10, 16)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &Server{Listener: tunnel, ReservedPort: uint32(tunnelPort), Credential: credential}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()

	connection, err := net.DialTimeout("tcp", tunnel.Addr().String(), time.Second)
	require.NoError(t, err)
	defer connection.Close()
	_, backendPortText, err := net.SplitHostPort(backend.Addr().String())
	require.NoError(t, err)
	backendPort, err := strconv.ParseUint(backendPortText, 10, 16)
	require.NoError(t, err)
	preamble, err := fastletnetwork.EncodeLocalForwardPreamble(uint32(backendPort), credential)
	require.NoError(t, err)
	require.NoError(t, fastletnetwork.WriteLocalForwardPreamble(connection, preamble))
	require.NoError(t, connection.SetDeadline(time.Now().Add(2*time.Second)))
	_, err = connection.Write([]byte("boxlite-tunnel"))
	require.NoError(t, err)
	response := make([]byte, len("boxlite-tunnel"))
	_, err = io.ReadFull(connection, response)
	require.NoError(t, err)
	require.Equal(t, "boxlite-tunnel", string(response))

	require.NoError(t, connection.Close())
	cancel()
	require.NoError(t, <-serveDone)
}

func TestServerRejectsReservedTunnelPort(t *testing.T) {
	credential := testCredential(t)
	tunnel := listenTCP(t)
	defer tunnel.Close()
	_, portText, err := net.SplitHostPort(tunnel.Addr().String())
	require.NoError(t, err)
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{Listener: tunnel, ReservedPort: uint32(port), Credential: credential}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()

	connection, err := net.DialTimeout("tcp", tunnel.Addr().String(), time.Second)
	require.NoError(t, err)
	preamble, err := fastletnetwork.EncodeLocalForwardPreamble(uint32(port), credential)
	require.NoError(t, err)
	require.NoError(t, fastletnetwork.WriteLocalForwardPreamble(connection, preamble))
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(time.Second)))
	buffer := make([]byte, 1)
	_, err = connection.Read(buffer)
	require.Error(t, err)
	_ = connection.Close()
	cancel()
	require.NoError(t, <-serveDone)
}

func TestServerHealthHandshakeDoesNotDialGuestPort(t *testing.T) {
	credential := testCredential(t)
	tunnel := listenTCP(t)
	defer tunnel.Close()
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		Listener: tunnel, Credential: credential,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("health handshake must not dial a guest target")
			return nil, nil
		},
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()
	connection, err := net.DialTimeout("tcp", tunnel.Addr().String(), time.Second)
	require.NoError(t, err)
	preamble, err := fastletnetwork.EncodeLocalForwardHealthPreamble(credential)
	require.NoError(t, err)
	require.NoError(t, fastletnetwork.WriteLocalForwardPreamble(connection, preamble))
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(time.Second)))
	buffer := make([]byte, 1)
	_, err = connection.Read(buffer)
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, connection.Close())
	cancel()
	require.NoError(t, <-serveDone)
}

func TestServerRejectsCredentialFromAnotherBox(t *testing.T) {
	credential := testCredential(t)
	otherCredential := testCredential(t)
	tunnel := listenTCP(t)
	defer tunnel.Close()
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		Listener: tunnel, Credential: credential,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("rejected credential must not reach the guest target dialer")
			return nil, nil
		},
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()
	connection, err := net.DialTimeout("tcp", tunnel.Addr().String(), time.Second)
	require.NoError(t, err)
	preamble, err := fastletnetwork.EncodeLocalForwardPreamble(8080, otherCredential)
	require.NoError(t, err)
	require.NoError(t, fastletnetwork.WriteLocalForwardPreamble(connection, preamble))
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(time.Second)))
	buffer := make([]byte, 1)
	_, err = connection.Read(buffer)
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, connection.Close())
	cancel()
	require.NoError(t, <-serveDone)
}

func TestServerCancellationClosesActiveRelay(t *testing.T) {
	credential := testCredential(t)
	backend := listenTCP(t)
	defer backend.Close()
	backendAccepted := make(chan net.Conn, 1)
	go func() {
		connection, err := backend.Accept()
		if err == nil {
			backendAccepted <- connection
		}
	}()

	tunnel := listenTCP(t)
	defer tunnel.Close()
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{Listener: tunnel, Credential: credential}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()

	connection, err := net.DialTimeout("tcp", tunnel.Addr().String(), time.Second)
	require.NoError(t, err)
	_, backendPortText, err := net.SplitHostPort(backend.Addr().String())
	require.NoError(t, err)
	backendPort, err := strconv.ParseUint(backendPortText, 10, 16)
	require.NoError(t, err)
	preamble, err := fastletnetwork.EncodeLocalForwardPreamble(uint32(backendPort), credential)
	require.NoError(t, err)
	require.NoError(t, fastletnetwork.WriteLocalForwardPreamble(connection, preamble))
	backendConnection := <-backendAccepted
	defer backendConnection.Close()

	cancel()
	select {
	case err := <-serveDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("sandbox tunnel did not stop while an active relay was open")
	}
	_ = connection.Close()
}

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return listener
}

func testCredential(t *testing.T) string {
	t.Helper()
	credential, err := fastletnetwork.GenerateLocalForwardCredential()
	require.NoError(t, err)
	return credential
}
