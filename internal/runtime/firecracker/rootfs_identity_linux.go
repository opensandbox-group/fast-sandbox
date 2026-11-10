//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Verify the Unix socket peer PID; an API response alone cannot prove the connection reaches the VMM referenced by meta.json.
func captureProcessIdentity(ctx context.Context, socket string, pid int) (string, error) {
	peer, err := captureSocketPID(ctx, socket)
	if err != nil {
		return "", err
	}
	if peer != pid {
		return "", fmt.Errorf("%w: API socket does not belong to recorded VMM PID", errCaptureIdentityMismatch)
	}
	return capturePIDIdentity(pid)
}

func capturePIDIdentity(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid VMM PID")
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("incomplete process stat")
	}
	if _, err = strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%d/%s", strings.TrimSpace(string(boot)), pid, fields[19]), nil
}

func captureSocketPID(ctx context.Context, socket string) (int, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var socketErr error
	if err = raw.Control(func(fd uintptr) { cred, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return 0, err
	}
	if socketErr != nil {
		return 0, socketErr
	}
	if cred == nil {
		return 0, fmt.Errorf("missing socket peer credentials")
	}
	return int(cred.Pid), nil
}

// Only confirmed absence in /proc proves exit; permission errors and socket timeouts do not prove death.
func capturePIDAbsent(pid int) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("invalid VMM PID")
	}
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if os.IsNotExist(err) {
		return true, nil
	}
	return false, err
}
