package portforward

import (
	"context"
	"net"
	"os/exec"
	"testing"
	"time"
)

func TestBuildKubectlArgs(t *testing.T) {
	args := BuildKubectlArgs("fastlet-pod", "tenant-a", 39090, 5758)
	want := []string{"port-forward", "pod/fastlet-pod", "39090:5758", "-n", "tenant-a"}

	if len(args) != len(want) {
		t.Fatalf("expected args %v, got %v", want, args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("expected args %v, got %v", want, args)
		}
	}
}

func TestManagedProcessCleanupStopsChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start child process: %v", err)
	}

	managed := ManagedProcess{Cmd: cmd}
	if err := managed.Cleanup(); err != nil {
		t.Fatalf("expected cleanup to succeed, got error: %v", err)
	}

	if cmd.ProcessState == nil {
		t.Fatal("expected cleanup to wait for the child process")
	}
}

func TestWaitForReadyDetectsListeningPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := WaitForReady(ctx, listener.Addr().String(), 10*time.Millisecond); err != nil {
		t.Fatalf("expected ready wait to succeed, got error: %v", err)
	}
}
