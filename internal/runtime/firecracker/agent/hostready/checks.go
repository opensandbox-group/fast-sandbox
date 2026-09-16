package hostready

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// FsStat is the filesystem summary of one directory.
type FsStat struct {
	// Type is the filesystem name ("xfs", "btrfs", "ext4", ...); empty
	// when the platform cannot report it.
	Type string
	// FreeBytes is the space available to unprivileged writes.
	FreeBytes int64
	// TotalBytes is the filesystem size.
	TotalBytes int64
}

// Probes abstracts every host observation the checks make. The zero value
// selects the real platform probes; tests inject fakes so the suite runs on
// any OS without /dev/kvm or /proc.
type Probes struct {
	// OpenDevice opens a device path for reading and writing (KVM, TUN).
	OpenDevice func(path string) error
	// StatFS summarizes the filesystem holding path.
	StatFS func(path string) (FsStat, error)
	// KernelRelease returns uname -r ("6.1.176").
	KernelRelease func() (string, error)
	// CPUFlags returns the CPU capability flags (vmx, svm, ...).
	CPUFlags func() ([]string, error)
	// Arch returns the machine architecture ("amd64", "arm64").
	Arch func() string
	// MemAvailable returns the available memory in bytes.
	MemAvailable func() (int64, error)
	// ReflinkProbe attempts a FICLONE inside dir and reports whether the
	// filesystem supports copy-on-write reflinks.
	ReflinkProbe func(dir string) (bool, error)
	// VerifyBinary runs "<path> --version" and reports an execution
	// failure (the Firecracker asset verification).
	VerifyBinary func(path string) error
}

// DefaultProbes returns the real platform probes.
func DefaultProbes() Probes {
	return Probes{
		OpenDevice:    openDeviceRW,
		StatFS:        statFS,
		KernelRelease: kernelRelease,
		CPUFlags:      cpuFlags,
		Arch:          func() string { return runtime.GOARCH },
		MemAvailable:  memAvailable,
		ReflinkProbe:  reflinkProbe,
		VerifyBinary:  verifyBinary,
	}
}

// verifyBinary is the default VerifyBinary probe.
func verifyBinary(path string) error {
	return exec.Command(path, "--version").Run()
}

// CheckConfig carries the paths and thresholds the checks evaluate.
// Zero thresholds take the defaults.
type CheckConfig struct {
	// StateRoot is the agent state root (directories are created here).
	StateRoot string
	// AssetsDir is the Firecracker asset directory (binary/jailer/kernel).
	AssetsDir string
	// MinFreeBytes is the minimum free space on the StateRoot filesystem.
	MinFreeBytes int64
	// MinMemBytes is the minimum available memory.
	MinMemBytes int64
}

// Defaults for the thresholds (overridable per environment).
const (
	DefaultMinFreeBytes = int64(10) << 30 // 10GiB
	DefaultMinMemBytes  = int64(2) << 30  // 2GiB
	// minKernelMajor/minor is the host kernel floor: Linux 5.10 (LTS,
	// the baseline Firecracker CI validates).
	minKernelMajor, minKernelMinor = 5, 10
)

// stateRootSubdirs is the directory layout the agent and the driver expect
// under the StateRoot (the pull cache, the DART arenas, the jailer chroot
// base). Created on every pass; existing dirs are a no-op.
var stateRootSubdirs = []string{"images", "cache", "jails"}

// RunChecks executes every host check in order and returns the report.
// It creates the StateRoot layout as a side effect (the "storage
// initialization" duty); a read-only or unwritable StateRoot surfaces as a
// failed stateroot-dirs check. The Firecracker assets are only verified
// here — InstallAssets performs the download when the node manager runs.
func RunChecks(config CheckConfig, probes Probes) Report {
	probes = withDefaults(probes)
	if config.MinFreeBytes <= 0 {
		config.MinFreeBytes = DefaultMinFreeBytes
	}
	if config.MinMemBytes <= 0 {
		config.MinMemBytes = DefaultMinMemBytes
	}
	report := Report{}
	checkArch(&report, probes)
	checkKernel(&report, probes)
	checkDevice(&report, probes, "kvm-device", "/dev/kvm")
	checkNested(&report, probes)
	checkDevice(&report, probes, "net-tun", "/dev/net/tun")
	checkMemory(&report, probes, config)
	checkStateRootDirs(&report, config)
	checkStateRootFS(&report, probes, config)
	checkAssets(&report, config, probes)
	report.summarize()
	return report
}

func withDefaults(probes Probes) Probes {
	fallback := DefaultProbes()
	if probes.OpenDevice == nil {
		probes.OpenDevice = fallback.OpenDevice
	}
	if probes.StatFS == nil {
		probes.StatFS = fallback.StatFS
	}
	if probes.KernelRelease == nil {
		probes.KernelRelease = fallback.KernelRelease
	}
	if probes.CPUFlags == nil {
		probes.CPUFlags = fallback.CPUFlags
	}
	if probes.Arch == nil {
		probes.Arch = fallback.Arch
	}
	if probes.MemAvailable == nil {
		probes.MemAvailable = fallback.MemAvailable
	}
	if probes.ReflinkProbe == nil {
		probes.ReflinkProbe = fallback.ReflinkProbe
	}
	if probes.VerifyBinary == nil {
		probes.VerifyBinary = fallback.VerifyBinary
	}
	return probes
}

// checkArch verifies the machine architecture ships Firecracker assets
// (x86_64 and aarch64 only).
func checkArch(report *Report, probes Probes) {
	switch arch := probes.Arch(); arch {
	case "amd64":
		report.pass("cpu-arch", "amd64 (Firecracker x86_64 assets)")
	case "arm64":
		report.pass("cpu-arch", "arm64 (Firecracker aarch64 assets)")
	default:
		report.fail("cpu-arch", "unsupported architecture "+arch+": no Firecracker assets")
	}
}

// checkKernel verifies the host kernel meets the Firecracker floor (4.14).
func checkKernel(report *Report, probes Probes) {
	release, err := probes.KernelRelease()
	if err != nil {
		report.fail("kernel-version", "uname failed: "+err.Error())
		return
	}
	major, minor, ok := parseKernelRelease(release)
	if !ok {
		report.warn("kernel-version", "unparseable release "+release)
		return
	}
	detail := fmt.Sprintf("Linux %d.%d (%s)", major, minor, release)
	if major < minKernelMajor || (major == minKernelMajor && minor < minKernelMinor) {
		report.fail("kernel-version", detail+fmt.Sprintf(": Firecracker requires >= %d.%d", minKernelMajor, minKernelMinor))
		return
	}
	report.pass("kernel-version", detail)
}

// parseKernelRelease extracts the (major, minor) prefix of a kernel
// release string, tolerating vendor suffixes ("6.1.176-fast-sandbox").
func parseKernelRelease(release string) (int, int, bool) {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// checkDevice verifies one device node exists and opens read-write.
func checkDevice(report *Report, probes Probes, name, path string) {
	if err := probes.OpenDevice(path); err != nil {
		report.fail(name, path+" unusable: "+err.Error())
		return
	}
	report.pass(name, path+" opens read-write")
}

// checkNested reports the Intel/AMD nested-virtualization capability. A
// missing flag is a warning: Firecracker still runs on bare metal without
// nested support, but inside a VM (the common CI shape) the flag decides
// usability.
func checkNested(report *Report, probes Probes) {
	flags, err := probes.CPUFlags()
	if err != nil {
		report.warn("nested-virtualization", "cpu flags unavailable: "+err.Error())
		return
	}
	set := make(map[string]bool, len(flags))
	for _, flag := range flags {
		set[flag] = true
	}
	switch {
	case set["vmx"]:
		report.pass("nested-virtualization", "vmx (Intel VT-x)")
	case set["svm"]:
		report.pass("nested-virtualization", "svm (AMD-V)")
	default:
		report.warn("nested-virtualization", "no vmx/svm flag: bare metal is fine, nested VMs cannot run Firecracker")
	}
}

// checkMemory warns when the available memory is below the threshold (a
// transient condition is not a permanent launch blocker, so it never fails
// the report).
func checkMemory(report *Report, probes Probes, config CheckConfig) {
	available, err := probes.MemAvailable()
	if err != nil {
		report.warn("memory-available", "meminfo unavailable: "+err.Error())
		return
	}
	detail := fmt.Sprintf("%d MiB available (minimum %d MiB)", available>>20, config.MinMemBytes>>20)
	if available < config.MinMemBytes {
		report.warn("memory-available", detail)
		return
	}
	report.pass("memory-available", detail)
}

// checkStateRootDirs materializes the StateRoot layout (the storage
// initialization duty) and fails when the host filesystem refuses.
func checkStateRootDirs(report *Report, config CheckConfig) {
	created := 0
	for _, sub := range append([]string{""}, stateRootSubdirs...) {
		path := filepath.Join(config.StateRoot, sub)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			report.fail("stateroot-dirs", "stat "+path+": "+err.Error())
			return
		}
		if err := os.MkdirAll(path, 0o750); err != nil {
			report.fail("stateroot-dirs", "mkdir "+path+": "+err.Error())
			return
		}
		created++
	}
	detail := fmt.Sprintf("%s {%s} ready", config.StateRoot, strings.Join(stateRootSubdirs, ","))
	if created > 0 {
		detail += fmt.Sprintf(" (%d created)", created)
	}
	report.pass("stateroot-dirs", detail)
}

// checkStateRootFS reports the StateRoot filesystem type, verifies the free
// space floor, and probes reflink support (CoW rootfs copies turn a ~2.5s
// clone into ~1ms on XFS/btrfs).
func checkStateRootFS(report *Report, probes Probes, config CheckConfig) {
	fs, err := probes.StatFS(config.StateRoot)
	if err != nil {
		report.fail("stateroot-filesystem", "statfs "+config.StateRoot+": "+err.Error())
		return
	}
	free := fmt.Sprintf("%d GiB free of %d GiB", fs.FreeBytes>>30, fs.TotalBytes>>30)
	if fs.FreeBytes < config.MinFreeBytes {
		report.fail("stateroot-filesystem", fmt.Sprintf("type %s, %s (minimum %d GiB)", fs.Type, free, config.MinFreeBytes>>30))
		return
	}
	reflinked, reflinkErr := probes.ReflinkProbe(config.StateRoot)
	switch {
	case reflinkErr != nil:
		report.warn("stateroot-filesystem", fmt.Sprintf("type %s, %s; reflink probe failed: %s", fs.Type, free, reflinkErr.Error()))
	case reflinked:
		report.pass("stateroot-filesystem", fmt.Sprintf("type %s, %s; reflink CoW supported", fs.Type, free))
	default:
		report.warn("stateroot-filesystem", fmt.Sprintf("type %s, %s; no reflink (rootfs copies fall back to full writes)", fs.Type, free))
	}
}

// checkAssets verifies the installed Firecracker binaries run and the
// kernel blob is non-empty. The manager installs missing assets before
// running the checks; in check-only mode (standalone script, local agent)
// a missing install surfaces here.
func checkAssets(report *Report, config CheckConfig, probes Probes) {
	binary := filepath.Join(config.AssetsDir, "firecracker")
	jailer := filepath.Join(config.AssetsDir, "jailer")
	kernel := filepath.Join(config.AssetsDir, "vmlinux.bin")
	for _, path := range []string{binary, jailer} {
		if err := probes.VerifyBinary(path); err != nil {
			report.fail("fc-assets", path+" --version failed: "+err.Error())
			return
		}
	}
	info, err := os.Stat(kernel)
	if err != nil || info.Size() == 0 {
		report.fail("fc-assets", kernel+" missing or empty")
		return
	}
	report.pass("fc-assets", "firecracker, jailer and vmlinux.bin verified in "+config.AssetsDir)
}

// ParseBytes parses a human byte size ("10GiB", "512MiB", "1GiB", bytes).
func ParseBytes(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("empty size")
	}
	multipliers := []struct {
		suffix string
		shift  uint
	}{
		{"gib", 30}, {"mib", 20}, {"kib", 10},
		{"gb", 30}, {"mb", 20}, {"kb", 10},
		{"g", 30}, {"m", 20}, {"k", 10},
	}
	lower := strings.ToLower(value)
	for _, multiplier := range multipliers {
		if strings.HasSuffix(lower, multiplier.suffix) {
			number, err := strconv.ParseFloat(strings.TrimSuffix(lower, multiplier.suffix), 64)
			if err != nil {
				return 0, fmt.Errorf("parse %q: %w", value, err)
			}
			return int64(number * float64(int64(1)<<multiplier.shift)), nil
		}
	}
	number, err := strconv.ParseInt(lower, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: expected a byte size like 10GiB", value)
	}
	return number, nil
}
