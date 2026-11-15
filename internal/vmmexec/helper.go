package vmmexec

import (
	"fmt"
	"os"
	"path/filepath"
)

// The helper binary ships in the Fastlet image, not in the managed Firecracker
// versions directory, so the paired-binary verification contract and version
// selection of the managed VMM delivery are untouched.

// DefaultHelperPath is where the Fastlet image installs the helper. The
// basename is contractual: the jailer derives the jail root
// (<chroot-base>/<exec-file-basename>/<id>/root) from the exec-file
// basename, and every Fastlet jail-path consumer derives the same layout
// from the Firecracker binary basename — keeping "firecracker" makes the
// jail path, the in-jail .pid file name, and the NodeJanitor argv match
// exactly what they were before the helper existed.
const DefaultHelperPath = "/usr/local/libexec/fast-sandbox/vmm-capdrop/firecracker"

// HelperPathEnv overrides the installed helper location. It exists for
// development and test environments that run the driver from a source tree
// (for example the Firecracker runtime E2E, which builds the helper from
// this module); it is not a knob to disable or relocate the drop in
// production — whatever binary the path holds performs the same hardcoded
// drop set, and a missing helper fails the launch.
const HelperPathEnv = "FAST_SANDBOX_VMM_CAPDROP_HELPER"

// HelperBasename is the required basename of the helper (see
// DefaultHelperPath).
const HelperBasename = "firecracker"

// ResolveHelperPath returns the installed helper path and validates that it
// satisfies the launch contract: an absolute path with the "firecracker"
// basename, pointing at a regular executable file. The path is resolved
// through symlinks and the CANONICAL path is returned (and validated): the
// jailer canonicalizes the exec file before deriving the jail basename, so
// a "firecracker" symlink pointing at a differently named binary would
// otherwise pass a plain basename check while the jailer enters a jail at
// <base>/<target-basename>/<id>/root — away from every Fastlet-computed
// <base>/firecracker/<id>/root path. A missing or invalid helper is a loud
// error — the VMM must never launch with the full Pod capability set
// because the image forgot to ship the helper.
func ResolveHelperPath() (string, error) {
	path := os.Getenv(HelperPathEnv)
	if path == "" {
		path = DefaultHelperPath
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("capability-drop helper path %q must be absolute", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("capability-drop helper %s is not installed: %w (the Fastlet image must ship it at %s)", path, err, DefaultHelperPath)
	}
	if filepath.Base(resolved) != HelperBasename {
		return "", fmt.Errorf("capability-drop helper %s resolves to %q, which must carry the %q basename: the jailer derives the jail root from the canonicalized exec-file basename", path, resolved, HelperBasename)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("capability-drop helper %s is not usable: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("capability-drop helper %s must be a regular file", resolved)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("capability-drop helper %s is not executable", resolved)
	}
	return resolved, nil
}
