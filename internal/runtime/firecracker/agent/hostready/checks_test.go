package hostready

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProbes assembles a Probes with every observation controlled by the
// test. StateRoot/AssetsDir paths default to exec-friendly values.
type fakeProbes struct {
	openDevice    func(path string) error
	statFS        func(path string) (FsStat, error)
	kernelRelease func() (string, error)
	cpuFlags      func() ([]string, error)
	arch          string
	memAvailable  func() (int64, error)
	reflinkProbe  func(dir string) (bool, error)
	verifyBinary  func(path string) error
}

func (f fakeProbes) probes(assetsDir string) Probes {
	verify := f.verifyBinary
	if verify == nil {
		verify = func(path string) error {
			if filepath.Dir(path) == assetsDir {
				return nil
			}
			return errors.New("not installed")
		}
	}
	return Probes{
		OpenDevice:    f.openDevice,
		StatFS:        f.statFS,
		KernelRelease: f.kernelRelease,
		CPUFlags:      f.cpuFlags,
		Arch:          func() string { return f.arch },
		MemAvailable:  f.memAvailable,
		ReflinkProbe:  f.reflinkProbe,
		VerifyBinary:  verify,
	}
}

func healthyProbes(t *testing.T) (fakeProbes, string) {
	t.Helper()
	assets := filepath.Join(t.TempDir(), "fc")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "vmlinux.bin"), []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fakeProbes{
		openDevice: func(string) error { return nil },
		statFS: func(string) (FsStat, error) {
			return FsStat{Type: "xfs", FreeBytes: 100 << 30, TotalBytes: 200 << 30}, nil
		},
		kernelRelease: func() (string, error) { return "6.1.176-fast-sandbox", nil },
		cpuFlags:      func() ([]string, error) { return []string{"fpu", "vmx", "sse"}, nil },
		arch:          "amd64",
		memAvailable:  func() (int64, error) { return 8 << 30, nil },
		reflinkProbe:  func(string) (bool, error) { return true, nil },
	}, assets
}

func checkByName(report Report, name string) Check {
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	return Check{Name: name, Detail: "<missing>"}
}

func TestRunChecksHealthyHost(t *testing.T) {
	probes, assets := healthyProbes(t)
	stateRoot := t.TempDir()
	report := RunChecks(CheckConfig{StateRoot: stateRoot, AssetsDir: assets}, probes.probes(assets))
	if !report.Ready {
		t.Fatalf("expected a ready report, got: %s", report.String())
	}
	for _, name := range []string{"cpu-arch", "kernel-version", "kvm-device", "nested-virtualization", "net-tun", "stateroot-dirs", "stateroot-filesystem", "fc-assets"} {
		if checkByName(report, name).Status != StatusPass {
			t.Fatalf("expected %s to pass: %s", name, report.String())
		}
	}
	// The storage initialization side effect: the layout exists.
	for _, sub := range stateRootSubdirs {
		info, err := os.Stat(filepath.Join(stateRoot, sub))
		if err != nil || !info.IsDir() {
			t.Fatalf("expected %s to be created: err=%v", sub, err)
		}
	}
}

func TestRunChecksFailuresBlockReady(t *testing.T) {
	probes, assets := healthyProbes(t)
	probes.openDevice = func(path string) error {
		if path == "/dev/kvm" {
			return errors.New("device does not exist")
		}
		return nil
	}
	report := RunChecks(CheckConfig{StateRoot: t.TempDir(), AssetsDir: assets}, probes.probes(assets))
	if report.Ready {
		t.Fatalf("a missing KVM device must block readiness: %s", report.String())
	}
	if got := checkByName(report, "kvm-device"); got.Status != StatusFail {
		t.Fatalf("kvm-device must fail, got %s", got.Status)
	}
}

func TestRunChecksKernelThresholds(t *testing.T) {
	probes, assets := healthyProbes(t)
	cases := []struct {
		release string
		want    Status
	}{
		{"5.10.0", StatusPass},
		{"4.14.100", StatusFail},
		{"5.4.0", StatusFail},
		{"6.1.176", StatusPass},
		{"garbage", StatusWarn},
	}
	for _, testCase := range cases {
		probes.kernelRelease = func() (string, error) { return testCase.release, nil }
		report := RunChecks(CheckConfig{StateRoot: t.TempDir(), AssetsDir: assets}, probes.probes(assets))
		if got := checkByName(report, "kernel-version").Status; got != testCase.want {
			t.Fatalf("kernel %q: expected %s, got %s (%s)", testCase.release, testCase.want, got, report.String())
		}
	}
}

func TestRunChecksWarningsKeepReady(t *testing.T) {
	probes, assets := healthyProbes(t)
	probes.cpuFlags = func() ([]string, error) { return []string{"fpu"}, nil }
	probes.memAvailable = func() (int64, error) { return 512 << 20, nil }
	probes.reflinkProbe = func(string) (bool, error) { return false, nil }
	probes.statFS = func(string) (FsStat, error) {
		return FsStat{Type: "ext4", FreeBytes: 20 << 30, TotalBytes: 40 << 30}, nil
	}
	report := RunChecks(CheckConfig{StateRoot: t.TempDir(), AssetsDir: assets}, probes.probes(assets))
	if !report.Ready {
		t.Fatalf("warnings must not block readiness: %s", report.String())
	}
	for _, name := range []string{"nested-virtualization", "memory-available", "stateroot-filesystem"} {
		if got := checkByName(report, name); got.Status != StatusWarn {
			t.Fatalf("expected %s to warn, got %s: %s", name, got.Status, got.Detail)
		}
	}
}

func TestRunChecksStateRootFailures(t *testing.T) {
	probes, assets := healthyProbes(t)
	report := RunChecks(CheckConfig{StateRoot: "/proc/definitely/not/writable", AssetsDir: assets}, probes.probes(assets))
	if report.Ready {
		t.Fatalf("an unwritable StateRoot must block readiness: %s", report.String())
	}
	if got := checkByName(report, "stateroot-dirs"); got.Status != StatusFail {
		t.Fatalf("stateroot-dirs must fail, got %s", got.Status)
	}

	probes, assets = healthyProbes(t)
	probes.statFS = func(string) (FsStat, error) { return FsStat{}, errors.New("statfs broke") }
	report = RunChecks(CheckConfig{StateRoot: t.TempDir(), AssetsDir: assets}, probes.probes(assets))
	if report.Ready {
		t.Fatalf("a broken statfs must block readiness: %s", report.String())
	}

	probes, assets = healthyProbes(t)
	probes.statFS = func(string) (FsStat, error) {
		return FsStat{Type: "xfs", FreeBytes: 1 << 30, TotalBytes: 10 << 30}, nil
	}
	report = RunChecks(CheckConfig{StateRoot: t.TempDir(), AssetsDir: assets}, probes.probes(assets))
	if report.Ready {
		t.Fatalf("insufficient free space must block readiness: %s", report.String())
	}
}

func TestRunChecksAssetsMissing(t *testing.T) {
	probes, assets := healthyProbes(t)
	missing := filepath.Join(t.TempDir(), "assets")
	report := RunChecks(CheckConfig{StateRoot: t.TempDir(), AssetsDir: missing}, probes.probes(assets))
	if report.Ready {
		t.Fatalf("missing firecracker assets must block readiness: %s", report.String())
	}
	if got := checkByName(report, "fc-assets"); got.Status != StatusFail || !strings.Contains(got.Detail, "firecracker --version failed") {
		t.Fatalf("unexpected fc-assets failure: %+v", got)
	}
}

func TestRunChecksArm64(t *testing.T) {
	probes, assets := healthyProbes(t)
	probes.arch = "arm64"
	probes.cpuFlags = func() ([]string, error) { return []string{"fp", "asimd"}, nil }
	report := RunChecks(CheckConfig{StateRoot: t.TempDir(), AssetsDir: assets}, probes.probes(assets))
	if !report.Ready {
		t.Fatalf("arm64 without vmx/svm must stay ready: %s", report.String())
	}
	if got := checkByName(report, "nested-virtualization"); got.Status != StatusWarn {
		t.Fatalf("expected the nested check to warn on arm64, got %s", got.Status)
	}
}

func TestParseKernelRelease(t *testing.T) {
	cases := []struct {
		release      string
		major, minor int
		ok           bool
	}{
		{"6.1.176-fast-sandbox", 6, 1, true},
		{"4.14.1", 4, 14, true},
		{"6.8", 6, 8, true},
		{"linux", 0, 0, false},
		{"6", 0, 0, false},
	}
	for _, testCase := range cases {
		major, minor, ok := parseKernelRelease(testCase.release)
		if major != testCase.major || minor != testCase.minor || ok != testCase.ok {
			t.Fatalf("parseKernelRelease(%q) = (%d,%d,%v), want (%d,%d,%v)",
				testCase.release, major, minor, ok, testCase.major, testCase.minor, testCase.ok)
		}
	}
}

func TestParseBytes(t *testing.T) {
	cases := []struct {
		value string
		want  int64
	}{
		{"10GiB", 10 << 30},
		{"512MiB", 512 << 20},
		{"2GiB", 2 << 30},
		{"1KiB", 1 << 10},
		{"4096", 4096},
		{"1.5GiB", int64(1.5 * (1 << 30))},
	}
	for _, testCase := range cases {
		got, err := ParseBytes(testCase.value)
		if err != nil || got != testCase.want {
			t.Fatalf("ParseBytes(%q) = %d, %v; want %d", testCase.value, got, err, testCase.want)
		}
	}
	for _, value := range []string{"", "abc", "10TB"} {
		if _, err := ParseBytes(value); err == nil {
			t.Fatalf("ParseBytes(%q) must fail", value)
		}
	}
}
