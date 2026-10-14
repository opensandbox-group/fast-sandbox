package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeProcTree points procRoot at a temporary tree for the test.
func fakeProcTree(t *testing.T, bootID string) string {
	t.Helper()
	root := t.TempDir()
	bootDir := filepath.Join(root, "sys", "kernel", "random")
	if err := os.MkdirAll(bootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bootDir, "boot_id"), []byte(bootID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = previous })
	return root
}

// statLine renders a /proc/<pid>/stat line with the given comm and
// starttime (field 22).
func statLine(pid int, comm string, startTime uint64) string {
	fields := []string{"S"}
	for field := 4; field < 22; field++ {
		fields = append(fields, strconv.Itoa(field))
	}
	fields = append(fields, strconv.FormatUint(startTime, 10), "4096", "100")
	return fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(fields, " "))
}

func writeFakeProcess(t *testing.T, root string, pid int, startTime uint64, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(statLine(pid, "fire cracker) (x", startTime)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdline := strings.Join(argv, "\x00") + "\x00"
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseStatStartTime(t *testing.T) {
	got, err := parseStatStartTime([]byte(statLine(42, "a) (b c", 987654)))
	if err != nil || got != 987654 {
		t.Fatalf("parseStatStartTime = %d, %v; want 987654", got, err)
	}
	for _, malformed := range []string{"42 no-comm S 1 2", "42 (short) S 1 2 3"} {
		if _, err := parseStatStartTime([]byte(malformed)); err == nil {
			t.Errorf("parseStatStartTime(%q) must fail", malformed)
		}
	}
}

func TestProcessRecordIdentity(t *testing.T) {
	root := fakeProcTree(t, "boot-a")
	writeFakeProcess(t, root, 100, 5000, "firecracker")

	record, err := recordProcess(100)
	if err != nil {
		t.Fatal(err)
	}
	if record != (processRecord{PID: 100, StartTime: 5000, BootID: "boot-a"}) {
		t.Fatalf("record = %+v", record)
	}
	if !record.matches() || !(&record).alive() {
		t.Fatal("fresh record must match its process")
	}

	recycled := record
	recycled.StartTime = 4999
	if recycled.matches() {
		t.Error("a recycled PID (different start time) must not match")
	}
	otherBoot := record
	otherBoot.BootID = "boot-b"
	if otherBoot.matches() {
		t.Error("a record from another boot must not match")
	}
	gone := record
	gone.PID = 101
	if gone.matches() {
		t.Error("a missing process must not match")
	}
	var none *processRecord
	if none.alive() {
		t.Error("a nil record is never alive")
	}
}

func TestFindProcessByArgs(t *testing.T) {
	root := fakeProcTree(t, "boot-a")
	writeFakeProcess(t, root, 200, 10, "firecracker", "--id", "sb", "--api-sock", "/run/sb/api.sock2")
	writeFakeProcess(t, root, 201, 11, "firecracker", "--id", "sb", "--api-sock", "/run/sb/api.sock")
	writeFakeProcess(t, root, 202, 12, "sleep", "/run/sb/api.sock")

	found, err := findProcessByArgs("--api-sock", "/run/sb/api.sock")
	if err != nil {
		t.Fatal(err)
	}
	if found == nil || found.PID != 201 || found.StartTime != 11 {
		t.Fatalf("found = %+v, want pid 201", found)
	}
	missing, err := findProcessByArgs("--api-sock", "/run/other/api.sock")
	if !errors.Is(err, os.ErrNotExist) || missing != nil {
		t.Fatalf("unrelated socket: found %+v, %v", missing, err)
	}
}

func startSleeper(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	command := exec.Command("sleep", "60")
	if err := command.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	t.Cleanup(func() { _ = command.Process.Kill(); <-done })
	return command, done
}

func TestTerminateProcess(t *testing.T) {
	command, done := startSleeper(t)
	record, err := recordProcess(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}

	stale := record
	stale.StartTime++
	if err := terminateProcess(stale, time.Second); err != nil {
		t.Fatalf("stale record: %v", err)
	}
	select {
	case <-done:
		t.Fatal("a stale record must never be signalled")
	case <-time.After(100 * time.Millisecond):
	}

	if err := terminateProcess(record, 2*time.Second); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process still running after terminate")
	}
	if err := terminateProcess(record, time.Second); err != nil {
		t.Fatalf("terminating an exited process must succeed: %v", err)
	}
}

func TestValidateSandboxID(t *testing.T) {
	for _, valid := range []string{"sb1", "redis-6.2.1_a", strings.Repeat("a", 64)} {
		if err := validateSandboxID(valid); err != nil {
			t.Errorf("validateSandboxID(%q) = %v", valid, err)
		}
	}
	for _, invalid := range []string{"", ".", "..", "../..", "a/b", ".locks", "-x", "_x", "a b", strings.Repeat("a", 65)} {
		if err := validateSandboxID(invalid); err == nil {
			t.Errorf("validateSandboxID(%q) must fail", invalid)
		}
	}
}

func TestTryLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	release, err := tryLock(dir, "sb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tryLock(dir, "sb"); !errors.Is(err, errBusy) {
		t.Fatalf("second lock = %v, want errBusy", err)
	}
	other, err := tryLock(dir, "other")
	if err != nil {
		t.Fatalf("an unrelated name must lock: %v", err)
	}
	other()
	release()
	again, err := tryLock(dir, "sb")
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}

// withSandboxRoots points the global state/runtime roots at temp dirs.
func withSandboxRoots(t *testing.T) {
	t.Helper()
	previousState, previousRuntime := global.stateDir, global.runtimeDir
	global.stateDir = filepath.Join(t.TempDir(), "state")
	global.runtimeDir = filepath.Join(t.TempDir(), "run")
	t.Cleanup(func() { global.stateDir, global.runtimeDir = previousState, previousRuntime })
}

func TestClaimSandboxIsAtomic(t *testing.T) {
	withSandboxRoots(t)
	layout := newSandboxLayout("sb")
	if err := claimSandbox(layout, "sb"); err != nil {
		t.Fatal(err)
	}
	if err := claimSandbox(layout, "sb"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second claim = %v, want already exists", err)
	}

	stale := newSandboxLayout("stale")
	if err := os.MkdirAll(stale.runtime, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := claimSandbox(stale, "stale"); err == nil || !strings.Contains(err.Error(), "leftover runtime") {
		t.Fatalf("claim over a stale runtime dir = %v", err)
	}
	if pathExists(stale.home) {
		t.Error("a failed claim must release the home it created")
	}

	ids, err := listSandboxIDs(global.stateDir)
	if err != nil || len(ids) != 1 || ids[0] != "sb" {
		t.Fatalf("listSandboxIDs = %v, %v", ids, err)
	}
}

// fakeReleaser records Del calls against a fixed device listing.
type fakeReleaser struct {
	devices []ublkdDevice
	listErr error
	delErr  map[int]error
	deleted []int
}

func (f *fakeReleaser) List(context.Context) ([]ublkdDevice, error) {
	return f.devices, f.listErr
}

func (f *fakeReleaser) Del(_ context.Context, devID int) error {
	if err := f.delErr[devID]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, devID)
	return nil
}

func TestReleaseOwnedDevicesMatchesConfig(t *testing.T) {
	releaser := &fakeReleaser{}
	devices := []ublkdDevice{
		{DevID: 1, Config: "/state/sb/rootfs.config.v1.json"},
		{DevID: 2, Config: "/state/other/rootfs.config.v1.json"},
		{DevID: 3, Config: "/state/sb/rootfs.config.v1.json"},
	}
	if err := releaseOwnedDevices(context.Background(), releaser, devices, "/state/sb/rootfs.config.v1.json"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(releaser.deleted) != "[1 3]" {
		t.Fatalf("deleted = %v, want [1 3]", releaser.deleted)
	}
}

func newReleaseFixture(t *testing.T) (sandboxLayout, *sandboxState) {
	t.Helper()
	withSandboxRoots(t)
	layout := newSandboxLayout("sb")
	if err := claimSandbox(layout, "sb"); err != nil {
		t.Fatal(err)
	}
	// The recorded dev_ids are stale: 9 now belongs to another sandbox.
	state := &sandboxState{
		SandboxID: "sb",
		Phase:     phaseRunning,
		Rootfs:    &deviceRecord{Config: layout.rootfsConfig, DevID: 9, DevPath: "/dev/ublkb9"},
		Snapfiles: &deviceRecord{Config: layout.snapfilesConfig, DevID: -1},
		APISocket: layout.apiSocket,
	}
	if err := writeState(layout.home, state); err != nil {
		t.Fatal(err)
	}
	if _, err := linkRootfsDrive(layout.runtime, "/dev/ublkb4"); err != nil {
		t.Fatal(err)
	}
	return layout, state
}

func TestReleaseSandboxReclaimsByConfig(t *testing.T) {
	layout, state := newReleaseFixture(t)
	releaser := &fakeReleaser{devices: []ublkdDevice{
		{DevID: 4, Config: layout.rootfsConfig},
		{DevID: 9, Config: "/elsewhere/rootfs.config.v1.json"},
		{DevID: 5, Config: layout.snapfilesConfig},
	}}
	if err := releaseSandbox(context.Background(), releaser, layout, state); err != nil {
		t.Fatalf("releaseSandbox: %v", err)
	}
	if fmt.Sprint(releaser.deleted) != "[4 5]" {
		t.Fatalf("deleted = %v, want [4 5] (never the recycled id 9)", releaser.deleted)
	}
	if pathExists(layout.rootfsLink) {
		t.Error("rootfs.dev link must be removed")
	}
	persisted, err := readState(layout.home)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != phaseDeleting || persisted.Rootfs != nil || persisted.Snapfiles != nil {
		t.Fatalf("persisted state = %+v", persisted)
	}
	if err := removeSandboxDirs(layout); err != nil || pathExists(layout.home) || pathExists(layout.runtime) {
		t.Fatalf("removeSandboxDirs: %v", err)
	}
}

func TestReleaseSandboxKeepsStateOnFailure(t *testing.T) {
	layout, state := newReleaseFixture(t)
	releaser := &fakeReleaser{
		devices: []ublkdDevice{{DevID: 4, Config: layout.rootfsConfig}, {DevID: 5, Config: layout.snapfilesConfig}},
		delErr:  map[int]error{4: errors.New("device busy")},
	}
	err := releaseSandbox(context.Background(), releaser, layout, state)
	if err == nil || !strings.Contains(err.Error(), "device busy") {
		t.Fatalf("releaseSandbox = %v, want device busy", err)
	}
	persisted, readErr := readState(layout.home)
	if readErr != nil {
		t.Fatalf("state must survive a failed teardown: %v", readErr)
	}
	if persisted.Rootfs == nil || persisted.Snapfiles != nil {
		t.Fatalf("only the unreleased rootfs record must remain: %+v", persisted)
	}

	// A retry with the device released finishes the job.
	releaser.delErr = nil
	releaser.devices = releaser.devices[:1]
	if err := releaseSandbox(context.Background(), releaser, layout, &persisted); err != nil {
		t.Fatalf("retry: %v", err)
	}

	listFailure := &fakeReleaser{listErr: errors.New("ublkd down")}
	if err := releaseSandbox(context.Background(), listFailure, layout, &persisted); err == nil {
		t.Fatal("an unreachable ublkd must fail the teardown")
	}
}

func TestDeviceAttachmentReleasesOnce(t *testing.T) {
	var released []int
	release := func(_ context.Context, devID int) error {
		released = append(released, devID)
		return nil
	}
	undo := &rollback{}
	attachment := attachDevice(5, "/dev/ublkb5", release, undo)
	if err := attachment.detach(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A later failure runs the rollback; dev_id 5 may already belong to
	// another device by then.
	if err := undo.run(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(released) != "[5]" {
		t.Fatalf("released = %v, want exactly one release", released)
	}
}

func TestOverlaybdCredentialsRekeysForBlobURL(t *testing.T) {
	token := base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	config := dockerConfig{Auths: map[string]authEntry{
		"https://index.docker.io/v1/":  {Auth: token},
		"registry.example.com/team":    {Username: "bob", Password: "pw"},
		"registry.example.com/team/db": {Username: "carol", Password: "pw2"},
	}}
	refs := []string{
		"docker.io/library/redis:t_rootfs",
		"registry.example.com/team/app:t_snapfiles",
		"registry.example.com/team/db:t_rootfs",
		"unknown.example.com/x/y:t",
	}
	handoff, err := overlaybdCredentials(config, refs...)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"registry-1.docker.io/library/redis": "alice:secret",
		"registry.example.com/team/app":      "bob:pw",
		"registry.example.com/team/db":       "carol:pw2",
	}
	if len(handoff.Auths) != len(want) {
		t.Fatalf("handoff keys = %v", handoff.Auths)
	}
	for key, credential := range want {
		entry, ok := handoff.Auths[key]
		if !ok {
			t.Fatalf("missing key %q in %v", key, handoff.Auths)
		}
		decoded, _ := base64.StdEncoding.DecodeString(entry.Auth)
		if string(decoded) != credential {
			t.Errorf("%s auth = %q, want %q", key, decoded, credential)
		}
	}
	// Each key must equal a host/repository prefix of the blob URL overlaybd
	// matches against (its parse_auths drops the scheme, /v2 and /blobs).
	for _, imageRef := range refs[:3] {
		ref, _ := parseRegistryReference(imageRef)
		blobURL := repoBlobURL(ref, false)
		prefix := strings.TrimSuffix(strings.Replace(strings.TrimPrefix(blobURL, "https://"), "/v2/", "/", 1), "/blobs")
		if _, ok := handoff.Auths[prefix]; !ok {
			t.Errorf("no cred.json key equals %q (blob URL %s)", prefix, blobURL)
		}
	}
}

func TestMarshalCredentialsLongestKeyFirst(t *testing.T) {
	config := dockerConfig{Auths: map[string]authEntry{
		"a.io":          {Auth: "MQ=="},
		"a.io/team/app": {Auth: "Mw=="},
		"a.io/team":     {Auth: "Mg=="},
	}}
	payload, err := marshalCredentials(config)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	first, second, third := strings.Index(text, `"a.io/team/app"`), strings.Index(text, `"a.io/team"`), strings.Index(text, `"a.io"`)
	if first < 0 || !(first < second && second < third) {
		t.Fatalf("keys not ordered longest first:\n%s", text)
	}
	var decoded dockerConfig
	if err := json.Unmarshal(payload, &decoded); err != nil || len(decoded.Auths) != 3 {
		t.Fatalf("round trip: %v %+v", err, decoded)
	}
	empty, err := marshalCredentials(dockerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(empty, &decoded); err != nil {
		t.Fatalf("empty cred.json is invalid JSON: %v\n%s", err, empty)
	}
}

func TestValidateInitPath(t *testing.T) {
	if err := validateInitPath(defaultInitPath); err != nil {
		t.Fatalf("default init path: %v", err)
	}
	for _, invalid := range []string{"", "init", "/", "/a/../init", "/a//init", "/my init", "/init\n"} {
		if err := validateInitPath(invalid); err == nil {
			t.Errorf("validateInitPath(%q) must fail", invalid)
		}
	}
}

func TestWriteGuestFileStaysInsideRoot(t *testing.T) {
	base := t.TempDir()
	guest := filepath.Join(base, "guest")
	host := filepath.Join(base, "host")
	for _, dir := range []string{guest, host} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hostFile := filepath.Join(host, "passwd")
	if err := os.WriteFile(hostFile, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Final component: an absolute symlink onto the host is replaced.
	if err := os.Symlink(hostFile, filepath.Join(guest, "init")); err != nil {
		t.Fatal(err)
	}
	if err := writeGuestFile(guest, "/init", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writeGuestFile: %v", err)
	}
	if payload, _ := os.ReadFile(hostFile); string(payload) != "host" {
		t.Fatalf("host file overwritten: %q", payload)
	}
	info, err := os.Lstat(filepath.Join(guest, "init"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("guest init = %v, %v; want regular 0755 file", info, err)
	}

	// Intermediate component: escaping links (absolute or ../) are refused.
	if err := os.Symlink(host, filepath.Join(guest, "sbin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../host", filepath.Join(guest, "etc")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/sbin/init", "/etc/init"} {
		if err := writeGuestFile(guest, target, []byte("x"), 0o755); err == nil {
			t.Errorf("writeGuestFile(%s) through an escaping link must fail", target)
		}
	}
	if _, err := os.Stat(filepath.Join(host, "init")); !os.IsNotExist(err) {
		t.Fatalf("a file escaped onto the host: %v", err)
	}

	// Missing parents are created inside the root.
	if err := writeGuestFile(guest, "/opt/fsb/init", []byte("x"), 0o755); err != nil {
		t.Fatalf("nested path: %v", err)
	}
}

func TestWriteGuestFileReplacesSpecialFiles(t *testing.T) {
	guest := t.TempDir()
	assertFreshScript := func(t *testing.T, name string) {
		t.Helper()
		if err := writeGuestFile(guest, "/"+name, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("writeGuestFile: %v", err)
		}
		info, err := os.Lstat(filepath.Join(guest, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
			t.Fatalf("guest %s = %v, %v; want regular 0755 file", name, info, err)
		}
		if payload, _ := os.ReadFile(filepath.Join(guest, name)); string(payload) != "#!/bin/sh\n" {
			t.Fatalf("guest %s payload = %q", name, payload)
		}
	}

	// A FIFO would block the open (or route the write to a reader); it is
	// unlinked and replaced by a regular file. Device nodes take the same
	// path but need CAP_MKNOD to set up.
	t.Run("fifo", func(t *testing.T) {
		if err := syscall.Mkfifo(filepath.Join(guest, "fifo-init"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertFreshScript(t, "fifo-init")
	})

	// A hard link shares its inode with another guest file, which must keep
	// its content.
	t.Run("hard link", func(t *testing.T) {
		shared := filepath.Join(guest, "shared")
		if err := os.WriteFile(shared, []byte("shared"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(shared, filepath.Join(guest, "link-init")); err != nil {
			t.Fatal(err)
		}
		assertFreshScript(t, "link-init")
		if payload, _ := os.ReadFile(shared); string(payload) != "shared" {
			t.Fatalf("hard-linked file overwritten: %q", payload)
		}
	})

	t.Run("directory", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(guest, "dir-init"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeGuestFile(guest, "/dir-init", []byte("x"), 0o755); err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("directory target error = %v", err)
		}
	})
}

func TestMetadataRequiresRelativeRootfsDrive(t *testing.T) {
	legacy := vmMetadata{HypervisorType: hypervisorFirecracker}
	if err := legacy.validate("firecracker", true); err == nil || !strings.Contains(err.Error(), "rebuild") {
		t.Fatalf("legacy template = %v, want rebuild error even with --force", err)
	}
	pinned := vmMetadata{RootfsDrivePath: "/dev/ublkb2"}
	if err := pinned.validate("firecracker", true); err == nil {
		t.Fatal("a host device path must be rejected")
	}
	current := vmMetadata{RootfsDrivePath: rootfsDriveLink}
	if err := current.validate("firecracker", true); err != nil {
		t.Fatalf("current template: %v", err)
	}
}

func TestCreateHasNoSpecOverrides(t *testing.T) {
	flags := newCreateCommand().Flags()
	for _, removed := range []string{"vcpu", "memory-mb", "rootfs-drive-id"} {
		if flags.Lookup(removed) != nil {
			t.Errorf("create must not expose --%s", removed)
		}
	}
}
