package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
)

func TestBuildProgress(t *testing.T) {
	for _, buildErr := range []error{nil, errors.New("build failed")} {
		status := "completed"
		if buildErr != nil {
			status = "failed"
		}
		t.Run(status, func(t *testing.T) {
			var output bytes.Buffer
			progress := &buildProgress{output: &output}
			progress.start("resolving source image")
			require.Contains(t, output.String(), `phase="resolving source image" status=started`)
			require.NotContains(t, output.String(), "status=completed")
			progress.start("preparing build rootfs")
			progress.finish(buildErr)
			progress.finish(nil)

			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			require.Len(t, lines, 4)
			require.Contains(t, lines[1], `phase="resolving source image" status=completed`)
			require.Contains(t, lines[2], `phase="preparing build rootfs" status=started`)
			require.Contains(t, lines[3], `phase="preparing build rootfs" status=`+status)
			for _, line := range lines {
				timestamp, _, ok := strings.Cut(line, " ")
				require.True(t, ok)
				_, err := time.Parse(time.RFC3339Nano, timestamp)
				require.NoError(t, err)
				require.True(t, strings.HasSuffix(timestamp, "Z"))
				require.Contains(t, line, "[build ")
				require.Contains(t, line, " duration=")
			}
		})
	}
}

func TestBuildProgressDurations(t *testing.T) {
	var output bytes.Buffer
	started := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.FixedZone("UTC+2", 2*60*60))
	progress := &buildProgress{
		output: &output, started: started, phaseStarted: started.Add(2 * time.Second), phase: "pulling source image",
	}
	progress.log(started.Add(4250*time.Millisecond), "completed")
	require.Equal(t, "2026-01-02T01:04:09.25Z [build 4.25s] phase=\"pulling source image\" status=completed duration=2.25s\n", output.String())
}

func TestBuildProgressStopsAtFailedPhase(t *testing.T) {
	for _, invalid := range []bool{true, false} {
		t.Run(strconv.FormatBool(invalid), func(t *testing.T) {
			options := validBuildOptions(t)
			previous := global
			t.Cleanup(func() { global = previous })
			global.ublkdSocket = filepath.Join(t.TempDir(), "missing.sock")
			failedPhase := "preparing build workspace"
			if invalid {
				options.diskSizeGiB = 0
				failedPhase = "validating build inputs"
			}
			output := captureCommandStdout(t, func() {
				require.Error(t, runBuild(context.Background(), options))
			})
			require.Contains(t, output, fmt.Sprintf("phase=%q status=started", failedPhase))
			require.Contains(t, output, fmt.Sprintf("phase=%q status=failed", failedPhase))
			require.NotContains(t, output, fmt.Sprintf("phase=%q status=completed", failedPhase))
			require.NotContains(t, output, "resolving source image")
			require.NotContains(t, output, "built template pair")
		})
	}
}

func captureCommandStdout(t *testing.T, run func()) string {
	t.Helper()
	output, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	require.NoError(t, err)
	previous := os.Stdout
	os.Stdout = output
	defer func() {
		os.Stdout = previous
		_ = output.Close()
	}()
	run()
	data, err := os.ReadFile(output.Name())
	require.NoError(t, err)
	return string(data)
}

func TestParseOutput(t *testing.T) {
	names, err := parseOutput("redis:6.2.1")
	if err != nil {
		t.Fatalf("parseOutput: %v", err)
	}
	if names.repo != "redis" || names.templateID != "6.2.1" {
		t.Errorf("names = %+v", names)
	}
	if got := names.rootfsName(); got != "redis:6.2.1_rootfs" {
		t.Errorf("rootfsName = %q", got)
	}
	if got := names.snapfilesName(); got != "redis:6.2.1_snapfiles" {
		t.Errorf("snapfilesName = %q", got)
	}

	for _, invalid := range []string{"redis", ":6.2.1", "redis:", "registry.io/redis:6.2.1"} {
		if _, err := parseOutput(invalid); err == nil {
			t.Errorf("parseOutput(%q) must fail", invalid)
		}
	}
}

// validBuildOptions returns options that pass validation against a fake
// kernel file in a temp directory.
func validBuildOptions(t *testing.T) *buildOptions {
	t.Helper()
	kernel := filepath.Join(t.TempDir(), "vmlinux-5.10.0")
	if err := os.WriteFile(kernel, []byte("fake kernel"), 0o640); err != nil {
		t.Fatal(err)
	}
	return &buildOptions{
		source:      "registry.hub.docker.com/overlaybd/redis:6.2.1_obd",
		output:      "redis:6.2.1",
		kernel:      kernel,
		vcpu:        1,
		memoryMB:    1024,
		diskSizeGiB: defaultBuildDiskSizeGiB,
		initPath:    defaultInitPath,
	}
}

func TestBuildDiskSizeFlag(t *testing.T) {
	command := newBuildCommand()
	flag := command.Flags().Lookup("disk-size-gb")
	if flag == nil || flag.DefValue != "20" {
		t.Fatalf("--disk-size-gb default = %v", flag)
	}
	if command.Flags().Lookup("disk-size-mb") != nil {
		t.Fatal("obsolete --disk-size-mb flag must not exist")
	}
	if command.Flags().Lookup("username") == nil {
		t.Fatal("--username flag must exist")
	}
	convertorFlag := rootCmd.PersistentFlags().Lookup("overlaybd-convertor")
	if convertorFlag == nil || convertorFlag.DefValue != defaultConvertor {
		t.Fatalf("--overlaybd-convertor default = %v", convertorFlag)
	}
}

func TestDiskSizeConversions(t *testing.T) {
	if got, err := diskSizeBytes(20); err != nil || got != 20<<30 {
		t.Errorf("20 GiB bytes = %d, %v", got, err)
	}
	if got, err := diskSizeMiB(30); err != nil || got != 30720 {
		t.Errorf("30 GiB MiB = %d, %v", got, err)
	}
	if got, err := bytesToGiBCeil((20 << 30) + 1); err != nil || got != 21 {
		t.Errorf("GiB ceiling = %d, %v", got, err)
	}
	if _, err := diskSizeBytes(int(^uint(0) >> 1)); err == nil {
		t.Fatal("overflowing GiB byte conversion must fail")
	}
}

func TestValidateBuildOptions(t *testing.T) {
	if err := validateBuildOptions(validBuildOptions(t)); err != nil {
		t.Fatalf("valid options must pass: %v", err)
	}

	mutate := func(f func(*buildOptions)) *buildOptions {
		options := validBuildOptions(t)
		f(options)
		return options
	}
	cases := []struct {
		name    string
		options *buildOptions
		want    string
	}{
		{"missing source", mutate(func(o *buildOptions) { o.source = "" }), "--source"},
		{"bad source", mutate(func(o *buildOptions) { o.source = "redis" }), "parse image reference"},
		{"missing output", mutate(func(o *buildOptions) { o.output = "" }), "--output"},
		{"missing kernel", mutate(func(o *buildOptions) { o.kernel = "" }), "--kernel"},
		{"kernel not found", mutate(func(o *buildOptions) { o.kernel = filepath.Join(t.TempDir(), "vmlinux-5.10.0") }), "kernel"},
		{"zero vcpu", mutate(func(o *buildOptions) { o.vcpu = 0 }), "--vcpu"},
		{"zero memory", mutate(func(o *buildOptions) { o.memoryMB = 0 }), "--memory-mb"},
		{"zero disk", mutate(func(o *buildOptions) { o.diskSizeGiB = 0 }), "--disk-size-gb"},
		{"negative disk", mutate(func(o *buildOptions) { o.diskSizeGiB = -1 }), "--disk-size-gb"},
		{"negative snapfiles", mutate(func(o *buildOptions) { o.snapfilesSizeMB = -1 }), "--snapfiles-size-mb"},
		{"conflicting credentials", mutate(func(o *buildOptions) { o.authFile = "config.json"; o.username = "alice:secret" }), "mutually exclusive"},
		{"malformed username", mutate(func(o *buildOptions) { o.username = "alice" }), "username:password"},
		{"empty username", mutate(func(o *buildOptions) { o.username = ":secret" }), "username:password"},
		{
			"underivable kernel version",
			mutate(func(o *buildOptions) {
				bare := filepath.Join(t.TempDir(), "vmlinux")
				if err := os.WriteFile(bare, []byte("x"), 0o640); err != nil {
					t.Fatal(err)
				}
				o.kernel = bare
			}),
			"--kernel-version",
		},
		{"missing init script", mutate(func(o *buildOptions) { o.initScriptFile = filepath.Join(t.TempDir(), "init.sh") }), "init script"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBuildOptions(tc.options)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}

	options := validBuildOptions(t)
	options.username = "alice:secret:with:colons"
	if err := validateBuildOptions(options); err != nil {
		t.Errorf("password containing colons must pass: %v", err)
	}

	// Firecracker runs in its own working directory: a relative kernel is
	// resolved against the CLI's cwd.
	options = validBuildOptions(t)
	t.Chdir(filepath.Dir(options.kernel))
	want := options.kernel
	options.kernel = filepath.Base(want)
	if err := validateBuildOptions(options); err != nil {
		t.Fatalf("relative kernel must pass: %v", err)
	}
	if options.kernel != want {
		t.Errorf("kernel = %q, want absolute %q", options.kernel, want)
	}

	// An explicit kernel version rescues a bare vmlinux filename.
	options = validBuildOptions(t)
	bare := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(bare, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	options.kernel = bare
	options.kernelVersion = "6.1.0"
	if err := validateBuildOptions(options); err != nil {
		t.Errorf("explicit kernel version must pass: %v", err)
	}
}

func TestSnapfilesSizeGiB(t *testing.T) {
	// An explicit size (MiB) is rounded up to whole GiB.
	options := &buildOptions{snapfilesSizeMB: 3072}
	if got, err := snapfilesSizeGiB(options, "", ""); err != nil || got != 3 {
		t.Errorf("explicit 3072 MiB = %d GiB, %v", got, err)
	}
	options.snapfilesSizeMB = 1
	if got, err := snapfilesSizeGiB(options, "", ""); err != nil || got != 1 {
		t.Errorf("explicit 1 MiB = %d GiB, %v", got, err)
	}

	// Automatic sizing: memfile + vmstate + headroom, with a 2 GiB floor.
	dir := t.TempDir()
	vmstate := filepath.Join(dir, "vmstate.bin")
	memfile := filepath.Join(dir, "memfile")
	if err := os.WriteFile(vmstate, make([]byte, 64<<20), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memfile, make([]byte, 128<<20), 0o640); err != nil {
		t.Fatal(err)
	}
	options = &buildOptions{}
	if got, err := snapfilesSizeGiB(options, vmstate, memfile); err != nil || got != 2 {
		t.Errorf("small snapshot must hit the 2 GiB floor: %d, %v", got, err)
	}

	// A 3 GiB memfile pushes the size above the floor.
	bigMemfile := filepath.Join(dir, "memfile-big")
	big, err := os.Create(bigMemfile)
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(3 << 30); err != nil {
		t.Fatal(err)
	}
	if err := big.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := snapfilesSizeGiB(options, vmstate, bigMemfile); err != nil || got != 4 {
		// 3 GiB + 64 MiB + 256 MiB headroom = 3392 MiB -> 4 GiB (ceiled).
		t.Errorf("3 GiB memfile = %d GiB, want 4: %v", got, err)
	}

	if _, err := snapfilesSizeGiB(options, vmstate, filepath.Join(dir, "absent")); err == nil {
		t.Error("missing memfile must fail")
	}
}

func TestKernelVersionFromPath(t *testing.T) {
	if got, err := kernelVersionFromPath("/opt/kernels/vmlinux-5.10.0", ""); err != nil || got != "5.10.0" {
		t.Errorf("derive = %q, %v", got, err)
	}
	if got, err := kernelVersionFromPath("/opt/kernels/vmlinux-5.10.0", "6.1.0"); err != nil || got != "6.1.0" {
		t.Errorf("override wins = %q, %v", got, err)
	}
	if _, err := kernelVersionFromPath("/opt/kernels/vmlinux", ""); err == nil {
		t.Error("bare vmlinux must fail without an override")
	}
}

func TestBuildSnapfilesManifestStandaloneAndExtras(t *testing.T) {
	options := validBuildOptions(t)
	options.kernelSHA256 = strings.Repeat("a", 64)
	previous := global
	t.Cleanup(func() { global = previous })
	binary := filepath.Join(t.TempDir(), "firecracker")
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\necho 'Firecracker v1.16.1'\n"), 0755))
	global.firecrackerBin = binary
	created := time.Date(2025, 6, 1, 12, 30, 0, 0, time.UTC)
	raw, err := buildSnapfilesManifest(options, created)
	require.NoError(t, err)
	document, err := artifacts.DecodeVMManifest(raw)
	require.NoError(t, err)
	require.Equal(t, 1, document.SchemaVersion)
	require.Equal(t, int64(1024), document.Machine.MemoryMiB)
	require.Equal(t, int64(20480), document.Machine.RootfsSizeMiB)
	require.Equal(t, "5.10.0", document.Kernel.Version)
	require.Equal(t, options.kernelSHA256, document.Kernel.SHA256)
	require.Equal(t, options.source, document.Lineage.Image)
	require.Equal(t, created.Format(time.RFC3339), document.Lineage.CreatedAt)
	require.Nil(t, document.Platform, "standalone builds need no platform input")
	options.manifestExtraFile = filepath.Join(t.TempDir(), "extra.json")
	require.NoError(t, os.WriteFile(options.manifestExtraFile, []byte(`{"platform":{"execd":"execd:v1","future":{"keep":true}}}`), 0600))
	raw, err = buildSnapfilesManifest(options, created)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"future"`)
	document, err = artifacts.DecodeVMManifest(raw)
	require.NoError(t, err)
	require.Equal(t, "execd:v1", document.Platform.Execd)
	for _, extra := range []string{`{"machine":{"vcpuCount":9}}`, `{"schemaVersion":9}`, `{"compatibility":{}}`, `null`, `{"guestNetwork":{"ip":"1.2.3.4"}}`, `{"lineage":{"createdAt":"wrong"}}`} {
		require.NoError(t, os.WriteFile(options.manifestExtraFile, []byte(extra), 0600))
		_, err = buildSnapfilesManifest(options, created)
		require.Error(t, err, extra)
	}
	options.tapName = "tap-test"
	options.guestMAC = "02:fc:00:00:00:01"
	require.NoError(t, os.WriteFile(options.manifestExtraFile, []byte(`{"guestNetwork":{"ip":"172.30.0.3","mtu":1500}}`), 0600))
	raw, err = buildSnapfilesManifest(options, created)
	require.NoError(t, err)
	document, err = artifacts.DecodeVMManifest(raw)
	require.NoError(t, err)
	require.Equal(t, options.guestMAC, document.GuestNetwork.MAC)
	require.Equal(t, "172.30.0.3", document.GuestNetwork.IP)
	require.NoError(t, os.WriteFile(options.manifestExtraFile, []byte(`{"guestNetwork":{"mac":"02:00:00:00:00:02"}}`), 0600))
	_, err = buildSnapfilesManifest(options, created)
	require.ErrorContains(t, err, "conflict")
}

func testVMManifest() artifacts.VMManifest {
	document := artifacts.NewVMManifest(artifacts.VMMachine{VCPUCount: 1, MemoryMiB: 1, RootfsSizeMiB: 1, RootfsDriveID: "rootfs", RootfsDrivePath: rootfsDriveLink}, "5.10.0", "/missing/firecracker", artifacts.CPUTemplateNone)
	document.Compatibility.FirecrackerVersion = "1.16.1"
	return document
}

func TestParseExt4Size(t *testing.T) {
	valid := "Filesystem volume name: rootfs\nBlock count: 5242880\nBlock size: 4096\n"
	if got, err := parseExt4Size(valid); err != nil || got != 20<<30 {
		t.Fatalf("parse ext4 size = %d, %v", got, err)
	}
	for _, output := range []string{
		"Block count: nope\nBlock size: 4096\n",
		"Block count: 1\n",
		"Block count: 0\nBlock size: 4096\n",
		"Block count: 9223372036854775807\nBlock size: 4096\n",
	} {
		if _, err := parseExt4Size(output); err == nil {
			t.Errorf("parseExt4Size(%q) must fail", output)
		}
	}
}

func TestExt4Commands(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "e2fsck"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "resize2fs"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ARGS_FILE", argsFile)
	if err := repairExt4(context.Background(), "/dev/fake"); err != nil {
		t.Fatalf("e2fsck exit 1 must be accepted: %v", err)
	}
	if err := resizeExt4(context.Background(), "/dev/fake", 30); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(payload); got != "/dev/fake\n30G\n" {
		t.Errorf("resize2fs arguments = %q", got)
	}
}

func TestResizeOverlayBDUpper(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	resizeBin := filepath.Join(dir, "overlaybd-resize")
	if err := os.WriteFile(resizeBin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGS_FILE", argsFile)
	oldResize := global.overlaybdResize
	global.overlaybdResize = resizeBin
	t.Cleanup(func() { global.overlaybdResize = oldResize })

	configPath := filepath.Join(dir, "rootfs.config.v1.json")
	config := OverlayBDBSConfig{Upper: OverlayBDBSConfigUpper{Data: "data", Index: "index", Vsize: 256}}
	if _, err := writeConfig(configPath, config); err != nil {
		t.Fatal(err)
	}
	if err := resizeOverlayBDUpper(context.Background(), configPath, 30); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(arguments); got != "--config\n"+configPath+"\n--size\n30\n" {
		t.Errorf("overlaybd-resize arguments = %q", got)
	}
	payload, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &config); err != nil {
		t.Fatal(err)
	}
	if config.Upper.Vsize != 30 {
		t.Errorf("updated upper vsize = %d", config.Upper.Vsize)
	}
}

func TestPrepareBuildRootfsShrinkAndReattach(t *testing.T) {
	var events []string
	resized := false
	addCount := 0
	targetBytes := int64(30 << 30)
	ops := buildRootfsOps{
		probeSourceSize: func(context.Context, string, fetchedImage, *rollback) (int64, error) {
			events = append(events, "probe")
			return 256 << 30, nil
		},
		writeConfig: func(_ context.Context, _ string, _ fetchedImage, initialGiB int) (string, error) {
			events = append(events, "config:"+stringInt(initialGiB))
			return "/work/rootfs.json", nil
		},
		addDevice: func(context.Context, string) (int, string, error) {
			addCount++
			if addCount == 1 {
				events = append(events, "add:old")
				return 2, "/dev/old", nil
			}
			events = append(events, "add:new")
			return 7, "/dev/new", nil
		},
		deleteDevice: func(_ context.Context, id int) error {
			events = append(events, "del:"+stringInt(id))
			return nil
		},
		repairFilesystem: func(_ context.Context, device string) error {
			events = append(events, "repair:"+device)
			return nil
		},
		resizeFilesystem: func(_ context.Context, device string, sizeGiB int) error {
			events = append(events, "resize-fs:"+device+":"+stringInt(sizeGiB))
			resized = true
			return nil
		},
		verifyFilesystem: func(_ context.Context, device string) error {
			events = append(events, "verify:"+device)
			return nil
		},
		filesystemSize: func(_ context.Context, device string) (int64, error) {
			events = append(events, "fs-size:"+device)
			if !resized {
				return 256 << 30, nil
			}
			return targetBytes, nil
		},
		resizeUpper: func(_ context.Context, _ string, sizeGiB int) error {
			events = append(events, "resize-upper:"+stringInt(sizeGiB))
			return nil
		},
		deviceSize: func(device string) (int64, error) {
			events = append(events, "device-size:"+device)
			return targetBytes, nil
		},
	}
	undo := &rollback{}
	attachment, err := prepareBuildRootfs(context.Background(), "/work", fetchedImage{}, 30, undo, ops)
	if err != nil {
		t.Fatal(err)
	}
	if attachment.devID != 7 || attachment.devPath != "/dev/new" || !attachment.attached {
		t.Fatalf("attachment = %+v", attachment)
	}
	want := []string{
		"probe", "config:256", "add:old", "repair:/dev/old", "fs-size:/dev/old",
		"resize-fs:/dev/old:30", "verify:/dev/old", "del:2", "resize-upper:30",
		"add:new", "device-size:/dev/new", "fs-size:/dev/new", "verify:/dev/new",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v\nwant   = %v", events, want)
	}
	if err := undo.run(); err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1] != "del:7" {
		t.Errorf("rollback detached %q, want new device", events[len(events)-1])
	}
}

func TestPrepareBuildRootfsRollback(t *testing.T) {
	t.Run("before detach", func(t *testing.T) {
		var deleted []int
		ops := buildRootfsOps{
			probeSourceSize: func(context.Context, string, fetchedImage, *rollback) (int64, error) { return 20 << 30, nil },
			writeConfig:     func(context.Context, string, fetchedImage, int) (string, error) { return "config", nil },
			addDevice:       func(context.Context, string) (int, string, error) { return 2, "/dev/old", nil },
			deleteDevice: func(_ context.Context, id int) error {
				deleted = append(deleted, id)
				return nil
			},
			repairFilesystem: func(context.Context, string) error { return errors.New("repair failed") },
		}
		undo := &rollback{}
		if _, err := prepareBuildRootfs(context.Background(), "work", fetchedImage{}, 20, undo, ops); err == nil {
			t.Fatal("prepare must fail")
		}
		if err := undo.run(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(deleted, []int{2}) {
			t.Errorf("deleted = %v", deleted)
		}
	})

	// A device that cannot be detached still reads the work dir's upper
	// files: the work dir must survive the rollback for a retry.
	t.Run("detach failure keeps work dir", func(t *testing.T) {
		workDir := filepath.Join(t.TempDir(), "work")
		if err := os.Mkdir(workDir, 0o750); err != nil {
			t.Fatal(err)
		}
		ops := buildRootfsOps{
			probeSourceSize: func(context.Context, string, fetchedImage, *rollback) (int64, error) { return 20 << 30, nil },
			writeConfig:     func(context.Context, string, fetchedImage, int) (string, error) { return "config", nil },
			addDevice:       func(context.Context, string) (int, string, error) { return 2, "/dev/old", nil },
			deleteDevice:    func(context.Context, int) error { return errors.New("delete response lost") },
			recoverDevice: func(context.Context, string) (int, error) {
				return 0, errors.New("device busy")
			},
			repairFilesystem: func(context.Context, string) error { return errors.New("repair failed") },
		}
		undo := &rollback{}
		undo.push("work dir "+workDir, func() error { return os.RemoveAll(workDir) })
		if _, err := prepareBuildRootfs(context.Background(), workDir, fetchedImage{}, 20, undo, ops); err == nil {
			t.Fatal("prepare must fail")
		}
		err := undo.run()
		if err == nil || !strings.Contains(err.Error(), "device busy") ||
			!strings.Contains(err.Error(), "ublk device owned by config config") || !strings.Contains(err.Error(), "work dir "+workDir) {
			t.Fatalf("rollback error = %v", err)
		}
		if _, err := os.Stat(workDir); err != nil {
			t.Errorf("work dir removed despite attached device: %v", err)
		}
	})

	t.Run("uncertain add is recovered before work dir removal", func(t *testing.T) {
		workDir := filepath.Join(t.TempDir(), "work")
		if err := os.Mkdir(workDir, 0o750); err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(workDir, "rootfs.config.v1.json")
		var recovered string
		ops := buildRootfsOps{
			probeSourceSize: func(context.Context, string, fetchedImage, *rollback) (int64, error) { return 20 << 30, nil },
			writeConfig:     func(context.Context, string, fetchedImage, int) (string, error) { return config, nil },
			addDevice: func(context.Context, string) (int, string, error) {
				return 0, "", fmt.Errorf("%w: EOF", errAddOutcomeUnknown)
			},
			deleteDevice: func(context.Context, int) error { return nil },
			recoverDevice: func(_ context.Context, ownedConfig string) (int, error) {
				recovered = ownedConfig
				return 1, nil
			},
		}
		undo := &rollback{}
		undo.push("work dir "+workDir, func() error { return os.RemoveAll(workDir) })
		if _, err := prepareBuildRootfs(context.Background(), workDir, fetchedImage{}, 20, undo, ops); err == nil || !strings.Contains(err.Error(), "EOF") {
			t.Fatalf("prepare error = %v", err)
		}
		if err := undo.run(); err != nil {
			t.Fatal(err)
		}
		if recovered != config {
			t.Errorf("recovered config = %q, want %q", recovered, config)
		}
		if _, err := os.Stat(workDir); !os.IsNotExist(err) {
			t.Errorf("work dir survived confirmed device recovery: %v", err)
		}
	})

	t.Run("uncertain add keeps work dir when recovery cannot confirm", func(t *testing.T) {
		workDir := filepath.Join(t.TempDir(), "work")
		if err := os.Mkdir(workDir, 0o750); err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(workDir, "rootfs.config.v1.json")
		ops := buildRootfsOps{
			probeSourceSize: func(context.Context, string, fetchedImage, *rollback) (int64, error) { return 20 << 30, nil },
			writeConfig:     func(context.Context, string, fetchedImage, int) (string, error) { return config, nil },
			addDevice: func(context.Context, string) (int, string, error) {
				return 0, "", fmt.Errorf("%w: EOF", errAddOutcomeUnknown)
			},
			deleteDevice:  func(context.Context, int) error { return nil },
			recoverDevice: func(context.Context, string) (int, error) { return 0, nil },
		}
		undo := &rollback{}
		undo.push("work dir "+workDir, func() error { return os.RemoveAll(workDir) })
		if _, err := prepareBuildRootfs(context.Background(), workDir, fetchedImage{}, 20, undo, ops); err == nil {
			t.Fatal("prepare must fail")
		}
		err := undo.run()
		if err == nil || !strings.Contains(err.Error(), "may still be in flight") || !strings.Contains(err.Error(), "work dir "+workDir) {
			t.Fatalf("rollback error = %v", err)
		}
		if _, err := os.Stat(workDir); err != nil {
			t.Errorf("work dir removed without confirmed recovery: %v", err)
		}
	})

	t.Run("after reattach", func(t *testing.T) {
		var deleted []int
		addCount := 0
		ops := buildRootfsOps{
			probeSourceSize: func(context.Context, string, fetchedImage, *rollback) (int64, error) { return 256 << 30, nil },
			writeConfig:     func(context.Context, string, fetchedImage, int) (string, error) { return "config", nil },
			addDevice: func(context.Context, string) (int, string, error) {
				addCount++
				if addCount == 1 {
					return 2, "/dev/old", nil
				}
				return 7, "/dev/new", nil
			},
			deleteDevice: func(_ context.Context, id int) error {
				deleted = append(deleted, id)
				return nil
			},
			repairFilesystem: func(context.Context, string) error { return nil },
			resizeFilesystem: func(context.Context, string, int) error { return nil },
			verifyFilesystem: func(context.Context, string) error { return nil },
			filesystemSize:   func(context.Context, string) (int64, error) { return 20 << 30, nil },
			resizeUpper:      func(context.Context, string, int) error { return nil },
			deviceSize:       func(string) (int64, error) { return 19 << 30, nil },
		}
		undo := &rollback{}
		if _, err := prepareBuildRootfs(context.Background(), "work", fetchedImage{}, 20, undo, ops); err == nil {
			t.Fatal("device-size mismatch must fail")
		}
		if err := undo.run(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(deleted, []int{2, 7}) {
			t.Errorf("deleted = %v", deleted)
		}
	})

	t.Run("filesystem mismatch", func(t *testing.T) {
		var deleted []int
		filesystemChecks := 0
		ops := buildRootfsOps{
			probeSourceSize: func(context.Context, string, fetchedImage, *rollback) (int64, error) { return 20 << 30, nil },
			writeConfig:     func(context.Context, string, fetchedImage, int) (string, error) { return "config", nil },
			addDevice:       func(context.Context, string) (int, string, error) { return 2, "/dev/rootfs", nil },
			deleteDevice: func(_ context.Context, id int) error {
				deleted = append(deleted, id)
				return nil
			},
			repairFilesystem: func(context.Context, string) error { return nil },
			resizeFilesystem: func(context.Context, string, int) error { return nil },
			verifyFilesystem: func(context.Context, string) error { return nil },
			filesystemSize: func(context.Context, string) (int64, error) {
				filesystemChecks++
				if filesystemChecks == 1 {
					return 20 << 30, nil
				}
				return (20 << 30) - 4096, nil
			},
			resizeUpper: func(context.Context, string, int) error { return nil },
			deviceSize:  func(string) (int64, error) { return 20 << 30, nil },
		}
		undo := &rollback{}
		if _, err := prepareBuildRootfs(context.Background(), "work", fetchedImage{}, 20, undo, ops); err == nil || !strings.Contains(err.Error(), "ext4 filesystem") {
			t.Fatalf("filesystem mismatch error = %v", err)
		}
		if err := undo.run(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(deleted, []int{2}) {
			t.Errorf("deleted = %v", deleted)
		}
	})
}

func stringInt(value int) string {
	return strconv.Itoa(value)
}

func TestRollback(t *testing.T) {
	order := []int{}
	undo := &rollback{}
	step := func(id int, err error) func() error {
		return func() error {
			order = append(order, id)
			return err
		}
	}
	undo.push("one", step(1, nil))
	undo.push("two", step(2, nil))
	undo.push("three", step(3, nil))
	if err := undo.run(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []int{3, 2, 1}) {
		t.Errorf("rollback order = %v, want reverse", order)
	}

	// A failed step stops the rollback: the earlier steps may be what the
	// failed resource still depends on, so they are kept and reported.
	order = nil
	undo = &rollback{}
	undo.push("work dir", step(1, nil))
	undo.push("device", step(2, errors.New("busy")))
	undo.push("mount", step(3, nil))
	err := undo.run()
	if !reflect.DeepEqual(order, []int{3, 2}) {
		t.Errorf("rollback order = %v, want stop at failed step", order)
	}
	if err == nil || !strings.Contains(err.Error(), "rollback stopped at device: busy") ||
		!strings.Contains(err.Error(), "kept for retry: work dir, device") {
		t.Errorf("rollback error = %v", err)
	}

	// disarm drops every pending step.
	order = nil
	undo = &rollback{}
	undo.push("one", step(1, nil))
	undo.disarm()
	if err := undo.run(); err != nil {
		t.Fatal(err)
	}
	if len(order) != 0 {
		t.Errorf("disarmed rollback ran %v", order)
	}
}

func TestWaitSerialFor(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "firecracker.log")
	if err := os.WriteFile(logPath, []byte("booting...\nReady to accept connections\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := waitSerialFor(logPath, defaultReadyPattern, time.Second); err != nil {
		t.Errorf("present pattern must succeed: %v", err)
	}

	if err := waitSerialFor(logPath, "   ", time.Second); err == nil {
		t.Error("blank pattern must fail")
	}

	// Timeout surfaces the log tail for diagnosis.
	err := waitSerialFor(logPath, "never appears", 250*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not report") || !strings.Contains(err.Error(), "booting...") {
		t.Errorf("timeout error must include the log tail: %v", err)
	}

	// A missing log file still times out gracefully.
	if err := waitSerialFor(filepath.Join(dir, "absent.log"), "x", 250*time.Millisecond); err == nil {
		t.Error("missing log must time out")
	}
}

func TestLogTail(t *testing.T) {
	if got := logTail([]byte("short"), 100); got != "short" {
		t.Errorf("short payload = %q", got)
	}
	payload := bytes.Repeat([]byte("a"), 8192)
	got := logTail(payload, 16)
	if !strings.HasPrefix(got, "...\n") || len(got) != len("...\n")+16 {
		t.Errorf("truncated tail length = %d", len(got))
	}
}

func TestGibCeil(t *testing.T) {
	cases := map[int]int{0: 1, -5: 1, 1: 1, 1023: 1, 1024: 1, 1025: 2, 2048: 2, 4096: 4}
	for input, want := range cases {
		if got := gibCeil(input); got != want {
			t.Errorf("gibCeil(%d) = %d, want %d", input, got, want)
		}
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "src")
	payload := bytes.Repeat([]byte("snapshot"), 1<<16)
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "dst")
	if err := copyFile(source, target, 0o640); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	copied, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(copied, payload) {
		t.Errorf("copied content mismatch: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	if err := copyFile(filepath.Join(dir, "absent"), target, 0o640); err == nil {
		t.Error("missing source must fail")
	}
}

func TestBuildManifestRecordsProbedHelperImplementation(t *testing.T) {
	options := validBuildOptions(t)
	options.kernelSHA256 = strings.Repeat("a", 64)
	options.guestControlPort = 1234
	// This test checks metadata, not host CPU discovery. Use a complete Full
	// compatibility fixture so both ARM64 and amd64 execute every assertion.
	base := artifacts.VMManifest{
		SchemaVersion: artifacts.VMManifestVersion, Runtime: "firecracker",
		Machine:       artifacts.VMMachine{VCPUCount: 1, MemoryMiB: 1024, RootfsSizeMiB: 20480, RootfsDriveID: "rootfs", RootfsDrivePath: rootfsDriveLink},
		Kernel:        artifacts.VMKernel{Version: "5.10.0"},
		Compatibility: artifacts.SnapshotCompatibility{CPUArch: "x86_64", FirecrackerVersion: "1.16.1", CPUTemplate: artifacts.CPUTemplateNone, Vendor: artifacts.VendorGenuineIntel, CPUFamily: 6, CPUModel: 85, CPUStepping: 7},
	}
	require.NoError(t, base.Validate())
	created := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	_, err := completeBuildManifest(options, created, base)
	require.ErrorContains(t, err, "freeze/thaw probe")
	for _, test := range []struct{ name, version string }{
		{"helper-a", "sha256:" + strings.Repeat("a", 64)},
		{"helper-b", "sha256:" + strings.Repeat("b", 64)},
	} {
		t.Run(test.name, func(t *testing.T) {
			options.guestControlVersion = test.version
			raw, err := completeBuildManifest(options, created, base)
			require.NoError(t, err)
			manifest, err := artifacts.DecodeVMManifest(raw)
			require.NoError(t, err)
			require.Equal(t, test.version, manifest.GuestControl.HelperVersion)
			require.Equal(t, options.kernelSHA256, manifest.Kernel.SHA256)
			require.Equal(t, base.Compatibility, manifest.Compatibility)
		})
	}
	// Isolating host discovery must not relax production compatibility checks.
	invalid := base
	invalid.Compatibility.Vendor = ""
	_, err = completeBuildManifest(options, created, invalid)
	require.ErrorContains(t, err, "resolved CPU identity")
	options.kernelSHA256 = ""
	_, err = completeBuildManifest(options, created, base)
	require.ErrorContains(t, err, "boot kernel digest")
}
