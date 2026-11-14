// Package vmmexec implements the Firecracker VMM capability-drop exec
// helper (FSEP-0003). The jailer owns all privileged jail setup and offers
// no capability knob through v1.18.0, so the drop must happen at its final
// exec boundary: the jailer execs this helper (installed with the
// "firecracker" basename so the jail path stays unchanged), the helper
// removes the fixed capability set from every capability set of the calling
// thread, sets PR_SET_NO_NEW_PRIVS, and execs the real Firecracker binary.
// The chain is a single execve chain in one process, so PID, parentage,
// argv shape, namespaces, cgroup, and working directory are unchanged.
//
// Linux applies capability sets, PR_CAPBSET_DROP and PR_SET_NO_NEW_PRIVS to
// the calling thread, and CGO_ENABLED=0 does not make a Go program
// single-threaded: Run requires a thread lock held across every transition
// and the final execve, so the thread that becomes the VMM received them
// all. The execed process keeps uid 0, and because the target is a plain
// root-owned file, the kernel recomputes effective = permitted =
// bounding ∪ inheritable on execve (handle_privileged_root is gated by
// SECURE_NOROOT, not by NO_NEW_PRIVS) — which is why the inheritable set
// must be cleared of the drop bits too, or the dropped capabilities would
// flow straight back into the VMM through pI'.
package vmmexec

import (
	"fmt"
	"path/filepath"
)

// Capability is one capability of the fixed drop set.
type Capability struct {
	// Value is the kernel capability number (the CAP_* constant).
	Value int
	// Name is the short capability name used in diagnostics and errors.
	Name string
}

// DropSet is the fixed, hardcoded capability set removed from the VMM
// process (FSEP-0003). The set is deliberately not operator-configurable:
// which privileges a multi-tenant VMM must not hold is a reviewed security
// decision, and a configuration surface would invite gradual widening. The
// VMM attaches to a tap prepared by the Fastlet network driver and performs
// no interface, firewall, raw-socket, BPF, perf-event, or mlock operation,
// so each entry carries per-item evidence in the FSEP. Later batches extend
// the set toward an empty bounding set through code review.
//
// Batch 1 is explicitly partial bit-level hardening: CAP_SYS_ADMIN is
// retained, and the kernel evaluates bpf_capable()/perfmon_capable() as
// CAP_BPF||CAP_SYS_ADMIN and CAP_PERFMON||CAP_SYS_ADMIN. Operation-level
// denial of BPF and perf events therefore relies on Firecracker's default,
// mandatory seccomp allowlist, verified by
// internal/runtime/firecracker/seccomp_policy_test.go; removing sys_admin is
// a later batch.
var DropSet = []Capability{
	// Interface and firewall administration: the VMM only attaches to the
	// pre-configured tap (TUNSETIFF/TUNSETOFFLOAD/TUNSETVNETHDRSZ) and never
	// administers the slot namespace. The tap is created with owner 0, so
	// the attach succeeds by ownership match on every kernel, including
	// post-CVE-2026-99281 kernels that require CAP_NET_ADMIN for ownerless
	// attach.
	{12, "net_admin"},
	// Raw and packet sockets: the VMM uses none.
	{13, "net_raw"},
	// mlock-family: Firecracker calls none (the only mlock mention in its
	// source is an abandoned TODO), and root's mlock bypasses
	// RLIMIT_MEMLOCK, so this capability is the only lever by which a
	// compromised VMM could pin unbounded host memory.
	{14, "ipc_lock"},
	// Perf events (kernel 5.8+): the VMM opens none.
	{38, "perfmon"},
	// eBPF administration (kernel 5.8+): the VMM loads no programs.
	{39, "bpf"},
}

// DropMask returns the bitmask of the drop set.
func DropMask() uint64 {
	var mask uint64
	for _, capability := range DropSet {
		mask |= 1 << uint(capability.Value)
	}
	return mask
}

// DropValues returns the kernel capability numbers of the drop set.
func DropValues() []int {
	values := make([]int, 0, len(DropSet))
	for _, capability := range DropSet {
		values = append(values, capability.Value)
	}
	return values
}

// DropNames returns the names of the drop set, in drop-set order.
func DropNames() []string {
	names := make([]string, 0, len(DropSet))
	for _, capability := range DropSet {
		names = append(names, capability.Name)
	}
	return names
}

// Offender returns the first dropped capability present in mask, for
// diagnostics when a capability set still holds a dropped bit.
func Offender(mask uint64) (Capability, bool) {
	for _, capability := range DropSet {
		if mask&(1<<uint(capability.Value)) != 0 {
			return capability, true
		}
	}
	return Capability{}, false
}

// Jail layout conventions. The jailer derives the jail root from the
// exec-file basename, so the helper must be installed with the "firecracker"
// basename (keeping <chroot-base>/firecracker/<id>/root and the in-jail
// .pid file name exactly as before the helper existed), and the real VMM
// binary must never occupy the exec-file path inside the jail: the jailer
// copies the exec file with a truncating create, which would fail on a
// read-only bind mount and write through a writable one, corrupting the
// managed host binary. The real binary therefore lives at /vmm/firecracker.
const (
	// VMMJailDir is the jail-root directory holding the real VMM binary.
	VMMJailDir = "vmm"
	// VMMJailName is the in-jail file name of the real VMM binary.
	VMMJailName = "firecracker"
	// JailerVMMPath is the in-jail path the helper execs in jailer mode.
	JailerVMMPath = "/" + VMMJailDir + "/" + VMMJailName
)

// ExecFlag introduces the explicit exec target in the direct-launch mode,
// where the Fastlet owns argv (the jailer owns argv in jailer mode and
// clears no channel to name the target, forcing the fixed
// JailerVMMPath convention there).
const ExecFlag = "--exec"

// ParseArgs resolves the exec target and the arguments to forward.
// Direct-launch mode names the target with "--exec <absolute-path>"; in
// jailer mode the helper argv is the jailer-owned Firecracker command line
// (--id, start-time flags, post--- arguments), so the target is the fixed
// in-jail convention. The forwarded arguments are returned unchanged.
func ParseArgs(argv []string) (target string, forwarded []string, err error) {
	if len(argv) < 1 {
		return "", nil, fmt.Errorf("missing argv")
	}
	rest := argv[1:]
	if len(rest) > 0 && rest[0] == ExecFlag {
		if len(rest) < 2 || !filepath.IsAbs(rest[1]) {
			return "", nil, fmt.Errorf("%s requires an absolute exec target path", ExecFlag)
		}
		return filepath.Clean(rest[1]), rest[2:], nil
	}
	return JailerVMMPath, rest, nil
}

// ExecFunc replaces the current process image; it never returns on success.
type ExecFunc func(path string, argv []string, envv []string) error

// Run is the helper mainline. lockThread pins the calling goroutine to its
// OS thread and must be called before the first privilege transition: the
// capability sets, PR_CAPBSET_DROP, PR_SET_NO_NEW_PRIVS and the final
// execve are all per-thread, and a goroutine migration in between would
// leave the thread that becomes the VMM holding capabilities the helper
// believed it had dropped. Run drops the fixed capability set,
// self-verifies every promised set, and execs the target with argv[0]
// replaced by the target path (the only argv difference from a direct
// Firecracker launch is this exec path prefix, which keeps the
// "firecracker" basename every consumer matches on).
func Run(argv []string, environ []string, lockThread func(), exec ExecFunc) error {
	return run(argv, environ, lockThread, exec, DropCapabilities, VerifyDropped)
}

// run is Run with the privilege transitions injected for platform-neutral
// orchestration tests.
func run(argv []string, environ []string, lockThread func(), exec ExecFunc, drop, verify func() error) error {
	if lockThread == nil {
		return fmt.Errorf("internal error: the thread lock hook is required")
	}
	if exec == nil {
		return fmt.Errorf("internal error: the exec hook is required")
	}
	lockThread()
	target, forwarded, err := ParseArgs(argv)
	if err != nil {
		return err
	}
	if err := drop(); err != nil {
		return err
	}
	if err := verify(); err != nil {
		return fmt.Errorf("capability drop self-check failed: %w", err)
	}
	return exec(target, append([]string{target}, forwarded...), environ)
}
