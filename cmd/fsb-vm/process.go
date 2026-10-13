package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// processRecord identifies one Firecracker process across CLI invocations. A
// bare PID is not an identity: after the VMM exits or the host reboots the
// number can be recycled by an unrelated workload, so the record also pins
// the kernel start time of the process and the boot it ran in.
type processRecord struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time"`
	BootID    string `json:"boot_id"`
}

// procRoot is the procfs mount; a variable so tests can point it at a fake
// tree.
var procRoot = "/proc"

// currentBootID returns the kernel boot id (a fresh UUID per boot).
func currentBootID() (string, error) {
	payload, err := os.ReadFile(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", fmt.Errorf("read boot id: %w", err)
	}
	return strings.TrimSpace(string(payload)), nil
}

// processStartTime returns field 22 (starttime, clock ticks since boot) of
// /proc/<pid>/stat. The comm field may contain spaces and parentheses, so the
// fields are counted from the last ')'.
func processStartTime(pid int) (uint64, error) {
	payload, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	return parseStatStartTime(payload)
}

func parseStatStartTime(payload []byte) (uint64, error) {
	end := bytes.LastIndexByte(payload, ')')
	if end < 0 {
		return 0, fmt.Errorf("malformed /proc stat: no comm terminator")
	}
	// After ") " the first field is field 3 (state); starttime is field 22.
	fields := strings.Fields(string(payload[end+1:]))
	const startTimeIndex = 22 - 3
	if len(fields) <= startTimeIndex {
		return 0, fmt.Errorf("malformed /proc stat: %d fields after comm", len(fields))
	}
	return strconv.ParseUint(fields[startTimeIndex], 10, 64)
}

// recordProcess captures the identity of a freshly started process.
func recordProcess(pid int) (processRecord, error) {
	startTime, err := processStartTime(pid)
	if err != nil {
		return processRecord{}, fmt.Errorf("read start time of pid %d: %w", pid, err)
	}
	bootID, err := currentBootID()
	if err != nil {
		return processRecord{}, err
	}
	return processRecord{PID: pid, StartTime: startTime, BootID: bootID}, nil
}

// matches reports whether the live process behind r.PID is still the one the
// record was taken from. A missing process, another boot, or a different
// start time all mean the record is stale.
func (r processRecord) matches() bool {
	if r.PID <= 0 || r.BootID == "" {
		return false
	}
	bootID, err := currentBootID()
	if err != nil || bootID != r.BootID {
		return false
	}
	startTime, err := processStartTime(r.PID)
	return err == nil && startTime == r.StartTime
}

// alive reports whether the recorded process is still running (and still
// the same process).
func (r *processRecord) alive() bool {
	return r != nil && r.matches()
}

// findProcessByArgs scans procfs for a process whose argv contains flag
// immediately followed by value, and returns its identity. It recovers a
// Firecracker launched by a create that died before persisting the PID: the
// per-sandbox API socket path is unique to the sandbox id.
func findProcessByArgs(flag, value string) (*processRecord, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", procRoot, err)
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(payload), "\x00"), "\x00")
		for index := 0; index+1 < len(args); index++ {
			if args[index] == flag && args[index+1] == value {
				record, err := recordProcess(pid)
				if err != nil {
					break // exited between the two reads
				}
				return &record, nil
			}
		}
	}
	return nil, os.ErrNotExist
}

// terminateProcess stops the recorded process: SIGTERM, then SIGKILL if it
// has not exited within grace. Signals go through a pidfd obtained before the
// identity is re-verified, so the number cannot be recycled between the
// check and the signal. A process that is already gone, or that is no longer
// the recorded one, is not an error.
func terminateProcess(record processRecord, grace time.Duration) error {
	if record.PID <= 0 {
		return nil
	}
	pidfd, err := unix.PidfdOpen(record.PID, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		return fmt.Errorf("pidfd_open %d: %w", record.PID, err)
	}
	defer unix.Close(pidfd)
	// The pidfd pins the process it was opened for; a matching identity now
	// proves the fd refers to the recorded process.
	if !record.matches() {
		return nil
	}
	for _, step := range []struct {
		signal syscall.Signal
		wait   time.Duration
	}{{syscall.SIGTERM, grace}, {syscall.SIGKILL, 5 * time.Second}} {
		if err := unix.PidfdSendSignal(pidfd, step.signal, nil, 0); err != nil {
			if errors.Is(err, unix.ESRCH) {
				return nil
			}
			return fmt.Errorf("signal pid %d with %s: %w", record.PID, step.signal, err)
		}
		exited, err := waitPidfd(pidfd, step.wait)
		if err != nil {
			return err
		}
		if exited {
			return nil
		}
	}
	return fmt.Errorf("pid %d did not exit after SIGKILL", record.PID)
}

// waitPidfd waits until the process behind pidfd exits (the fd turns
// readable) or timeout elapses.
func waitPidfd(pidfd int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		ready, err := unix.Poll(fds, int(remaining.Milliseconds()))
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return false, fmt.Errorf("poll pidfd: %w", err)
		}
		if ready > 0 {
			return true, nil
		}
		if remaining == 0 {
			return false, nil
		}
	}
}
