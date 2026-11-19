package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fullstorydev/grpchan/httpgrpc"
	"github.com/stretchr/testify/require"
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
	return &fastpathv2.PoolInfo{Namespace: req.Namespace, Name: req.PoolName}, nil
}

func TestFastPathHTTPPort(t *testing.T) {
	service := poolService{}
	require.Nil(t, newFastPathHTTPServer(0, service, nil))
	httpServer := newFastPathHTTPServer(8090, service, nil)
	require.Equal(t, ":8090", httpServer.Server.Addr)
	server := httptest.NewServer(httpServer.Server.Handler)
	t.Cleanup(server.Close)

	base, err := url.Parse(server.URL)
	require.NoError(t, err)
	client := fastpathv2.NewFastPathServiceClient(&httpgrpc.Channel{BaseURL: base, Transport: server.Client().Transport})
	got, err := client.GetPool(t.Context(), &fastpathv2.GetPoolRequest{Namespace: "team-a", PoolName: "demo"})
	require.NoError(t, err)
	require.Equal(t, "team-a", got.Namespace)
	require.Equal(t, "demo", got.Name)
	_, err = client.GetPool(t.Context(), &fastpathv2.GetPoolRequest{})
	require.Equal(t, codes.NotFound, status.Code(err))

	oversize, err := proto.Marshal(&fastpathv2.GetPoolRequest{PoolName: strings.Repeat("p", 4<<20)})
	require.NoError(t, err)
	for _, tc := range []struct {
		contentType string
		body        []byte
		status      int
	}{
		{"application/json", []byte("{}"), http.StatusUnsupportedMediaType},
		{httpgrpc.UnaryRpcContentType_V1, oversize, http.StatusRequestEntityTooLarge},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+fastpathv2.FastPathService_GetPool_FullMethodName, bytes.NewReader(tc.body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", tc.contentType)
			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, "HTTP/1.1", resp.Proto)
			require.Equal(t, tc.status, resp.StatusCode)
		})
	}
}
