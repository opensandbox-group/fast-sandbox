package env

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"time"

	"fast-sandbox/test/e2e/support/portforward"
)

func StartControllerPortForward(ctx context.Context, namespace string) (string, *portforward.ManagedProcess, error) {
	endpoint, managed, err := startServicePortForward(ctx, namespace, "fast-sandbox-fastpath", 9090)
	if err != nil {
		return "", nil, err
	}
	return endpoint, managed, nil
}

func StartSandboxProxyPortForward(ctx context.Context, namespace string) (string, *portforward.ManagedProcess, error) {
	endpoint, managed, err := startServicePortForward(ctx, namespace, "fast-sandbox-proxy", 8080)
	if err != nil {
		return "", nil, err
	}
	return "http://" + endpoint, managed, nil
}

func StartPodPortForward(ctx context.Context, namespace, pod string, remotePort int) (string, *portforward.ManagedProcess, error) {
	endpoint, managed, err := StartPodTCPPortForward(ctx, namespace, pod, remotePort)
	if err != nil {
		return "", nil, err
	}
	return "http://" + endpoint, managed, nil
}

// StartPodTCPPortForward returns a raw host:port endpoint for protocols such
// as gRPC. Callers that need HTTP should use StartPodPortForward.
func StartPodTCPPortForward(ctx context.Context, namespace, pod string, remotePort int) (string, *portforward.ManagedProcess, error) {
	localPort, err := reserveLocalPort()
	if err != nil {
		return "", nil, err
	}
	cmd := exec.CommandContext(ctx, "kubectl", portforward.BuildKubectlArgs(pod, namespace, localPort, remotePort)...)
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start Pod %s port-forward: %w", pod, err)
	}
	managed := &portforward.ManagedProcess{Cmd: cmd}
	endpoint := fmt.Sprintf("localhost:%d", localPort)
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := portforward.WaitForReady(waitCtx, endpoint, 100*time.Millisecond); err != nil {
		_ = managed.Cleanup()
		return "", nil, fmt.Errorf("wait for Pod %s port-forward: %w", pod, err)
	}
	return endpoint, managed, nil
}

func startServicePortForward(ctx context.Context, namespace, service string, remotePort int) (string, *portforward.ManagedProcess, error) {
	localPort, err := reserveLocalPort()
	if err != nil {
		return "", nil, err
	}

	cmd := exec.CommandContext(ctx, "kubectl", servicePortForwardArgs(namespace, service, localPort, remotePort)...)
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start %s port-forward: %w", service, err)
	}

	managed := &portforward.ManagedProcess{Cmd: cmd}
	endpoint := fmt.Sprintf("localhost:%d", localPort)

	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := portforward.WaitForReady(waitCtx, endpoint, 100*time.Millisecond); err != nil {
		_ = managed.Cleanup()
		return "", nil, fmt.Errorf("wait for %s port-forward: %w", service, err)
	}

	return endpoint, managed, nil
}

func controllerPortForwardArgs(namespace string, localPort int) []string {
	return servicePortForwardArgs(namespace, "fast-sandbox-fastpath", localPort, 9090)
}

func servicePortForwardArgs(namespace, service string, localPort, remotePort int) []string {
	return []string{
		"port-forward",
		"service/" + service,
		fmt.Sprintf("%d:%d", localPort, remotePort),
		"-n",
		namespace,
	}
}

func reserveLocalPort() (int, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, fmt.Errorf("reserve local port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
