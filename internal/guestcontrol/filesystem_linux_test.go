//go:build linux && (amd64 || arm64)

package guestcontrol

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This opt-in test creates its own loopback ext4 mount and never freezes the machine's root filesystem.
// Requires root, loop devices, and mount permissions.
func TestLinuxFreezeBlocksWritesAndThaws(t *testing.T) {
	if os.Getenv("FSB_TEST_GUEST_FREEZE") != "1" {
		t.Skip("set FSB_TEST_GUEST_FREEZE=1 on a disposable Linux test VM")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root")
	}
	for _, tool := range []string{"mkfs.ext4", "mount", "umount"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	image := filepath.Join(dir, "disk.img")
	mount := filepath.Join(dir, "mount")
	f, err := os.Create(image)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(64 << 20); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	command := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
	}
	command("mkfs.ext4", "-q", "-F", image)
	if err = os.Mkdir(mount, 0700); err != nil {
		t.Fatal(err)
	}
	command("mount", "-o", "loop", image, mount)
	defer func() {
		if out, err := exec.Command("umount", mount).CombinedOutput(); err != nil {
			t.Errorf("unmount: %v: %s", err, out)
		}
	}()
	fs, err := OpenFilesystem(mount)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	// Perform emergency cleanup on test failure; this descriptor refers only to the newly created test filesystem.
	// The lease also triggers thaw if the test times out.
	defer fs.Thaw()
	if err = os.Mkdir(filepath.Join(mount, "subdir"), 0700); err != nil {
		t.Fatal(err)
	}
	if wrong, err := OpenFilesystem(filepath.Join(mount, "subdir")); err == nil {
		wrong.Close()
		t.Fatal("subdirectory accepted as mount")
	}
	m, err := NewManager(fs, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Freeze(1, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, func(s Status) bool { return s.State == Held })
	done := make(chan error, 1)
	go func() { done <- os.WriteFile(filepath.Join(mount, "after-thaw"), []byte("writable"), 0600) }()
	select {
	case err := <-done:
		t.Fatalf("write completed while frozen: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err = m.Thaw(1); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, func(s Status) bool { return s.State == Released && !s.CleanupPending })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write did not resume after thaw")
	}
	data, err := os.ReadFile(filepath.Join(mount, "after-thaw"))
	if err != nil || string(data) != "writable" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}
