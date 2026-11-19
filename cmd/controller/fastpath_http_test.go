package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	fastpathv2 "fast-sandbox/api/proto/v2"
)

type poolService struct {
	fastpathv2.UnimplementedFastPathServiceServer
}

func (poolService) GetPool(_ context.Context, req *fastpathv2.GetPoolRequest) (*fastpathv2.PoolInfo, error) {
	if req.PoolName == "" {
		return nil, status.Error(codes.NotFound, "pool missing")
	}
	return &fastpathv2.PoolInfo{Name: req.PoolName}, nil
}

func TestFastPathGRPCWeb(t *testing.T) {
	backend := grpc.NewServer()
	t.Cleanup(backend.Stop)
	fastpathv2.RegisterFastPathServiceServer(backend, poolService{})
	require.Nil(t, newFastPathHTTPServer(0, backend))
	listener := newFastPathHTTPServer(8090, backend)
	require.Equal(t, ":8090", listener.Server.Addr)
	server := httptest.NewServer(listener.Server.Handler)
	t.Cleanup(server.Close)

	post := func(t *testing.T, name string) (http.Header, []byte) {
		t.Helper()
		payload, err := proto.Marshal(&fastpathv2.GetPoolRequest{PoolName: name})
		require.NoError(t, err)
		frame := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(payload)))
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+fastpathv2.FastPathService_GetPool_FullMethodName, bytes.NewReader(append(frame, payload...)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/grpc-web+proto")
		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, "HTTP/1.1", resp.Proto)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "application/grpc-web+proto", resp.Header.Get("Content-Type"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.Header, body
	}

	t.Run("response and status trailer", func(t *testing.T) {
		_, body := post(t, "demo")
		expected, err := proto.Marshal(&fastpathv2.PoolInfo{Name: "demo"})
		require.NoError(t, err)
		frame := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(expected)))
		frame = append(frame, expected...)
		require.True(t, bytes.HasPrefix(body, frame))
		trailer := body[len(frame):]
		require.GreaterOrEqual(t, len(trailer), 5)
		require.Equal(t, byte(0x80), trailer[0])
		require.Equal(t, uint32(len(trailer)-5), binary.BigEndian.Uint32(trailer[1:5]))
		require.Contains(t, string(trailer[5:]), "grpc-status: 0\r\n")
	})
	t.Run("trailers-only error", func(t *testing.T) {
		headers, body := post(t, "")
		require.Empty(t, body)
		require.Equal(t, "5", headers.Get("Grpc-Status"))
		require.Equal(t, "pool missing", headers.Get("Grpc-Message"))
	})
}
