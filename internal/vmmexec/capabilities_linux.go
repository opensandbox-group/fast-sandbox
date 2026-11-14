//go:build linux

package vmmexec

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux privilege-transition primitives of the helper. Capability sets,
// PR_CAPBSET_DROP and PR_SET_NO_NEW_PRIVS apply to the calling thread: Run
// keeps them on the locked thread.

// _LINUX_CAPABILITY_VERSION_3 covers all 40+ capabilities with two u32
// pairs. Every kernel this project supports (5.10+) implements it.
const capVersion3 = 0x20080522

const (
	prCapbsetRead        = 23 // reads whether a capability is in the bounding set
	prCapbsetDrop        = 24 // drops a capability from the bounding set
	prSetNoNewPrivs      = 38
	prGetNoNewPrivs      = 39
	prCapAmbient         = 47
	prCapAmbientIsSet    = 1
	prCapAmbientClearAll = 3
)

// capUserHeader/capUserData mirror linux/capability.h for the capget and
// capset syscalls.
type capUserHeader struct {
	version uint32
	pid     int32
}

type capUserData [2]struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

// readCapabilitySets returns the current effective, permitted and
// inheritable sets of the calling thread.
func readCapabilitySets() (effective, permitted, inheritable uint64, err error) {
	header := capUserHeader{version: capVersion3}
	var data capUserData
	if _, _, errno := syscall.Syscall(syscall.SYS_CAPGET,
		uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data)), 0); errno != 0 {
		return 0, 0, 0, fmt.Errorf("capget: %w", errno)
	}
	effective = uint64(data[0].effective) | uint64(data[1].effective)<<32
	permitted = uint64(data[0].permitted) | uint64(data[1].permitted)<<32
	inheritable = uint64(data[0].inheritable) | uint64(data[1].inheritable)<<32
	return effective, permitted, inheritable, nil
}

// writeCapabilitySets replaces the effective, permitted and inheritable
// sets of the calling thread. Lowering the sets is unprivileged.
func writeCapabilitySets(effective, permitted, inheritable uint64) error {
	header := capUserHeader{version: capVersion3}
	var data capUserData
	data[0].effective = uint32(effective)
	data[0].permitted = uint32(permitted)
	data[0].inheritable = uint32(inheritable)
	data[1].effective = uint32(effective >> 32)
	data[1].permitted = uint32(permitted >> 32)
	data[1].inheritable = uint32(inheritable >> 32)
	if _, _, errno := syscall.Syscall(syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data)), 0); errno != 0 {
		return fmt.Errorf("capset: %w", errno)
	}
	return nil
}

// boundingSetContains reports whether the capability is in the bounding set
// of the calling thread. An EINVAL means the running kernel does not define
// the capability number (only possible for bpf/perfmon on pre-5.8 kernels),
// which implies absence.
func boundingSetContains(capability int) (bool, error) {
	present, err := prctlRetInt(prCapbsetRead, uintptr(capability), 0)
	if err != nil {
		if errors.Is(err, syscall.EINVAL) {
			return false, nil
		}
		return false, fmt.Errorf("read bounding set for capability %d: %w", capability, err)
	}
	return present == 1, nil
}

// DropCapabilities removes the fixed drop set from the effective, permitted,
// inheritable, ambient and bounding sets of the calling thread. An
// already-absent capability is accepted (for example a future
// non-privileged Fastlet Pod); a real EPERM fails loudly, so the caller
// never proceeds with unintended privileges.
func DropCapabilities() error {
	mask := DropMask()
	// Clear the drop set from the inheritable set first: on execve the
	// kernel recomputes pP' = bounding ∪ inheritable for uid 0, so any bit
	// left in pI would resurrect it in the VMM no matter what the bounding
	// set says. Effective and permitted are cleared for the same reason at
	// the helper level (defense in depth; the exec recomputes them).
	effective, permitted, inheritable, err := readCapabilitySets()
	if err != nil {
		return err
	}
	if remaining := (effective | permitted | inheritable) & mask; remaining != 0 {
		if err := writeCapabilitySets(effective&^mask, permitted&^mask, inheritable&^mask); err != nil {
			return fmt.Errorf("clear the dropped capabilities from the thread sets: %w", err)
		}
	}
	// The ambient set is preserved across execve for plain files; clear it
	// so it cannot carry a dropped bit into the VMM. EINVAL means the
	// running kernel predates ambient sets (pre-4.3): nothing to clear.
	if err := prctl(prCapAmbient, prCapAmbientClearAll); err != nil && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("clear the ambient capability set: %w", err)
	}
	// Drop each capability from the bounding set. PR_CAPBSET_DROP needs
	// CAP_SETPCAP, so a bit that is already absent is skipped (no
	// privilege required to observe absence) and a real EPERM on a present
	// bit fails the launch.
	for _, capability := range DropSet {
		if err := dropOneFromBoundingSet(capability.Value); err != nil {
			return err
		}
	}
	// No new privileges closes the remaining elevation channel for the
	// execed VMM (setuid/file-cap targets and ptrace-assisted gains).
	if err := prctl(prSetNoNewPrivs, 1); err != nil {
		return fmt.Errorf("set PR_SET_NO_NEW_PRIVS: %w", err)
	}
	return nil
}

// dropOneFromBoundingSet drops one capability from the bounding set of the
// calling thread. An already-absent capability is an accepted no-op, and an
// EINVAL (the running kernel does not define the number — only possible for
// bpf/perfmon on pre-5.8 kernels) is a skip; a real EPERM fails loudly so
// the caller never proceeds with unintended privileges.
func dropOneFromBoundingSet(capability int) error {
	present, err := boundingSetContains(capability)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if err := prctl(prCapbsetDrop, uintptr(capability)); err != nil {
		if errors.Is(err, syscall.EINVAL) {
			// The kernel does not define the number; absence is already
			// verified by boundingSetContains.
			return nil
		}
		name := fmt.Sprintf("%d", capability)
		if known, ok := Offender(1 << uint(capability)); ok {
			name = known.Name
		}
		return fmt.Errorf("drop capability %s from the bounding set: %w (refusing to launch the VMM with an unintended privilege)", name, err)
	}
	return nil
}

// VerifyDropped self-verifies every promised set after the transitions: the
// drop set absent from effective, permitted, inheritable, ambient and
// bounding, and NoNewPrivs set. A mismatch fails the exec loudly.
func VerifyDropped() error {
	effective, permitted, inheritable, err := readCapabilitySets()
	if err != nil {
		return err
	}
	mask := DropMask()
	if remaining := (effective | permitted | inheritable) & mask; remaining != 0 {
		capability, _ := Offender(remaining)
		return fmt.Errorf("capability %q is still present in the thread capability sets (%#x)", capability.Name, remaining)
	}
	for _, capability := range DropSet {
		present, err := boundingSetContains(capability.Value)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("capability %q is still present in the bounding set", capability.Name)
		}
		ambient, err := prctlRetInt(prCapAmbient, uintptr(prCapAmbientIsSet), uintptr(capability.Value))
		if err != nil && !errors.Is(err, syscall.EINVAL) {
			return fmt.Errorf("read the ambient set for capability %d: %w", capability.Value, err)
		}
		if err == nil && ambient == 1 {
			return fmt.Errorf("capability %q is still present in the ambient set", capability.Name)
		}
	}
	enabled, err := prctlRetInt(prGetNoNewPrivs, 0, 0)
	if err != nil {
		return fmt.Errorf("read PR_GET_NO_NEW_PRIVS: %w", err)
	}
	if enabled != 1 {
		return fmt.Errorf("PR_SET_NO_NEW_PRIVS did not take effect")
	}
	return nil
}

// prctl wraps prctl(2) for the call shapes the helper uses.
func prctl(option int, arg2 uintptr) error {
	return unix.Prctl(option, arg2, 0, 0, 0)
}

// prctlRetInt wraps the prctl(2) shapes that return a value.
func prctlRetInt(option int, arg2, arg3 uintptr) (int, error) {
	return unix.PrctlRetInt(option, arg2, arg3, 0, 0)
}
