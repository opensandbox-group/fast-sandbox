package diagnostics

import "testing"

func TestControllerLogsCommandUsesDefaultNamespace(t *testing.T) {
	target := ControllerLogsTarget()

	if target.Namespace != DefaultControllerNamespace {
		t.Fatalf("expected default controller namespace %q, got %q", DefaultControllerNamespace, target.Namespace)
	}
	if target.Selector != "app=fast-sandbox-controller" {
		t.Fatalf("expected controller selector, got %q", target.Selector)
	}
}

func TestPodLogsCommandBuildsKubectlInvocation(t *testing.T) {
	cmd := PodLogsCommand(Target{
		Namespace: "tenant-a",
		PodName:   "fastlet-pod",
	}, 50)

	want := []string{"logs", "fastlet-pod", "-n", "tenant-a", "--tail=50"}
	if len(cmd) != len(want) {
		t.Fatalf("expected command %v, got %v", want, cmd)
	}
	for i := range want {
		if cmd[i] != want[i] {
			t.Fatalf("expected command %v, got %v", want, cmd)
		}
	}
}
