package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func newFastPathHTTPServer(port int, grpcServer *grpc.Server) *manager.Server {
	if port == 0 {
		return nil
	}
	return &manager.Server{
		Name: "fastpath-http",
		Server: &http.Server{
			Addr:              ":" + strconv.Itoa(port),
			Handler:           grpcweb.WrapServer(grpcServer),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       time.Minute,
		},
	}
}
