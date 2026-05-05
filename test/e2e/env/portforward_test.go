package env

import "testing"

func TestControllerPortForwardArgsUseControllerDeployment(t *testing.T) {
	args := controllerPortForwardArgs("fast-sandbox-system", 19090)
	want := []string{
		"port-forward",
		"deployment/fast-sandbox-controller",
		"19090:9090",
		"-n",
		"fast-sandbox-system",
	}
	if len(args) != len(want) {
		t.Fatalf("args length = %d, want %d: %#v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; all args: %#v", i, args[i], want[i], args)
		}
	}
}
