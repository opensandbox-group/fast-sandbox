package firecracker

import (
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/vmmexec"
)

// sampleVMMStatus mirrors a /proc/<pid>/status document of a jailed VMM
// after the capability drop (full sets minus the dropped bits).
const sampleVMMStatus = `Name:	firecracker
Umask:	0022
State:	S (sleeping)
Pid:	4242
CapInh:	000001ffffffffff
CapPrm:	0000013fffff8fff
CapEff:	0000013fffff8fff
CapBnd:	0000013fffff8fff
CapAmb:	0000000000000000
NoNewPrivs:	1
`

func TestParseVMMCapabilityStatus(t *testing.T) {
	status, err := parseVMMCapabilityStatus(sampleVMMStatus)
	require.NoError(t, err)
	require.Equal(t, uint64(0x000001ffffffffff), status.inheritable)
	// The dropped bits (net_admin 12, net_raw 13, ipc_lock 14, perfmon 38,
	// bpf 39) are absent from the effective/permitted/bounding sets.
	require.Equal(t, uint64(0x0000013fffff8fff), status.effective)
	require.Zero(t, status.effective&vmmexec.DropMask())
	require.True(t, status.noNewPrivs)

	// A kernel without the NoNewPrivs field is a parse error, not a silent
	// pass.
	_, err = parseVMMCapabilityStatus("CapEff:\t0\n")
	require.ErrorContains(t, err, "NoNewPrivs")

	_, err = parseVMMCapabilityStatus("CapEff:\tnot-hex\nNoNewPrivs:\t1\n")
	require.ErrorContains(t, err, "parse CapEff")
}

func TestVerifyCapabilitiesDropped(t *testing.T) {
	dropped, err := parseVMMCapabilityStatus(sampleVMMStatus)
	require.NoError(t, err)
	require.NoError(t, dropped.verifyCapabilitiesDropped())

	// A dropped bit still present in CapEff names the capability.
	status := dropped
	status.effective |= 1 << 12
	err = status.verifyCapabilitiesDropped()
	require.ErrorContains(t, err, "CapEff")
	require.ErrorContains(t, err, "net_admin")

	// A dropped bit restored in the bounding set fails the assertion too
	// (the next exec would resurrect it).
	status = dropped
	status.bounding |= 1 << 39
	err = status.verifyCapabilitiesDropped()
	require.ErrorContains(t, err, "CapBnd")
	require.ErrorContains(t, err, "bpf")

	// NoNewPrivs is required unconditionally.
	status = dropped
	status.noNewPrivs = false
	require.ErrorContains(t, status.verifyCapabilitiesDropped(), "NoNewPrivs")

	// Retained capabilities outside the drop set are fine (batch 1 keeps
	// CAP_SYS_ADMIN; the operation-level BPF/perf denial is the seccomp
	// allowlist's, verified by seccomp_policy_test.go).
	status = dropped
	status.effective |= 1 << 21 // CAP_SYS_ADMIN
	require.NoError(t, status.verifyCapabilitiesDropped())
}
