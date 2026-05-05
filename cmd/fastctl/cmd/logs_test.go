package cmd

import (
	"io"
	"reflect"
	"testing"
)

func TestNewPortForwardCommandDiscardsOutput(t *testing.T) {
	cmd := newPortForwardCommand("fastlet-pod", "tenant-a", 19090)
	wantArgs := []string{"kubectl", "port-forward", "pod/fastlet-pod", "19090:5758", "-n", "tenant-a"}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", cmd.Args, wantArgs)
	}
	if cmd.Stdout != io.Discard {
		t.Fatalf("Stdout = %#v, want io.Discard", cmd.Stdout)
	}
	if cmd.Stderr != io.Discard {
		t.Fatalf("Stderr = %#v, want io.Discard", cmd.Stderr)
	}
}
