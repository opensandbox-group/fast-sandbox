package portforward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"time"
)

type ManagedProcess struct {
	Cmd *exec.Cmd
}

func BuildKubectlArgs(podName, namespace string, localPort, remotePort int) []string {
	return []string{
		"port-forward",
		fmt.Sprintf("pod/%s", podName),
		fmt.Sprintf("%d:%d", localPort, remotePort),
		"-n",
		namespace,
	}
}

func (m ManagedProcess) Cleanup() error {
	if m.Cmd == nil || m.Cmd.Process == nil {
		return nil
	}
	if err := m.Cmd.Process.Kill(); err != nil {
		return err
	}
	if err := m.Cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
		return err
	}
	return nil
}

func WaitForReady(ctx context.Context, address string, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		conn, err := net.DialTimeout("tcp", address, interval)
		if err == nil {
			conn.Close()
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
