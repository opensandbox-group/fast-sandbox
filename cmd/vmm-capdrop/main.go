// Command vmm-capdrop is the Firecracker VMM capability-drop exec helper
// (FSEP-0003). The jailer execs it as the final step of the jail setup (it
// is installed with the "firecracker" basename and passed to the jailer as
// --exec-file); the helper drops the fixed capability set on its locked
// thread, sets PR_SET_NO_NEW_PRIVS, self-verifies every promised set, and
// execs the real Firecracker binary — in jailer mode at the fixed in-jail
// convention /vmm/firecracker with argv forwarded unchanged, in direct-launch
// mode at the path named by "--exec <path>". Any failure exits loudly so a
// VM create never proceeds with unintended privileges.
package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"

	"fast-sandbox/internal/vmmexec"
)

func main() {
	// Capability sets, PR_CAPBSET_DROP and PR_SET_NO_NEW_PRIVS apply to the
	// calling thread, and CGO_ENABLED=0 does not make a Go program
	// single-threaded. Pin the goroutine before the first privilege
	// transition and keep the lock through the final execve, so the thread
	// that becomes the VMM received every restriction (the execed process
	// inherits the surviving thread's credentials; the kernel destroys all
	// other threads at execve).
	runtime.LockOSThread()
	if err := vmmexec.Run(os.Args, os.Environ(), runtime.LockOSThread, syscall.Exec); err != nil {
		fmt.Fprintf(os.Stderr, "vmm-capdrop: %v\n", err)
		os.Exit(1)
	}
}
