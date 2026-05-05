package main

import (
	"context"
	"flag"
	"os"

	"k8s.io/klog/v2"

	"fast-sandbox/internal/agent/runtime"
	"fast-sandbox/internal/agent/server"
)

func main() {
	flag.Parse()
	klog.Info("starting sandbox agent")

	podName := getEnv("POD_NAME", "")
	podIP := getEnv("POD_IP", "")
	nodeName := getEnv("NODE_NAME", "")
	namespace := getEnv("NAMESPACE", "")
	agentPort := getEnv("AGENT_PORT", ":5758")
	runtimeTypeStr := getEnv("RUNTIME_TYPE", "container")
	runtimeHandler := getEnv("RUNTIME_HANDLER", "")
	runtimeSocket := getEnv("RUNTIME_SOCKET", "")

	klog.InfoS("Agent Info", "PodName", podName, "PodIP", podIP, "NodeName", nodeName, "Namespace", namespace)
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

	agentServer := server.NewAgentServer(agentPort, sandboxManager)
	klog.InfoS("Starting Agent HTTP Server", "port", agentPort)

	if err := agentServer.Start(); err != nil {
		klog.ErrorS(err, "Agent server failed")
		os.Exit(1)
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
