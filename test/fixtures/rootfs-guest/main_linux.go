//go:build linux

// This PID 1 is only for disposable rootfs E2E: a real root filesystem, an independent production helper, and a continuous fsync writer.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	for _, m := range []struct{ path, kind string }{{"/proc", "proc"}, {"/sys", "sysfs"}, {"/dev", "devtmpfs"}, {"/run", "tmpfs"}} {
		if err := os.MkdirAll(m.path, 0755); err != nil {
			panic(err)
		}
		if err := unix.Mount(m.kind, m.path, m.kind, 0, ""); err != nil && !(m.path == "/dev" && err == unix.EBUSY) {
			panic(err)
		}
	}
	raw, err := os.ReadFile("/boot-count")
	if err != nil && !os.IsNotExist(err) {
		panic(err)
	}
	boot := 0
	if len(raw) > 0 {
		boot, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			panic(err)
		}
	}
	boot++
	writeSync("/boot-count", boot)
	cmd := exec.Command("/fsb-guest-control", "--mount", "/", "--port", "1234")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		panic(err)
	}
	go func() { panic(fmt.Sprintf("helper exited: %v", cmd.Wait())) }()
	fmt.Printf("FSB_ROOT_BOOT %d\nFSB_SMOKE_READY\n", boot)
	for i := 1; ; i++ {
		writeSync("/p0-write", i)
		fmt.Printf("P0_WRITE_DONE %d\n", i)
		time.Sleep(10 * time.Millisecond)
	}
}
func writeSync(path string, value int) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	if _, err = fmt.Fprintf(f, "%d\n", value); err != nil {
		panic(err)
	}
	if err = f.Sync(); err != nil {
		panic(err)
	}
	if err = f.Close(); err != nil {
		panic(err)
	}
}
