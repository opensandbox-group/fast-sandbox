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
	localPort, err := reserveLocalPort()
	if err != nil {
		return "", nil, err
	}

	cmd := exec.CommandContext(ctx, "kubectl", controllerPortForwardArgs(namespace, localPort)...)
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start controller port-forward: %w", err)
	}

	managed := &portforward.ManagedProcess{Cmd: cmd}
	endpoint := fmt.Sprintf("localhost:%d", localPort)

	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := portforward.WaitForReady(waitCtx, endpoint, 100*time.Millisecond); err != nil {
		_ = managed.Cleanup()
		return "", nil, fmt.Errorf("wait for controller port-forward: %w", err)
	}

	return endpoint, managed, nil
}

func controllerPortForwardArgs(namespace string, localPort int) []string {
	return []string{
		"port-forward",
		"service/fast-sandbox-fastpath",
		fmt.Sprintf("%d:9090", localPort),
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
