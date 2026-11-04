package env

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationRuntimeLeafCountRequiresSuccessfulObservation(t *testing.T) {
	functions := integrationCgroupFunctions(t, "resource_aware_runtime_leaf_count", "resource_aware_runtime_leaf_count_matches")
	for _, test := range []struct {
		name      string
		status    int
		leaves    int
		expected  int
		malformed bool
		want      string
	}{
		{name: "exec failure with no output", status: 42, expected: 0, want: "mismatch"},
		{name: "exec failure with partial output", status: 42, leaves: 2, expected: 2, want: "mismatch"},
		{name: "observed empty", expected: 0, want: "match"},
		{name: "observed two leaves", leaves: 2, expected: 2, want: "match"},
		{name: "leaves still present", leaves: 2, expected: 0, want: "mismatch"},
		{name: "count is an expression", malformed: true, expected: 0, want: "mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := "set -euo pipefail\nNS=test\n" + functions + fmt.Sprintf(`
kubectl() {
  for ((leaf=0; leaf<%d; leaf++)); do printf '/cg/fsb-%%s\n' "$leaf"; done
  return %d
}
`, test.leaves, test.status)
			if test.malformed {
				script += "resource_aware_runtime_leaf_count() { printf '0+0'; }\n"
			}
			// wait_for invokes its predicate in a conditional context, where
			// errexit cannot replace explicit observation-status handling.
			script += fmt.Sprintf("if resource_aware_runtime_leaf_count_matches pod /cg %d; then echo match; else echo mismatch; fi\n", test.expected)
			output, err := exec.Command("bash", "-c", script).CombinedOutput()
			if err != nil || strings.TrimSpace(string(output)) != test.want {
				t.Fatalf("leaf observation: err=%v output=%q, want %q", err, output, test.want)
			}
		})
	}
}

func TestIntegrationOOMCleanupStopsLoadBeforeDeletingResources(t *testing.T) {
	cleanup := integrationCgroupFunctions(t, "resource_aware_oom_cleanup")
	for _, test := range []struct {
		name        string
		coordinates string
		stopStatus  int
		wantStop    bool
	}{
		{name: "active load", coordinates: "pod=fastlet\naggregate=/cg\n", wantStop: true},
		{name: "stop failed", coordinates: "pod=fastlet\naggregate=/cg\n", stopStatus: 42, wantStop: true},
		{name: "early failure with no pod"},
		{name: "early failure before cgroup lookup", coordinates: "pod=fastlet\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracePath := filepath.Join(t.TempDir(), "cleanup-trace")
			script := "set -euo pipefail\n" + cleanup + test.coordinates + fmt.Sprintf(`
NS=test
RESOURCE_AWARE_OOM_SANDBOX_A=a
RESOURCE_AWARE_OOM_SANDBOX_B=b
RESOURCE_AWARE_OOM_POOL=pool
recovery_name=recovery
resource_aware_stop_runtime_cpu_load() { echo stop-load >>"$TRACE_PATH"; return %d; }
kubectl() { echo delete >>"$TRACE_PATH"; }
resolve_daemon_down() { echo resolve-down >>"$TRACE_PATH"; }
port_forward_down() { echo forward-down >>"$TRACE_PATH"; }
trap 'resource_aware_oom_cleanup' EXIT
exit 77
`, test.stopStatus)
			command := exec.Command("bash", "-c", script)
			command.Env = append(os.Environ(), "TRACE_PATH="+tracePath)
			output, err := command.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 77 {
				t.Fatalf("cleanup changed the original failure: err=%v output=%q", err, output)
			}
			trace, err := os.ReadFile(tracePath)
			if err != nil {
				t.Fatal(err)
			}
			want := "delete\ndelete\nresolve-down\nforward-down\n"
			if test.wantStop {
				want = "stop-load\n" + want
			}
			if string(trace) != want {
				t.Fatalf("cleanup order = %q, want %q", trace, want)
			}
		})
	}
}

// Extract the actual helpers without executing the script's host setup or CLI.
func integrationCgroupFunctions(t *testing.T, names ...string) string {
	t.Helper()
	root, err := findRootDir()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "scripts", "integration-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	var functions strings.Builder
	for _, name := range names {
		start := strings.Index(source, "\n"+name+"() {")
		if start < 0 {
			t.Fatalf("missing shell helper %s", name)
		}
		body := source[start+1:]
		end := strings.Index(body, "\n}\n")
		if end < 0 {
			t.Fatalf("missing end of shell helper %s", name)
		}
		functions.WriteString(body[:end+3])
	}
	return functions.String()
}
