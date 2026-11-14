package firecracker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/firecrackervmm"
)

// The first FSEP-0003 batch retains CAP_SYS_ADMIN, and the kernel evaluates
// bpf_capable()/perfmon_capable() as CAP_BPF||CAP_SYS_ADMIN and
// CAP_PERFMON||CAP_SYS_ADMIN — so clearing the bpf/perfmon bits alone does
// not deny the operations. The operation-level denial this batch relies on
// is Firecracker's default, mandatory seccomp allowlist (default action
// trap): these checked-in upstream policies (the files the release binaries
// compile their filters from) must contain neither bpf(2) nor
// perf_event_open. Adding a managed Firecracker version requires adding its
// policy here with the same evidence bar.

// seccompPolicyFile models the seccompiler policy JSON documents.
type seccompPolicyFile struct {
	VMM struct {
		DefaultAction string `json:"default_action"`
		Filter        []struct {
			Syscall string `json:"syscall"`
		} `json:"filter"`
	} `json:"vmm"`
}

func TestManagedVMMSeccompPolicyDeniesBPFOperations(t *testing.T) {
	catalog, err := firecrackervmm.ParseCatalog(firecrackervmm.Releases)
	require.NoError(t, err)

	for _, version := range catalog.Versions {
		for _, arch := range []string{"x86_64", "aarch64"} {
			path := filepath.Join("testdata", "seccomp", version, arch+".json")
			payload, err := os.ReadFile(path)
			require.NoError(t, err, "the managed version %s must ship its upstream seccomp policy for review", version)

			var policy seccompPolicyFile
			require.NoError(t, json.Unmarshal(payload, &policy))
			// The default action is a trap: anything not allowlisted kills
			// the VMM rather than failing the call.
			require.Equal(t, "trap", policy.VMM.DefaultAction, "%s", path)

			allowed := map[string]bool{}
			for _, entry := range policy.VMM.Filter {
				allowed[entry.Syscall] = true
			}
			require.NotContains(t, allowed, "bpf", "%s must not allow bpf(2)", path)
			require.NotContains(t, allowed, "perf_event_open", "%s must not allow perf_event_open", path)
		}
	}
}
