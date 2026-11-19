package main

import (
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/fullstorydev/grpchan/httpgrpc"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	fastpathv2 "fast-sandbox/api/proto/v2"
)

func newFastPathHTTPServer(port int, service fastpathv2.FastPathServiceServer, interceptor grpc.UnaryServerInterceptor) *manager.Server {
	if port == 0 {
		return nil
	}
	rpc := httpgrpc.NewServer(httpgrpc.WithServerUnaryInterceptor(interceptor))
	fastpathv2.RegisterFastPathServiceServer(rpc, service)

	const maxRequestBytes = 4 << 20 // Match the default gRPC receive limit.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if r.Method == http.MethodPost && mediaType != httpgrpc.UnaryRpcContentType_V1 {
			http.Error(w, "expected "+httpgrpc.UnaryRpcContentType_V1, http.StatusUnsupportedMediaType)
			return
		}
		if r.ContentLength > maxRequestBytes {
			http.Error(w, "protobuf request exceeds 4 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		rpc.ServeHTTP(w, r)
	})
	return &manager.Server{
		Name: "fastpath-http",
		Server: &http.Server{
			Addr:              ":" + strconv.Itoa(port),
			Handler:           http.MaxBytesHandler(handler, maxRequestBytes),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       time.Minute,
		},
	}
}
