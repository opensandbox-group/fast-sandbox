package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// resolveKernel maps the spec kernel name to the embedded file in the
// builder image.
func resolveKernel(name string) (string, error) {
	if name == "" {
		return "", errors.New("spec.kernel is required")
	}
	path := filepath.Join(kernelDir, filepath.Base(name))
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("kernel %s not embedded in the builder image: %w", path, err)
	}
	return path, nil
}

// ensureLoopDevices creates the loop device nodes the rootfs loop mount
// needs. A CRI Pod's /dev is not populated from the host even when
// privileged (unlike docker --privileged), so the builder creates the nodes
// itself with CAP_MKNOD; losetup attaches them through the shared kernel.
// Existing nodes (e.g. mounted via hostPath) are left untouched.
func ensureLoopDevices() error {
	devices := []struct {
		path  string
		kind  string
		major int
		minor int
	}{
		{"/dev/loop-control", "c", 10, 237},
		{"/dev/loop0", "b", 7, 0},
		{"/dev/loop1", "b", 7, 1},
		{"/dev/loop2", "b", 7, 2},
		{"/dev/loop3", "b", 7, 3},
		{"/dev/loop4", "b", 7, 4},
		{"/dev/loop5", "b", 7, 5},
		{"/dev/loop6", "b", 7, 6},
		{"/dev/loop7", "b", 7, 7},
	}
	for _, device := range devices {
		if _, err := os.Stat(device.path); err == nil {
			continue
		}
		// mknod must not fail the build on read-only /dev: the mount below
		// reports the authoritative error if a loop device is missing.
		_ = exec.Command("mknod", device.path, device.kind,
			strconv.Itoa(device.major), strconv.Itoa(device.minor)).Run()
	}
	return nil
}

// stageConvert materializes the OCI layout into a sparse ext4 rootfs and
// injects execd, the optional guest init, envs, and /etc/hosts.
func stageConvert(spec apiv1alpha2.SandboxTemplateSpec, workdir string) (string, error) {
	rootfs := filepath.Join(workdir, rootfsImageName)
	layoutDir := filepath.Join(workdir, "oci-layout")
	sizeGiB, err := sizeGiB(spec.Output.RootfsSize)
	if err != nil {
		return "", err
	}
	if output, err := exec.Command(oci2rootfsBin, layoutDir, "--output", rootfs,
		"--size", fmt.Sprintf("%dG", sizeGiB), "--platform", "linux/amd64").CombinedOutput(); err != nil {
		return "", fmt.Errorf("oci2rootfs: %w: %s", err, output)
	}
	// e2fsck exit 1 means "errors corrected" (e.g. oci2rootfs writes a low
	// ref count for multi-hardlink inodes) — a repaired rootfs is success.
	if output, err := exec.Command("e2fsck", "-fy", rootfs).CombinedOutput(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return "", fmt.Errorf("e2fsck: %w: %s", err, output)
		}
		klog.V(2).InfoS("e2fsck corrected rootfs inconsistencies", "output", strings.TrimSpace(string(output)))
	}
	if err := ensureLoopDevices(); err != nil {
		return "", err
	}

	mountPoint := filepath.Join(workdir, ".rootfs-mount")
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return "", err
	}
	if output, err := exec.Command("mount", "-o", "loop", rootfs, mountPoint).CombinedOutput(); err != nil {
		return "", fmt.Errorf("loop mount rootfs: %w: %s", err, output)
	}
	if err := injectRuntime(spec, workdir, mountPoint); err != nil {
		_ = exec.Command("umount", mountPoint).Run()
		_ = os.RemoveAll(mountPoint)
		return "", err
	}
	// Unmount before verifying: e2fsck must never run on a mounted (rw)
	// filesystem.
	if output, err := exec.Command("umount", mountPoint).CombinedOutput(); err != nil {
		return "", fmt.Errorf("umount rootfs: %w: %s", err, output)
	}
	_ = os.RemoveAll(mountPoint)
	if output, err := exec.Command("e2fsck", "-fn", rootfs).CombinedOutput(); err != nil {
		return "", fmt.Errorf("e2fsck verify: %w: %s", err, output)
	}
	return rootfs, nil
}

// defaultGuestInit is injected when spec.init is empty: the readiness
// marker and heartbeat the snapshot stage depends on only exist inside the
// injected init, so every build gets one.
const defaultGuestInit = "/usr/local/sbin/sandbox-init"

func guestInitPath(spec apiv1alpha2.SandboxTemplateSpec) string {
	if spec.Init != "" {
		return spec.Init
	}
	return defaultGuestInit
}

// injectRuntime writes the execd files, the rendered guest init, the sandbox
// env, and /etc/hosts into the mounted rootfs.
func injectRuntime(spec apiv1alpha2.SandboxTemplateSpec, workdir, mountPoint string) error {
	opt := filepath.Join(mountPoint, "opt", "opensandbox")
	if err := os.MkdirAll(opt, 0o755); err != nil {
		return err
	}
	execdRoot := filepath.Join(workdir, "execd-root")
	for _, name := range []string{execdAssetName, bootstrapScriptName, prepareScriptName, "bwrap"} {
		source := filepath.Join(execdRoot, name)
		if info, err := os.Stat(source); err == nil && info.Mode().IsRegular() {
			payload, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(opt, name), payload, 0o755); err != nil { //nolint:gosec // executable file mode inside the built rootfs image
				return err
			}
		} else if spec.Execd != "" {
			// execd is requested but this file is missing from the image:
			// warn instead of failing silently.
			klog.V(2).InfoS("execd file missing from execd image", "file", name, execdAssetName, spec.Execd)
		}
	}

	// The guest init is always injected (default path when spec.init is
	// empty) at the exact path the spec declares; the boot args pass the
	// same path to the kernel. The path is validated to stay inside the
	// mount: a crafted "../.." would otherwise escape the rootfs and
	// overwrite build artifacts or container files.
	initPath := guestInitPath(spec)
	relative := filepath.Clean(strings.TrimPrefix(initPath, "/"))
	if !strings.HasPrefix(initPath, "/") || relative == ".." || strings.HasPrefix(relative, "../") {
		return fmt.Errorf("init path %q must be an absolute path inside the rootfs", initPath)
	}
	guestInit := filepath.Join(mountPoint, relative)
	if err := os.MkdirAll(filepath.Dir(guestInit), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(guestInit, []byte(renderGuestInit(spec)), 0o755); err != nil { //nolint:gosec // executable guest init inside the built rootfs image
		return err
	}

	envLines := []string{"export ENTRYPOINT=" + shellQuote(entrypointCommand(spec))}
	merged, err := mergeGuestEnvs(spec, workdir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// shellQuote, not %q: the guest init sources this file, and a
		// bare value containing $ or backticks would be expanded by sh.
		envLines = append(envLines, fmt.Sprintf("export %s=%s", name, shellQuote(merged[name])))
	}
	if err := os.WriteFile(filepath.Join(mountPoint, "etc", "sandbox-init.env"),
		[]byte(strings.Join(envLines, "\n")+"\n"), 0o600); err != nil {
		return err
	}

	hosts := filepath.Join(mountPoint, "etc", "hosts")
	if _, err := os.Stat(hosts); err != nil {
		if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n::1 localhost ip6-localhost ip6-loopback\n"), 0o644); err != nil { //nolint:gosec // guest /etc/hosts, world-readable by design
			return err
		}
	}
	// Guest DNS resolver: the guest network (address/gateway) is baked by
	// this template, and egress-managed pools redirect gateway:53 to the
	// egress DNS proxy — so the resolver is baked alongside the gateway it
	// points at (deterministic; no per-instance rootfs mutation at Create
	// time, which previously corrupted the image on the loop-mount write).
	if err := os.WriteFile(filepath.Join(mountPoint, "etc", "resolv.conf"), //nolint:gosec // guest resolv.conf, world-readable by design
		[]byte("nameserver "+bakedGuestGateway+"\n"), 0o644); err != nil {
		return err
	}
	return nil
}

// entrypointCommand renders the spec entrypoint (default tail -f /dev/null)
// as a single quoted shell command.
func entrypointCommand(spec apiv1alpha2.SandboxTemplateSpec) string {
	entrypoint := spec.Entrypoint
	if len(entrypoint) == 0 {
		entrypoint = []string{"tail", "-f", "/dev/null"}
	}
	quoted := make([]string, 0, len(entrypoint))
	for _, argument := range entrypoint {
		quoted = append(quoted, shellQuote(argument))
	}
	return strings.Join(quoted, " ")
}

// validEnvName matches POSIX shell variable names; sandbox envs are
// exported by the guest init, so a crafted name must not break it.
var validEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// readImageEnvs parses the source image's OCI Config.Env persisted by the
// pull stage; a missing file means no inherited env. Entries that are not
// KEY=VALUE with a valid shell variable name are skipped.
func readImageEnvs(workdir string) (map[string]string, error) {
	envs := map[string]string{}
	payload, err := os.ReadFile(filepath.Join(workdir, imageEnvFileName))
	if errors.Is(err, os.ErrNotExist) {
		return envs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", imageEnvFileName, err)
	}
	for _, line := range strings.Split(string(payload), "\n") {
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || !validEnvName.MatchString(name) {
			klog.V(2).InfoS("skipping unusable source-image env entry", "entry", line)
			continue
		}
		envs[name] = value
	}
	return envs, nil
}

// mergeGuestEnvs layers the spec envs over the inherited image env; a spec
// env with the same name wins, and duplicate spec names resolve to the last
// entry. Spec envs stay strict: no valueFrom, valid shell names only.
func mergeGuestEnvs(spec apiv1alpha2.SandboxTemplateSpec, workdir string) (map[string]string, error) {
	merged, err := readImageEnvs(workdir)
	if err != nil {
		return nil, err
	}
	for _, entry := range spec.Envs {
		if entry.ValueFrom != nil {
			return nil, fmt.Errorf("env %q: valueFrom is not supported in sandbox envs (only literal values)", entry.Name)
		}
		if !validEnvName.MatchString(entry.Name) {
			return nil, fmt.Errorf("env name %q is not a valid shell variable name", entry.Name)
		}
		merged[entry.Name] = entry.Value
	}
	return merged, nil
}

// renderGuestInit renders the in-guest init script: mounts, runtime bootstrap,
// entrypoint, readiness wait (probe > execd ping > warmup + healthcheck), the
// one-shot SANDBOX_READY marker, and a heartbeat loop so the host can verify
// the guest is alive after a snapshot restore (the init does not re-run after
// resume).
func renderGuestInit(spec apiv1alpha2.SandboxTemplateSpec) string {
	var readiness []string
	if spec.Readiness.Probe != "" {
		readiness = append(readiness, "READINESS_PROBE="+shellQuote(spec.Readiness.Probe))
	}
	// Always render WARMUP_SECONDS (0 included): ${VAR:-60} would otherwise
	// turn an explicit 0 into a 60s sleep, contradicting the CRD's Minimum=0
	// ("no warmup" is expressible). The CRD default of 60 only applies at
	// admission time.
	readiness = append(readiness, fmt.Sprintf("WARMUP_SECONDS=%d", spec.Readiness.WarmupSeconds))
	if spec.Readiness.HealthCheck != "" {
		readiness = append(readiness, "HEALTHCHECK="+shellQuote(spec.Readiness.HealthCheck))
	}
	readinessBlock := ""
	if len(readiness) > 0 {
		readinessBlock = "\n" + strings.Join(readiness, "\n")
	}
	return `#!/bin/sh
set -eu
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mountpoint -q /proc || mount -t proc proc /proc
mountpoint -q /sys || mount -t sysfs sysfs /sys
mountpoint -q /dev || mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /dev/shm /run /tmp
mountpoint -q /dev/pts || mount -t devpts devpts /dev/pts
mountpoint -q /run || mount -t tmpfs tmpfs /run
mountpoint -q /tmp || mount -t tmpfs tmpfs /tmp
ip link set lo up 2>/dev/null || true
exec </dev/console >/dev/console 2>&1
hostname sandbox
[ -f /etc/sandbox-init.env ] && . /etc/sandbox-init.env
if [ -x /opt/opensandbox/bootstrap.sh ]; then
  setsid /bin/sh /opt/opensandbox/bootstrap.sh &
fi
if [ -n "${ENTRYPOINT:-}" ]; then
  setsid /bin/sh -c "$ENTRYPOINT" &
fi
` + readinessBlock + `
if ! command -v nc >/dev/null 2>&1; then
  echo "SANDBOX_STARTUP_FAILED nc_missing"
  exit 1
fi
ready_tcp() { host=${1#tcp://}; host=${host%%:*}; port=${1##*:}; nc -w 1 "$host" "$port" </dev/null >/dev/null 2>&1; }
ready_cmd() { /bin/sh -c "${1#cmd://}" >/dev/null 2>&1; }
ready_execd_ping() { nc -w 1 127.0.0.1 44772 </dev/null >/dev/null 2>&1; }
if [ -n "${READINESS_PROBE:-}" ]; then
  case "$READINESS_PROBE" in
    tcp://*) until ready_tcp "$READINESS_PROBE"; do sleep 1; done ;;
    cmd://*) until ready_cmd "$READINESS_PROBE"; do sleep 1; done ;;
    *) echo "SANDBOX_STARTUP_FAILED invalid_probe"; exit 1 ;;
  esac
elif [ -x /opt/opensandbox/execd ]; then
  until ready_execd_ping; do sleep 1; done
else
  sleep "${WARMUP_SECONDS:-60}"
  if [ -n "${HEALTHCHECK:-}" ]; then
    until /bin/sh -c "$HEALTHCHECK" >/dev/null 2>&1; do sleep 1; done
  fi
fi
echo SANDBOX_READY
while true; do echo SANDBOX_HEARTBEAT; sleep 5; done
`
}

// sizeGiB converts the rootfsSize quantity (e.g. "30Gi") into the GiB value
// passed to oci2rootfs --size. oci2rootfs takes SI units (10^9), so the
// result is rounded UP: the produced rootfs always has at least the declared
// capacity (30Gi declared → 33G passed → ~30.7 GiB actual). Unparsable input
// fails the build instead of silently producing a wrong-size rootfs.
func sizeGiB(quantity string) (int, error) {
	if quantity == "" {
		return 30, nil
	}
	parsed, err := resource.ParseQuantity(quantity)
	if err != nil {
		return 0, fmt.Errorf("invalid rootfsSize %q: %w", quantity, err)
	}
	bytes := parsed.Value()
	if bytes < 1024*1024*1024 {
		return 1, nil
	}
	siGiB := (bytes + 1e9 - 1) / 1e9
	if siGiB > int64(^uint(0)>>1) {
		return 0, fmt.Errorf("rootfsSize %q out of range", quantity)
	}
	return int(siGiB), nil
}
