package cli

import (
	"context"
	"testing"
)

func TestCommandUsesConfiguredEndpointAndNamespace(t *testing.T) {
	client := New("/tmp/fastctl", WithEndpoint("127.0.0.1:19090"), WithNamespace("tenant-a"))

	cmd := client.Command(context.Background(), "logs", "sandbox-a")

	want := []string{"--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "logs", "sandbox-a"}
	assertArgsEqual(t, want, cmd.Args[1:])
}

func TestCommandOmitsNamespaceWhenEmpty(t *testing.T) {
	client := New("/tmp/fastctl", WithEndpoint("127.0.0.1:19090"))

	cmd := client.Command(context.Background(), "list")

	want := []string{"--endpoint", "127.0.0.1:19090", "list"}
	assertArgsEqual(t, want, cmd.Args[1:])
}

func assertArgsEqual(t *testing.T, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("expected args %v, got %v", want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("expected args %v, got %v", want, got)
		}
	}
}
