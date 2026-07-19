package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/sandboxtunnel"
)

func main() {
	listenAddress := flag.String("listen", ":19090", "guest address used by the runtime LocalForward mapping")
	credential := flag.String("credential", "", "per-Box LocalForward authentication credential")
	flag.Parse()
	if err := fastletnetwork.ValidateLocalForwardCredential(*credential); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-tunnel: credential: %v\n", err)
		os.Exit(1)
	}
	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-tunnel: listen: %v\n", err)
		os.Exit(1)
	}
	defer listener.Close()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-tunnel: resolve listen port: %v\n", err)
		os.Exit(1)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		fmt.Fprintf(os.Stderr, "sandbox-tunnel: invalid listen port %q\n", portText)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	server := &sandboxtunnel.Server{Listener: listener, ReservedPort: uint32(port), Credential: *credential}
	if err := server.Serve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-tunnel: %v\n", err)
		os.Exit(1)
	}
}
