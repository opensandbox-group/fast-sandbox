package main

import (
	"context"
	"flag"
	"os"

	"k8s.io/klog/v2"

	"fast-sandbox/internal/fastlet/runtime"
	"fast-sandbox/internal/fastlet/server"
)

func main() {
	flag.Parse()
	klog.Info("starting sandbox fastlet")

	podName := getEnv("POD_NAME", "")
	podIP := getEnv("POD_IP", "")
	nodeName := getEnv("NODE_NAME", "")
	namespace := getEnv("NAMESPACE", "")
	fastletPort := getEnv("FASTLET_CONTROL_PORT", getEnv("AGENT_PORT", ":5758"))
	runtimeTypeStr := getEnv("RUNTIME_TYPE", "container")
	runtimeHandler := getEnv("RUNTIME_HANDLER", "")
	runtimeSocket := getEnv("RUNTIME_SOCKET", "")

	klog.InfoS("Fastlet Info", "PodName", podName, "PodIP", podIP, "NodeName", nodeName, "Namespace", namespace)
	klog.InfoS("Runtime", "Type", runtimeTypeStr, "Handler", runtimeHandler, "Socket", runtimeSocket)

	ctx := context.Background()
	var rt runtime.Runtime
	var err error

	rt, err = runtime.NewRuntimeWithHandler(ctx, runtime.RuntimeType(runtimeTypeStr), runtimeSocket, runtimeHandler)

	if err != nil {
		klog.ErrorS(err, "Failed to initialize runtime")
		os.Exit(1)
	}
	defer rt.Close()

	rt.SetNamespace(namespace)

	klog.InfoS("Runtime initialized successfully", "type", runtimeTypeStr)

	sandboxManager := runtime.NewSandboxManager(rt)
	defer sandboxManager.Close()

	fastletServer := server.NewFastletServer(fastletPort, sandboxManager)
	klog.InfoS("Starting Fastlet HTTP Server", "port", fastletPort)

	if err := fastletServer.Start(); err != nil {
		klog.ErrorS(err, "Fastlet server failed")
		os.Exit(1)
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
