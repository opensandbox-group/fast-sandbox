package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

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

func TestBuildSnapfilesMetadata(t *testing.T) {
	options := validBuildOptions(t)
	options.rootfsDriveID = defaultRootfsDriveID
	createdAt := time.Date(2025, 6, 1, 12, 30, 0, 0, time.FixedZone("CST", 8*3600))

	payload, err := buildSnapfilesMetadata(options, options.source, createdAt)
	if err != nil {
		t.Fatalf("buildSnapfilesMetadata: %v", err)
	}

	// The consumer schema parses the generated metadata unchanged.
	var metadata snapfilesMetadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		t.Fatalf("metadata JSON: %v", err)
	}
	if metadata.VCPUCount != 1 || metadata.MemoryMB != 1024 || metadata.DiskSizeMB != 20480 {
		t.Errorf("machine fields = %+v", metadata.vmMetadata)
	}
	if metadata.KernelVersion != "5.10.0" {
		t.Errorf("kernel_version = %q", metadata.KernelVersion)
	}
	if metadata.HypervisorType != hypervisorFirecracker {
		t.Errorf("hypervisor_type = %q", metadata.HypervisorType)
	}
	if metadata.SchemaVersion != metadataSchemaVersion {
		t.Errorf("schema_version = %d", metadata.SchemaVersion)
	}
	if metadata.SourceImage != options.source || metadata.RootfsDriveID != defaultRootfsDriveID {
		t.Errorf("provenance = %q / %q", metadata.SourceImage, metadata.RootfsDriveID)
	}
	if metadata.CreatedAt != createdAt.UTC().Format(time.RFC3339) {
		t.Errorf("created_at = %q, want UTC", metadata.CreatedAt)
	}
	if metadata.DefaultWorkdir != "/data" {
		t.Errorf("default_workdir = %q", metadata.DefaultWorkdir)
	}

	// loadMetadata (consumer path) accepts the same payload.
	var consumer vmMetadata
	if err := json.Unmarshal(payload, &consumer); err != nil {
		t.Fatalf("consumer parse: %v", err)
	}
	if consumer.VCPUCount != 1 || consumer.KernelVersion != "5.10.0" {
		t.Errorf("consumer view = %+v", consumer)
	}

	options.diskSizeGiB = 30
	payload, err = buildSnapfilesMetadata(options, options.source, createdAt)
	if err != nil {
		t.Fatalf("custom size metadata: %v", err)
	}
	if err := json.Unmarshal(payload, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.DiskSizeMB != 30720 {
		t.Errorf("30 GiB metadata = %d MiB", metadata.DiskSizeMB)
	}
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
		probeSourceSize: func(context.Context, string, fetchedImage) (int64, error) {
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
	undo.run()
	if events[len(events)-1] != "del:7" {
		t.Errorf("rollback detached %q, want new device", events[len(events)-1])
	}
}

func TestPrepareBuildRootfsRollback(t *testing.T) {
	t.Run("before detach", func(t *testing.T) {
		var deleted []int
		ops := buildRootfsOps{
			probeSourceSize: func(context.Context, string, fetchedImage) (int64, error) { return 20 << 30, nil },
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
		undo.run()
		if !reflect.DeepEqual(deleted, []int{2}) {
			t.Errorf("deleted = %v", deleted)
		}
	})

	t.Run("after reattach", func(t *testing.T) {
		var deleted []int
		addCount := 0
		ops := buildRootfsOps{
			probeSourceSize: func(context.Context, string, fetchedImage) (int64, error) { return 256 << 30, nil },
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
		undo.run()
		if !reflect.DeepEqual(deleted, []int{2, 7}) {
			t.Errorf("deleted = %v", deleted)
		}
	})

	t.Run("filesystem mismatch", func(t *testing.T) {
		var deleted []int
		filesystemChecks := 0
		ops := buildRootfsOps{
			probeSourceSize: func(context.Context, string, fetchedImage) (int64, error) { return 20 << 30, nil },
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
		undo.run()
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
	undo.push(func() { order = append(order, 1) })
	undo.push(func() { order = append(order, 2) })
	undo.push(func() { order = append(order, 3) })
	undo.run()
	if !reflect.DeepEqual(order, []int{3, 2, 1}) {
		t.Errorf("rollback order = %v, want reverse", order)
	}

	// disarm drops every pending step.
	order = nil
	undo = &rollback{}
	undo.push(func() { order = append(order, 1) })
	undo.disarm()
	undo.run()
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
