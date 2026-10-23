package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrintSandboxSharedAndPrivateSnapfiles(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(strconv.FormatBool(shared), func(t *testing.T) {
			state := &sandboxState{SandboxID: "sandbox", Firecracker: &processRecord{PID: 123}, Rootfs: &deviceRecord{DevID: 1, DevPath: "/dev/ublkb1"}, Snapfiles: &deviceRecord{DevID: 2, DevPath: "/dev/ublkb2"}}
			want := "snapfiles device: /dev/ublkb2 (dev_id 2)"
			if shared {
				state.Snapfiles = nil
				state.SharedSnapfiles = strings.Repeat("a", 64)
				state.MountPoint = "/shared/mnt"
				want = "snapfiles shared: /shared/mnt"
			}
			reader, writer, err := os.Pipe()
			require.NoError(t, err)
			previous := os.Stdout
			os.Stdout = writer
			t.Cleanup(func() { os.Stdout = previous; _ = reader.Close(); _ = writer.Close() })
			require.NotPanics(t, func() { printSandbox(state) })
			require.NoError(t, writer.Close())
			os.Stdout = previous
			output, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.Contains(t, string(output), want)
		})
	}
}

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
	matches, err := releaseOwnedDevices(context.Background(), releaser, devices, "/state/sb/rootfs.config.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if matches != 2 {
		t.Fatalf("matches = %d, want 2", matches)
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

func TestReleaseSandboxKeepsUnknownAddStateWhenListIsEmpty(t *testing.T) {
	layout, state := newReleaseFixture(t)
	state.Rootfs.addOutcomeUnknown = true
	releaser := &fakeReleaser{devices: []ublkdDevice{{DevID: 5, Config: layout.snapfilesConfig}}}

	err := releaseSandbox(context.Background(), releaser, layout, state)
	if err == nil || !strings.Contains(err.Error(), "may still be in flight") {
		t.Fatalf("releaseSandbox = %v, want in-flight Add error", err)
	}
	if state.Rootfs == nil || state.Snapfiles != nil {
		t.Fatalf("only the unknown rootfs Add must remain: %+v", state)
	}
	persisted, readErr := readState(layout.home)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if persisted.Rootfs == nil || persisted.Rootfs.addOutcomeUnknown {
		t.Fatalf("persisted rootfs = %+v, want retryable record without process-local marker", persisted.Rootfs)
	}
	// A later explicit delete has no in-process Add to wait for and may accept
	// the empty listing as confirmation that there is no device to release.
	if err := releaseSandbox(context.Background(), &fakeReleaser{}, layout, &persisted); err != nil {
		t.Fatalf("explicit delete retry: %v", err)
	}
	if persisted.Rootfs != nil {
		t.Fatalf("retry kept rootfs record: %+v", persisted.Rootfs)
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
	attachment := trackDevice("/config", release, func(context.Context, string) (int, error) {
		t.Fatal("config recovery must not run after a confirmed delete")
		return 0, nil
	}, undo)
	if err := attachment.add(context.Background(), func(context.Context, string) (int, string, error) {
		return 5, "/dev/ublkb5", nil
	}); err != nil {
		t.Fatal(err)
	}
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

func TestDeviceAttachmentRecoversUncertainResponsesByConfig(t *testing.T) {
	t.Run("add response lost", func(t *testing.T) {
		var recovered string
		undo := &rollback{}
		attachment := trackDevice("/work/rootfs.config.v1.json", func(context.Context, int) error {
			t.Fatal("an unknown dev_id must not be deleted")
			return nil
		}, func(_ context.Context, config string) (int, error) {
			recovered = config
			return 1, nil
		}, undo)
		if err := attachment.add(context.Background(), func(context.Context, string) (int, string, error) {
			return 0, "", fmt.Errorf("%w: EOF", errAddOutcomeUnknown)
		}); err == nil {
			t.Fatal("add must report the lost response")
		}
		if err := undo.run(); err != nil {
			t.Fatal(err)
		}
		if recovered != attachment.config || attachment.attached {
			t.Fatalf("recovered = %q, attachment = %+v", recovered, attachment)
		}
	})

	t.Run("add still in flight", func(t *testing.T) {
		undo := &rollback{}
		attachment := trackDevice("/work/rootfs.config.v1.json", func(context.Context, int) error {
			t.Fatal("an unknown dev_id must not be deleted")
			return nil
		}, func(context.Context, string) (int, error) {
			return 0, nil
		}, undo)
		if err := attachment.add(context.Background(), func(context.Context, string) (int, string, error) {
			return 0, "", fmt.Errorf("%w: EOF", errAddOutcomeUnknown)
		}); err == nil {
			t.Fatal("add must report the lost response")
		}
		err := undo.run()
		if err == nil || !strings.Contains(err.Error(), "may still be in flight") {
			t.Fatalf("rollback error = %v", err)
		}
		if !attachment.attached {
			t.Fatal("attachment must remain armed while Add may still be running")
		}
	})

	t.Run("del response lost", func(t *testing.T) {
		var deleted []int
		var recovered string
		undo := &rollback{}
		attachment := trackDevice("/work/rootfs.config.v1.json", func(_ context.Context, devID int) error {
			deleted = append(deleted, devID)
			return errors.New("EOF")
		}, func(_ context.Context, config string) (int, error) {
			recovered = config
			return 0, nil
		}, undo)
		if err := attachment.add(context.Background(), func(context.Context, string) (int, string, error) {
			return 5, "/dev/ublkb5", nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := attachment.detach(context.Background()); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(deleted) != "[5]" || recovered != attachment.config {
			t.Fatalf("deleted = %v, recovered = %q", deleted, recovered)
		}
		if err := undo.run(); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(deleted) != "[5]" {
			t.Fatalf("possibly recycled dev_id deleted twice: %v", deleted)
		}
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestUblkdAddClassifiesTransportOutcome(t *testing.T) {
	cases := []struct {
		name        string
		roundTrip   roundTripperFunc
		wantUnknown bool
	}{
		{
			name: "response lost after connect",
			roundTrip: func(*http.Request) (*http.Response, error) {
				return nil, io.EOF
			},
			wantUnknown: true,
		},
		{
			name: "dial failed",
			roundTrip: func(*http.Request) (*http.Response, error) {
				return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.ENOENT}
			},
		},
		{
			name: "response received but malformed",
			roundTrip: func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("{")),
				}, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &ublkdClient{httpClient: &http.Client{Transport: tc.roundTrip}}
			_, _, err := client.Add(context.Background(), "/work/config.json")
			if err == nil {
				t.Fatal("Add must fail")
			}
			if got := errors.Is(err, errAddOutcomeUnknown); got != tc.wantUnknown {
				t.Fatalf("unknown outcome = %t, want %t: %v", got, tc.wantUnknown, err)
			}
		})
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
		"unknown.example.com/x/y":            ":",
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

func TestManifestRequiresRelativeRootfsDrive(t *testing.T) {
	for _, path := range []string{"", "/dev/ublkb2"} {
		document := testVMManifest()
		document.Machine.RootfsDrivePath = path
		if err := validateManifest(document, "firecracker", true); err == nil {
			t.Fatalf("drive %q must fail even with --force", path)
		}
	}
	document := testVMManifest()
	if err := validateManifest(document, "/missing/firecracker", true); err != nil {
		t.Fatal(err)
	}
}

func TestCreateHasNoSpecOverrides(t *testing.T) {
	flags := newCreateCommand().Flags()
	for _, removed := range []string{"vcpu", "memory-mb", "rootfs-drive-id", "share-snapfiles"} {
		if flags.Lookup(removed) != nil {
			t.Errorf("create must not expose --%s", removed)
		}
	}
}

func TestInjectionPreservesGuestHostsAndConfinesSymlinks(t *testing.T) {
	for _, mode := range []string{"existing", "missing", "symlink", "escaping-parent"} {
		t.Run(mode, func(t *testing.T) {
			guest, source, outside := t.TempDir(), t.TempDir(), t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(source, "etc"), 0755))
			require.NoError(t, os.WriteFile(filepath.Join(source, "etc/hosts"), []byte("default"), 0644))
			sentinel := filepath.Join(outside, "hosts")
			require.NoError(t, os.WriteFile(sentinel, []byte("host-private"), 0600))
			if mode == "escaping-parent" {
				require.NoError(t, os.Symlink(outside, filepath.Join(guest, "etc")))
			} else {
				require.NoError(t, os.MkdirAll(filepath.Join(guest, "etc"), 0755))
				if mode == "existing" {
					require.NoError(t, os.WriteFile(filepath.Join(guest, "etc/hosts"), []byte("10.0.0.1 custom"), 0644))
				}
				if mode == "symlink" {
					require.NoError(t, os.Symlink(sentinel, filepath.Join(guest, "etc/hosts")))
				}
			}
			err := copyInjection(guest, source)
			if mode == "escaping-parent" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				if mode == "symlink" {
					target, e := os.Readlink(filepath.Join(guest, "etc/hosts"))
					require.NoError(t, e)
					require.Equal(t, sentinel, target)
				} else {
					data, e := os.ReadFile(filepath.Join(guest, "etc/hosts"))
					require.NoError(t, e)
					want := "default"
					if mode == "existing" {
						want = "10.0.0.1 custom"
					}
					require.Equal(t, want, string(data))
				}
			}
			raw, err := os.ReadFile(sentinel)
			require.NoError(t, err)
			require.Equal(t, "host-private", string(raw))
		})
	}
}
