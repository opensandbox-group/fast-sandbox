//go:build linux

// Package backendidentity verifies node-generated daemon qualification records without requiring Fastlet to share the host PID namespace.
package backendidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type socketIdentity struct {
	Device  uint64 `json:"device"`
	Inode   uint64 `json:"inode"`
	Seconds int64  `json:"ctimeSeconds"`
	Nanos   int64  `json:"ctimeNanos"`
}
type record struct {
	Version int            `json:"version"`
	SHA256  string         `json:"sha256"`
	BootID  string         `json:"bootID"`
	Socket  socketIdentity `json:"socket"`
}

func fingerprint(socket string) (socketIdentity, error) {
	var s unix.Stat_t
	if err := unix.Stat(socket, &s); err != nil {
		return socketIdentity{}, err
	}
	if s.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return socketIdentity{}, errors.New("backend path is not a socket")
	}
	return socketIdentity{uint64(s.Dev), s.Ino, s.Ctim.Sec, s.Ctim.Nsec}, nil
}
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func peerPID(ctx context.Context, socket string) (int, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var inner error
	if err = raw.Control(func(fd uintptr) { cred, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return 0, err
	}
	if inner != nil {
		return 0, inner
	}
	if cred == nil || cred.Pid <= 0 || cred.Uid != 0 {
		return 0, errors.New("backend peer must be visible and root-owned")
	}
	return int(cred.Pid), nil
}

// Attest runs only in the node daemon launcher and binds the serving process executable to the current socket instance.
// The launcher must remove stale records first, hold an exclusive lock throughout the daemon lifetime, and revoke the record on exit.
func Attest(ctx context.Context, socket, expected string) error {
	before, err := fingerprint(socket)
	if err != nil {
		return err
	}
	pid, err := peerPID(ctx, socket)
	if err != nil {
		return err
	}
	sum, err := hashFile(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return err
	}
	if sum != expected {
		return errors.New("OverlayBD executable does not match qualified artifact")
	}
	after, err := fingerprint(socket)
	if err != nil {
		return err
	}
	if before != after {
		return errors.New("backend socket changed during qualification")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	raw, err := json.Marshal(record{1, sum, string(boot), before})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(socket), ".qualification-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), socket+".qualification.json")
}

// Verify trusts the shared socket directory managed by node root, not a client-supplied digest.
// Reject capture if the record is missing, the socket was replaced, the host rebooted, or the deployment digest differs; never fall back to unverified operation.
func Verify(ctx context.Context, socket, expected string) error {
	var directory unix.Stat_t
	if err := unix.Stat(filepath.Dir(socket), &directory); err != nil {
		return err
	}
	if directory.Uid != 0 || directory.Mode&0022 != 0 {
		return errors.New("backend directory must be root-owned and not writable by others")
	}
	fd, err := unix.Open(socket+".qualification.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("node backend qualification unavailable: %w", err)
	}
	f := os.NewFile(uintptr(fd), "backend-qualification")
	defer f.Close()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != 0 || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0022 != 0 {
		return errors.New("untrusted backend qualification file")
	}
	var r record
	if err = json.NewDecoder(io.LimitReader(f, 4096)).Decode(&r); err != nil {
		return err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	actual, err := fingerprint(socket)
	if err != nil {
		return err
	}
	if r.Version != 1 || r.SHA256 != expected || r.BootID != string(boot) || r.Socket != actual {
		return errors.New("backend qualification is stale or mismatched")
	}
	// The record attests only to startup qualification for the current socket; the service must still be live, and callers must verify device ownership.
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer c.Close()
	after, err := fingerprint(socket)
	if err != nil {
		return err
	}
	if after != actual {
		return errors.New("backend socket replaced during verification")
	}
	return nil
}
