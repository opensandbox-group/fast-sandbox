package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	infracontract "fast-sandbox/internal/infra/contract"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
)

// firecrackerConfigForTest returns the built-in Firecracker configuration with
// the StateRoot redirected to a test directory.
func firecrackerConfigForTest(t *testing.T, root string) runtimecatalog.FirecrackerConfig {
	t.Helper()
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	profile.Firecracker.StateRoot = root
	return *profile.Firecracker
}

// fakeNetworkDriver no-ops the host netns preparation steps but assigns the
// guest tap name, which the restore path's network_overrides requires.
type fakeNetworkDriver struct{}

func (fakeNetworkDriver) Prepare(_ context.Context, slot *fastletnetwork.Slot) error {
	slot.GuestTap = "fc-tap"
	return nil
}
func (fakeNetworkDriver) Validate(_ context.Context, slot *fastletnetwork.Slot) error {
	if slot.GuestTap == "" {
		return errors.New("no guest tap")
	}
	return nil
}
func (fakeNetworkDriver) Destroy(context.Context, *fastletnetwork.Slot) error { return nil }

// flakyDestroyDriver fails the first slot destroys so DeleteSandbox must
// surface the release failure and converge on a later retry.
type flakyDestroyDriver struct {
	fakeNetworkDriver
	failuresRemaining int
	destroyCalls      int
}

func (f *flakyDestroyDriver) Destroy(ctx context.Context, slot *fastletnetwork.Slot) error {
	f.destroyCalls++
	if f.failuresRemaining > 0 {
		f.failuresRemaining--
		return errors.New("netns delete failed: EBUSY")
	}
	return nil
}

// memoryStateStore keeps slots in memory for the manager fixture.
type memoryStateStore struct {
	mu    sync.Mutex
	slots map[string]*fastletnetwork.Slot
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{slots: make(map[string]*fastletnetwork.Slot)}
}

func (s *memoryStateStore) LoadAll(context.Context) ([]*fastletnetwork.Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slots := make([]*fastletnetwork.Slot, 0, len(s.slots))
	for _, slot := range s.slots {
		slots = append(slots, cloneSlotForTest(slot))
	}
	return slots, nil
}

func (s *memoryStateStore) Save(_ context.Context, slot *fastletnetwork.Slot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slots[slot.ID] = cloneSlotForTest(slot)
	return nil
}

func (s *memoryStateStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.slots, id)
	return nil
}

func cloneSlotForTest(slot *fastletnetwork.Slot) *fastletnetwork.Slot {
	clone := *slot
	return &clone
}

func newNetworkManagerForTest(t *testing.T) *fastletnetwork.Manager {
	return newNetworkManagerWithDriverForTest(t, fakeNetworkDriver{})
}

func newNetworkManagerWithDriverForTest(t *testing.T, driver fastletnetwork.Driver) *fastletnetwork.Manager {
	t.Helper()
	root := t.TempDir()
	manager, err := fastletnetwork.NewManager(fastletnetwork.Config{
		Capacity: 1, PodUID: "pod-1", PrivateCIDR: "172.30.0.0/24",
		StateRoot: root, NetNSRoot: filepath.Join(root, "netns"), HostNetNSRoot: filepath.Join(root, "host-netns"),
		IDGenerator: func() (string, error) { return "slot-1", nil },
		Now:         func() time.Time { return time.Unix(1720000000, 0) },
	}, driver, newMemoryStateStore())
	require.NoError(t, err)
	require.NoError(t, manager.Initialize(context.Background()))
	return manager
}

// statefulFakeServer scripts the Firecracker API and records calls.
type statefulFakeServer struct {
	mu            sync.Mutex
	calls         []string
	snapshotLoads []SnapshotLoadRequest
	snapshotDumps []SnapshotCreateRequest
	failSnapshot  bool
	running       bool
	socket        string
}

func newStatefulFakeServer(t *testing.T) *statefulFakeServer {
	server := &statefulFakeServer{}
	server.socket = startFakeFirecracker(t, server.handle)
	return server
}

func (s *statefulFakeServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	if r.URL.Path == "/snapshot/load" {
		var request SnapshotLoadRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err == nil {
			s.snapshotLoads = append(s.snapshotLoads, request)
		}
	}
	if r.URL.Path == "/snapshot/create" {
		var request SnapshotCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err == nil {
			s.snapshotDumps = append(s.snapshotDumps, request)
		}
	}
	failSnapshot := s.failSnapshot
	s.mu.Unlock()
	switch r.URL.Path {
	case "/version":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.15.1"}`))
	case "/actions":
		var action actionRequest
		_ = json.NewDecoder(r.Body).Decode(&action)
		s.mu.Lock()
		s.running = action.ActionType == "InstanceStart"
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "/vm":
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		s.mu.Lock()
		switch payload["state"] {
		case "Paused":
			s.running = false
		case "Resumed":
			s.running = true
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "/snapshot/create":
		if failSnapshot {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"dump failed"}`))
			return
		}
		s.mu.Lock()
		request := s.snapshotDumps[len(s.snapshotDumps)-1]
		s.mu.Unlock()
		// Emulate the VMM: write the two snapshot files where the request
		// points so the driver's staging assembly finds them.
		_ = os.WriteFile(request.SnapshotPath, []byte("vmstate-dump-data"), 0o640)
		_ = os.WriteFile(request.MemFilePath, []byte("memory-dump-data"), 0o640)
		w.WriteHeader(http.StatusNoContent)
	case "/":
		s.mu.Lock()
		state := "NotStarted"
		if s.running {
			state = "Running"
		}
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"state":"` + state + `"}`))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *statefulFakeServer) recordedCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *statefulFakeServer) recordedSnapshotLoads() []SnapshotLoadRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SnapshotLoadRequest(nil), s.snapshotLoads...)
}

// driverFixture wires a Driver with fake Firecracker, process, and network
// backends.
type driverFixture struct {
	driver      *Driver
	runner      *infraRunner
	launcher    *fakeProcessRunner
	server      *statefulFakeServer
	killCalls   []int
	stateRoot   string
	manager     *fastletnetwork.Manager
	sandboxSpec fastletapi.RuntimeSandboxConfig
}

func newDriverFixture(t *testing.T) *driverFixture {
	t.Helper()
	stateRoot := t.TempDir()
	config := firecrackerConfigForTest(t, stateRoot)
	profile := runtimecatalog.RuntimeProfile{
		Name: apiv1alpha2.RuntimeFirecracker, ProfileHash: "hash",
		Firecracker:  &config,
		Capabilities: runtimecatalog.Capabilities{DefaultState: runtimecatalog.CapabilityReady},
	}
	server := newStatefulFakeServer(t)
	launcher := &fakeProcessRunner{}
	runner := &infraRunner{}
	fixture := &driverFixture{
		runner: runner, launcher: launcher, server: server, stateRoot: stateRoot,
		manager: newNetworkManagerForTest(t),
	}
	driver := &Driver{
		profile: profile, config: config,
		runner: runner, launcher: launcher,
		newClient: func(string) *Client { return NewClient(server.socket) },
		stat:      func(string) (os.FileInfo, error) { return fakeFileInfoForTest{}, nil },
		killProcess: func(pid int) error {
			fixture.killCalls = append(fixture.killCalls, pid)
			return nil
		},
		probeProcess:         func(int) error { return nil },
		waitSocket:           func(context.Context, string, time.Duration) error { return nil },
		processes:            make(map[string]Process),
		imageGCInterval:      defaultImageGCInterval,
		imageCacheLimitBytes: defaultImageCacheLimitBytes,
	}
	driver.SetNetworkManager(fixture.manager)
	fixture.driver = driver
	fixture.sandboxSpec = fastletapi.RuntimeSandboxConfig{
		Spec: fastletapi.SandboxSpec{
			Image: "example.com/app:v1", CPU: "2", Memory: "1Gi",
			RuntimeProfileHash: "hash", ResourceProfileHash: "r-hash",
		},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "sandbox-1", Name: "sandbox-1", Namespace: "tenant-a",
			InstanceGeneration: 1, RuntimeInstanceID: "runtime-1", AssignmentAttempt: 1, FastletPodUID: "pod-1"},
	}
	return fixture
}

func ensureInput(config *fastletapi.RuntimeSandboxConfig) *fastletapi.EnsureSandboxInput {
	if config == nil {
		return nil
	}
	return &fastletapi.EnsureSandboxInput{RequestID: "test-request", Sandbox: *config}
}

func (f *driverFixture) prepareCachedImage(t *testing.T, image string) {
	t.Helper()
	dir := filepath.Join(f.stateRoot, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), []byte("rootfs-image-data"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, vmstateSnapshotName), []byte("vmstate-snapshot-data"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, memorySnapshotName), []byte("memory-snapshot-data"), 0o640))
	manifest, err := json.Marshal(map[string]any{
		"machine": map[string]any{"vcpu": "2", "memory": "1Gi"},
		// The baked guest address every slot netns translates its slot IP
		// to (per-clone clone model). Must stay the reserved convention
		// address (gateway + 2 of the runtime CIDR).
		"guestNetwork": map[string]any{"iface": "eth0", "mac": "02:00:00:00:00:01", "ip": "172.30.0.3", "gateway": "172.30.0.1", "netmask": "255.255.255.0"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o640))
}

func TestNewRequiresProfileConfig(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	require.NotNil(t, profile.Firecracker)

	profile.Firecracker = nil
	_, err = New(profile)
	require.Error(t, err)
}

func TestInitializeValidatesBootConfig(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	broken, err := New(fixture.driver.profile)
	require.NoError(t, err)
	broken.config.BinaryPath = ""
	require.ErrorIs(t, broken.Initialize(context.Background(), ""), ErrInvalidConfig)

	invalid, err := New(fixture.driver.profile)
	require.NoError(t, err)
	invalid.config.BootTimeoutSeconds = 0
	require.ErrorIs(t, invalid.Initialize(context.Background(), ""), ErrInvalidConfig)
}

func TestProbeCapabilitiesFailsClosedWithProfileGate(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.driver.profile.Capabilities.DefaultState = runtimecatalog.CapabilityUnsupported
	fixture.driver.profile.Capabilities.Reason = "FirecrackerDriverUnimplemented"
	report := fixture.driver.ProbeCapabilities(context.Background())
	require.Equal(t, runtimecatalog.CapabilityUnsupported, report.State)
	require.Equal(t, "FirecrackerDriverUnimplemented", report.Reason)
}

func TestProbeCapabilitiesReady(t *testing.T) {
	fixture := newDriverFixture(t)
	report := fixture.driver.ProbeCapabilities(context.Background())
	require.Equal(t, runtimecatalog.CapabilityReady, report.State)
	require.Empty(t, report.Missing)

	fixture.driver.stat = func(path string) (os.FileInfo, error) {
		if path == "/dev/kvm" {
			return nil, os.ErrNotExist
		}
		return fakeFileInfoForTest{}, nil
	}
	report = fixture.driver.ProbeCapabilities(context.Background())
	require.Equal(t, runtimecatalog.CapabilityDegraded, report.State)
	require.Equal(t, "KVMUnavailable", report.Reason)
	require.Contains(t, report.Missing, "/dev/kvm")
}

type fakeFileInfoForTest struct{}

func (fakeFileInfoForTest) Name() string       { return "probe" }
func (fakeFileInfoForTest) Size() int64        { return 0 }
func (fakeFileInfoForTest) Mode() os.FileMode  { return 0 }
func (fakeFileInfoForTest) ModTime() time.Time { return time.Time{} }
func (fakeFileInfoForTest) IsDir() bool        { return false }
func (fakeFileInfoForTest) Sys() any           { return nil }

func TestEnsureSandboxDeliversInfraComponents(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	infraJSON := filepath.Join(t.TempDir(), "infra.json")
	require.NoError(t, os.WriteFile(infraJSON, []byte(`{"version":1}`), 0o400))
	fixture.driver.prepareInfra = func(context.Context, *fastletapi.RuntimeSandboxConfig) (fastletinfra.PreparedInstance, error) {
		return fastletinfra.PreparedInstance{
			ConfigHostPath: infraJSON,
			Mounts: []fastletinfra.Mount{
				{Source: infraJSON, Destination: "/.fast/run/infra.json", Options: []string{"ro"}},
			},
			Services: []infracontract.ServiceEndpoint{
				{Component: "execd", Protocol: "HTTP", Port: 44772},
			},
			Diagnostics: []infracontract.ComponentDiagnostic{{Component: "execd", State: "Starting"}},
		}, nil
	}
	fixture.driver.infraMgr = &fastletinfra.Manager{} // enable the infra path; prepareInfra is faked

	metadata, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	require.Len(t, metadata.InfraServices, 1)
	require.Equal(t, uint32(44772), metadata.InfraServices[0].Port)
	require.Len(t, metadata.InfraDiagnostics, 1)

	joined := strings.Join(fixture.runner.commands, "\n")
	require.Contains(t, joined, "mount -o loop")
	require.Contains(t, joined, "/.fast/run/infra.json")
	require.Contains(t, joined, "umount ")

	// The persisted state carries the services for route publication.
	directory, err := sandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	state, err := loadState(directory)
	require.NoError(t, err)
	require.Len(t, state.InfraServices, 1)
}

func TestEnsureSandboxInfraFailureReleasesResources(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	fixture.driver.prepareInfra = func(context.Context, *fastletapi.RuntimeSandboxConfig) (fastletinfra.PreparedInstance, error) {
		return fastletinfra.PreparedInstance{}, errors.New("plan revision mismatch")
	}
	root := t.TempDir()
	store, err := fastletinfra.NewArtifactStore(filepath.Join(root, "pod"), filepath.Join(root, "host"))
	require.NoError(t, err)
	fixture.driver.infraMgr, err = fastletinfra.NewManagerWithConfig(fastletinfra.ManagerConfig{Store: store, Resolver: fakeResolver{}})
	require.NoError(t, err)

	_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.ErrorIs(t, err, ErrInfraUnavailable)
	require.Empty(t, fixture.launcher.started)
	require.Equal(t, 0, fixture.manager.Snapshot().Bound)

	// A failed Create must not leak the jail / per-sandbox state dirs it
	// prepared before the Infra failure (regression: orphaned jail roots).
	directory, err := sandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	require.NoDirExists(t, directory)
}

// fakeResolver is a minimal ArtifactResolver used to construct an Infra
// Manager in tests.
type fakeResolver struct{}

func (fakeResolver) Prepare(context.Context, infracatalog.ArtifactSource, *fastletinfra.ArtifactStore) (fastletinfra.PreparedSource, error) {
	return fastletinfra.PreparedSource{}, nil
}

func TestEnsureSandboxBootsVM(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	metadata, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	require.Equal(t, "sandbox-1", metadata.ContainerID)
	require.Equal(t, 4242, metadata.PID)
	require.Equal(t, string(PhaseRunning), metadata.Phase)
	require.Empty(t, metadata.InfraServices)
	require.Equal(t, "sandbox-1", metadata.Config.Identity.SandboxUID)
	require.Equal(t, "172.30.0.2", metadata.Allocation.Network.IP)
	require.NotEmpty(t, metadata.Allocation.Network.SlotID)

	require.Len(t, fixture.launcher.started, 1)
	require.Equal(t, "/usr/local/bin/firecracker", fixture.launcher.started[0][0])
	require.Contains(t, fixture.launcher.started[0], "--api-sock")

	// The guest data plane is applied from the manifest guestNetwork: the
	// bound slot records the baked guest address all slots translate to.
	slot, exists := fixture.manager.Lookup("sandbox-1")
	require.True(t, exists)
	require.Equal(t, "172.30.0.3", slot.GuestIP)

	calls := fixture.server.recordedCalls()
	// v1.16 restore: LoadSnapshot is the first (and only pre-boot) API call;
	// machine/drive/network configuration is restored from the vmstate and
	// is rejected by Firecracker before load. The boot source is never set.
	require.Equal(t, []string{"PUT /snapshot/load", "PATCH /vm", "GET /"}, calls)

	directory, err := sandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	state, err := loadState(directory)
	require.NoError(t, err)
	require.Equal(t, PhaseRunning, state.Phase)
	require.Equal(t, metadata.Allocation, state.Allocation, "network allocation must be durable driver state")

	instanceRootfs := filepath.Join(directory, instanceRootfsName)
	content, err := os.ReadFile(instanceRootfs)
	require.NoError(t, err)
	require.Equal(t, "rootfs-image-data", string(content))
}

func TestEnsureSandboxRestoresGoldenSnapshot(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	metadata, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	require.Equal(t, string(PhaseRunning), metadata.Phase)

	// v1.16 restore: LoadSnapshot is the first (and only pre-boot) API call;
	// the kernel boot source is never configured, and the machine/drive/nic
	// configuration comes from the vmstate.
	calls := fixture.server.recordedCalls()
	require.Equal(t, []string{"PUT /snapshot/load", "PATCH /vm", "GET /"}, calls)

	// The snapshot/load payload references the cached golden artifacts, the
	// per-instance NIC tap override, and leaves the VM paused for the
	// explicit PATCH /vm resume.
	loads := fixture.server.recordedSnapshotLoads()
	require.Len(t, loads, 1)
	imageDir := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(fixture.sandboxSpec.Spec.Image))
	require.Equal(t, filepath.Join(imageDir, vmstateSnapshotName), loads[0].SnapshotPath)
	require.Equal(t, "File", loads[0].MemBackend.BackendType)
	require.Equal(t, filepath.Join(imageDir, memorySnapshotName), loads[0].MemBackend.BackendPath)
	require.False(t, loads[0].ResumeVM)
	require.Len(t, loads[0].NetworkOverrides, 1)
	require.Equal(t, "eth0", loads[0].NetworkOverrides[0].IfaceID)
	require.Equal(t, "fc-tap", loads[0].NetworkOverrides[0].HostDevName)
}

func TestEnsureSandboxRestoreMachineConfigFromManifest(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	// The fixture manifest records {vcpu: 2, memory: 1Gi}; the fixture spec
	// requests the same so restore succeeds with the manifest machine.
	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)

	// A request memory below the snapshot memory fails restore explicitly.
	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), fixture.sandboxSpec.Identity.SandboxUID))
	require.Eventually(t, func() bool { return fixture.manager.Snapshot().Clean == 1 }, time.Second, time.Millisecond,
		"network slot replenishment is asynchronous after delete")
	small := fixture.sandboxSpec
	small.Spec.Memory = "256Mi"
	_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&small))
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "below the template snapshot memory")
}

func TestEnsureSandboxMissingSnapshotFilesReleasesSlot(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	// Only the rootfs exists; the golden snapshot set is incomplete.
	dir := filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(fixture.sandboxSpec.Spec.Image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), []byte("rootfs-image-data"), 0o640))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.ErrorIs(t, err, ErrImageNotReady)
	require.Empty(t, fixture.launcher.started)
	require.Equal(t, 0, fixture.manager.Snapshot().Bound)
}

func TestEnsureSandboxIsIdempotent(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	firstInput := ensureInput(&fixture.sandboxSpec)
	firstInput.RequestID = "invocation-1"
	first, err := fixture.driver.EnsureSandbox(context.Background(), firstInput)
	require.NoError(t, err)
	secondInput := ensureInput(&fixture.sandboxSpec)
	secondInput.RequestID = "invocation-2"
	second, err := fixture.driver.EnsureSandbox(context.Background(), secondInput)
	require.NoError(t, err)
	require.Equal(t, first.ContainerID, second.ContainerID)
	require.Len(t, fixture.launcher.started, 1)
}

func TestEnsureSandboxReplacesDifferentRuntimeIdentity(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	first, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)

	replacement := fixture.sandboxSpec
	replacement.Identity.RuntimeInstanceID = "runtime-2"
	replacement.Identity.AssignmentAttempt = 2
	replacement.Identity.RouteGeneration = 2
	second, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&replacement))
	require.NoError(t, err)
	require.Equal(t, replacement.Identity, second.Config.Identity)
	require.Equal(t, first.Allocation.Network.SlotID, second.Allocation.Network.SlotID)
	require.Len(t, fixture.launcher.started, 2)
	require.Equal(t, 1, fixture.manager.Snapshot().Bound)

	directory, err := sandboxDir(fixture.stateRoot, replacement.Identity.SandboxUID)
	require.NoError(t, err)
	persisted, err := loadState(directory)
	require.NoError(t, err)
	require.Equal(t, replacement.Identity, persisted.Config.Identity)
}

func TestEnsureSandboxValidatesIdentity(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	spec := fixture.sandboxSpec
	spec.Identity.AssignmentAttempt = 0
	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&spec))
	require.ErrorIs(t, err, ErrInvalidConfig)

	_, err = fixture.driver.EnsureSandbox(context.Background(), nil)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestEnsureSandboxMissingImageReleasesSlot(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.ErrorIs(t, err, ErrImageNotReady)
	require.Empty(t, fixture.launcher.started)
	require.Equal(t, 0, fixture.manager.Snapshot().Bound)
}

func TestEnsureSandboxCleansStaleRecord(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	// Plant a stale record whose process is dead.
	directory, err := ensureSandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	require.NoError(t, saveState(directory, &SandboxState{
		Config: fixture.sandboxSpec, Phase: PhaseRunning, PID: 999, APIAddress: filepath.Join(directory, "dead.sock"),
	}))
	fixture.driver.probeProcess = func(pid int) error {
		if pid == 999 {
			return os.ErrNotExist
		}
		return nil
	}

	metadata, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	require.Equal(t, "sandbox-1", metadata.ContainerID)
	require.Len(t, fixture.launcher.started, 1)
	require.Contains(t, fixture.killCalls, 999)
}

func TestInspectSandbox(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)

	metadata, err := fixture.driver.InspectSandbox(context.Background(), "sandbox-1")
	require.NoError(t, err)
	require.Equal(t, string(PhaseRunning), metadata.Phase)

	_, err = fixture.driver.InspectSandbox(context.Background(), "sandbox-missing")
	require.ErrorIs(t, err, ErrSandboxNotFound)
}

func TestDeleteSandboxIsIdempotent(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), "sandbox-1"))
	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), "sandbox-1"))
	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), "sandbox-missing"))

	directory, err := sandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	_, err = os.Stat(directory)
	require.True(t, os.IsNotExist(err))
}

func TestDeleteSandboxRetriesFailedSlotRelease(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	flaky := &flakyDestroyDriver{failuresRemaining: 1}
	manager := newNetworkManagerWithDriverForTest(t, flaky)
	fixture.driver.SetNetworkManager(manager)

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	require.Equal(t, 1, manager.Snapshot().Bound)

	directory, err := sandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)

	// The slot destroy fails (netns EBUSY): the delete must NOT report
	// success, and the durable state directory must stay so a delete-failed
	// retry can re-release the slot once the dying VMM drains.
	err = fixture.driver.DeleteSandbox(context.Background(), "sandbox-1")
	require.ErrorContains(t, err, "release network slot")
	require.DirExists(t, directory)
	require.Equal(t, 1, flaky.destroyCalls)
	require.Equal(t, 1, manager.Snapshot().Destroying)
	require.Zero(t, manager.Snapshot().Bound)

	// The retry reaches the Destroying leftover and converges.
	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), "sandbox-1"))
	require.NoDirExists(t, directory)
	require.Equal(t, 2, flaky.destroyCalls)
	require.Zero(t, manager.Snapshot().Destroying)
	require.Zero(t, manager.Snapshot().Bound)
}

func TestListManagedSandboxesFiltersNamespace(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	fixture.driver.SetNamespace("tenant-a")
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)

	managed, err := fixture.driver.ListManagedSandboxes(context.Background())
	require.NoError(t, err)
	require.Len(t, managed, 1)
	require.Equal(t, "sandbox-1", managed[0].Config.Identity.SandboxUID)

	fixture.driver.SetNamespace("tenant-b")
	managed, err = fixture.driver.ListManagedSandboxes(context.Background())
	require.NoError(t, err)
	require.Empty(t, managed)
}

func TestRecoverRuntimeResourcesCleansDeadVM(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.driver.probeProcess = func(int) error { return os.ErrNotExist }
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	directory, err := ensureSandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	require.NoError(t, saveState(directory, &SandboxState{
		Config: fixture.sandboxSpec, Phase: PhaseRunning, PID: 777, APIAddress: filepath.Join(directory, "dead.sock"),
	}))

	require.NoError(t, fixture.driver.RecoverRuntimeResources(context.Background(), nil))
	_, err = os.Stat(directory)
	require.True(t, os.IsNotExist(err))
	require.Contains(t, fixture.killCalls, 777)
	require.Equal(t, 0, fixture.manager.Snapshot().Bound)
}

func TestRecoverRuntimeResourcesKeepsAliveVM(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)

	require.NoError(t, fixture.driver.RecoverRuntimeResources(context.Background(), nil))
	managed, err := fixture.driver.ListManagedSandboxes(context.Background())
	require.NoError(t, err)
	require.Len(t, managed, 1)
	require.Equal(t, "172.30.0.2", managed[0].Allocation.Network.IP)
	require.NotEmpty(t, managed[0].Allocation.Network.SlotID)
}

func TestGetAccessDescriptor(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)

	access, err := fixture.driver.GetAccessDescriptor("sandbox-1")
	require.NoError(t, err)
	require.Equal(t, "172.30.0.2", access.Address)
	require.NoError(t, access.Validate())

	_, err = fixture.driver.GetAccessDescriptor("sandbox-missing")
	require.ErrorIs(t, err, ErrSandboxNotFound)
}

func TestRuntimeResourceAvailable(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
	// A prepared clean slot admits.
	require.True(t, fixture.driver.RuntimeResourceAvailable())

	// No manager: the gate fails closed.
	fixture.driver.mu.Lock()
	fixture.driver.networkManager = nil
	fixture.driver.mu.Unlock()
	require.False(t, fixture.driver.RuntimeResourceAvailable())
}

func TestCloseKillsManagedProcesses(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)

	fixture.driver.mu.Lock()
	process := fixture.driver.processes["sandbox-1"]
	fixture.driver.mu.Unlock()
	require.NotNil(t, process)

	require.NoError(t, fixture.driver.Close())
	fixture.driver.mu.Lock()
	require.Empty(t, fixture.driver.processes)
	fixture.driver.mu.Unlock()
	require.True(t, process.(*fakeProcess).killed)
}

func TestResolveMachineConfig(t *testing.T) {
	config := firecrackerConfigForTest(t, t.TempDir())

	request, err := resolveMachineConfig(fastletapi.SandboxSpec{}, config)
	require.NoError(t, err)
	require.Equal(t, int(config.DefaultVCPUs), request.VCPUs)
	require.Equal(t, 512, request.MemSizeMiB)

	request, err = resolveMachineConfig(fastletapi.SandboxSpec{CPU: "500m", Memory: "1Gi"}, config)
	require.NoError(t, err)
	require.Equal(t, 1, request.VCPUs)
	require.Equal(t, 1024, request.MemSizeMiB)

	request, err = resolveMachineConfig(fastletapi.SandboxSpec{CPU: "2.5", Memory: "256Mi"}, config)
	require.NoError(t, err)
	require.Equal(t, 3, request.VCPUs)
	require.Equal(t, 256, request.MemSizeMiB)

	_, err = resolveMachineConfig(fastletapi.SandboxSpec{CPU: "not-a-quantity"}, config)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestValidateRestoreMachineConfigUsesManifest(t *testing.T) {
	stateRoot := t.TempDir()
	image := "example.com/app:v1"
	dir := filepath.Join(stateRoot, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	manifest, err := json.Marshal(map[string]any{
		"machine": map[string]any{"vcpu": "1", "memory": "512Mi"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o640))
	config := firecrackerConfigForTest(t, stateRoot)

	// The manifest machine tuple is authoritative: a request profile that
	// differs (more cpu/mem) still validates.
	require.NoError(t, validateRestoreMachineConfig(fastletapi.SandboxSpec{CPU: "4", Memory: "1Gi"}, config, stateRoot, image))

	// A request memory below the snapshot memory is rejected.
	err = validateRestoreMachineConfig(fastletapi.SandboxSpec{Memory: "256Mi"}, config, stateRoot, image)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "below the template snapshot memory")
}

func TestValidateRestoreMachineConfigFallsBackWithoutManifest(t *testing.T) {
	stateRoot := t.TempDir()
	image := "example.com/app:v1"
	config := firecrackerConfigForTest(t, stateRoot)

	require.NoError(t, validateRestoreMachineConfig(fastletapi.SandboxSpec{CPU: "2", Memory: "1Gi"}, config, stateRoot, image))
	require.ErrorIs(t, validateRestoreMachineConfig(fastletapi.SandboxSpec{CPU: "not-a-quantity"}, config, stateRoot, image), ErrInvalidConfig)
}

func TestResolveRestoreSnapshotFilesRequiresBothArtifacts(t *testing.T) {
	stateRoot := t.TempDir()
	image := "example.com/app:v1"
	dir := filepath.Join(stateRoot, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, vmstateSnapshotName), []byte("vmstate"), 0o640))

	_, _, err := resolveRestoreSnapshotFiles(stateRoot, image)
	require.ErrorIs(t, err, ErrImageNotReady)

	require.NoError(t, os.WriteFile(filepath.Join(dir, memorySnapshotName), []byte("memory"), 0o640))
	vmstate, memory, err := resolveRestoreSnapshotFiles(stateRoot, image)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, vmstateSnapshotName), vmstate)
	require.Equal(t, filepath.Join(dir, memorySnapshotName), memory)
}

func TestResolveBakedGuestIPFallsBackWithoutManifest(t *testing.T) {
	stateRoot := t.TempDir()
	image := "example.com/app:v1"
	dir := filepath.Join(stateRoot, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsImageName), []byte("rootfs"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, vmstateSnapshotName), []byte("vmstate"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, memorySnapshotName), []byte("memory"), 0o640))

	// No manifest: the BakedGuestIP convention (gateway + 2, the baked
	// address of the builder/E2E prep) is the fallback; no MTU is known.
	guestIP, guestMTU, err := resolveBakedGuestIP(stateRoot, image, &fastletnetwork.Slot{Gateway: "172.30.0.1"})
	require.NoError(t, err)
	require.Equal(t, "172.30.0.3", guestIP)
	require.Zero(t, guestMTU)

	// A manifest without an MTU keeps reporting the baked address only.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(
		`{"guestNetwork":{"ip":"172.30.0.3"}}`), 0o640))
	guestIP, guestMTU, err = resolveBakedGuestIP(stateRoot, image, &fastletnetwork.Slot{Gateway: "172.30.0.1", PrivateCIDR: "172.30.0.0/24"})
	require.NoError(t, err)
	require.Equal(t, "172.30.0.3", guestIP)
	require.Zero(t, guestMTU)

	// A manifest with an MTU reports it so callers can validate the slot
	// data plane against the frozen guest value.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(
		`{"guestNetwork":{"ip":"172.30.0.3","mtu":1500}}`), 0o640))
	guestIP, guestMTU, err = resolveBakedGuestIP(stateRoot, image, &fastletnetwork.Slot{Gateway: "172.30.0.1", PrivateCIDR: "172.30.0.0/24"})
	require.NoError(t, err)
	require.Equal(t, "172.30.0.3", guestIP)
	require.Equal(t, 1500, guestMTU)

	// A corrupt manifest fails explicitly instead of silently guessing.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{"), 0o640))
	_, _, err = resolveBakedGuestIP(stateRoot, image, &fastletnetwork.Slot{Gateway: "172.30.0.1"})
	require.Error(t, err)
}

// TestResolveBakedGuestIPRejectsRuntimeMismatch covers the frozen guest
// network (manifest) vs runtime CIDR alignment: the manifest is only
// authoritative when it describes the subnet the slot data plane actually
// implements.
func TestResolveBakedGuestIPRejectsRuntimeMismatch(t *testing.T) {
	stateRoot := t.TempDir()
	image := "example.com/app:v1"
	dir := filepath.Join(stateRoot, imageCacheDir, imageKey(image))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	slot := &fastletnetwork.Slot{Gateway: "172.30.0.1", PrivateCIDR: "172.30.0.0/24"}

	writeManifest := func(payload string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(payload), 0o640))
	}

	// The aligned case (builder convention) passes.
	writeManifest(`{"guestNetwork":{"ip":"172.30.0.3","gateway":"172.30.0.1","netmask":"255.255.255.0","mtu":1500}}`)
	guestIP, guestMTU, err := resolveBakedGuestIP(stateRoot, image, slot)
	require.NoError(t, err)
	require.Equal(t, "172.30.0.3", guestIP)
	require.Equal(t, 1500, guestMTU)

	// A baked address outside the runtime CIDR.
	writeManifest(`{"guestNetwork":{"ip":"10.5.0.3","gateway":"10.5.0.1","netmask":"255.255.255.0"}}`)
	_, _, err = resolveBakedGuestIP(stateRoot, image, slot)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "outside the private CIDR")

	// A baked address inside the CIDR but not the reserved convention
	// address: the IPAM only reserves gateway+2, any other address can be
	// allocated to a sibling slot and shadow the guest.
	writeManifest(`{"guestNetwork":{"ip":"172.30.0.9","gateway":"172.30.0.1"}}`)
	_, _, err = resolveBakedGuestIP(stateRoot, image, slot)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "reserved address")

	// A baked gateway differing from the runtime gateway: guest DNS and
	// proxy-ARP termination break.
	writeManifest(`{"guestNetwork":{"ip":"172.30.0.3","gateway":"10.5.0.1"}}`)
	_, _, err = resolveBakedGuestIP(stateRoot, image, slot)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "runtime gateway")

	// A baked netmask differing from the runtime prefix length.
	writeManifest(`{"guestNetwork":{"ip":"172.30.0.3","gateway":"172.30.0.1","netmask":"255.255.0.0"}}`)
	_, _, err = resolveBakedGuestIP(stateRoot, image, slot)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "netmask")
}

func TestCloseResetsDriver(t *testing.T) {
	fixture := newDriverFixture(t)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
	require.NoError(t, fixture.driver.Close())
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
}

// enableJailer activates the jailer mode on a fixture (the fake launcher
// records the jailer argv; the jail root is prepared on the real StateRoot).
func (f *driverFixture) enableJailer() {
	f.driver.config.JailerPath = "/usr/local/bin/jailer"
}

func TestEnsureSandboxJailerMode(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.enableJailer()
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	metadata, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	require.Equal(t, string(PhaseRunning), metadata.Phase)

	// The jailer launches firecracker inside the slot netns and its chroot.
	require.Len(t, fixture.launcher.started, 1)
	require.Equal(t, "/usr/local/bin/jailer", fixture.launcher.started[0][0])
	started := fixture.launcher.started[0]
	require.Equal(t, "--id", started[argvIndex(started, "--id")])
	require.Equal(t, "--netns", started[argvIndex(started, "--netns")])
	require.Equal(t, "--chroot-base-dir", started[argvIndex(started, "--chroot-base-dir")])
	require.Equal(t, filepath.Join(fixture.stateRoot, jailerChrootBaseDir), started[argvIndex(started, "--chroot-base-dir")+1])
	// The slot netns is the manager's NetNSPath.
	slot, exists := fixture.manager.Lookup("sandbox-1")
	require.True(t, exists)
	require.Equal(t, slot.NetNSPath, started[argvIndex(started, "--netns")+1])
	// The post--- firecracker args use the chroot-relative socket path.
	require.Contains(t, started, jailerChrootAPISock)

	// The jail root holds the instance rootfs copy and the hard-linked
	// snapshot files; the persisted API address points at the jail socket.
	jailRoot := jailerRoot(filepath.Join(fixture.stateRoot, jailerChrootBaseDir), "firecracker", "sandbox-1")
	content, err := os.ReadFile(filepath.Join(jailRoot, rootfsImageName))
	require.NoError(t, err)
	require.Equal(t, "rootfs-image-data", string(content))
	for _, name := range []string{vmstateSnapshotName, memorySnapshotName} {
		inJail, statErr := os.Stat(filepath.Join(jailRoot, jailerChrootSnapshotsDir, name))
		require.NoError(t, statErr)
		inCache, statErr := os.Stat(filepath.Join(fixture.stateRoot, imageCacheDir, imageKey(fixture.sandboxSpec.Spec.Image), name))
		require.NoError(t, statErr)
		require.True(t, os.SameFile(inJail, inCache), "%s must be hard-linked into the jail root", name)
	}
	directory, err := sandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	state, err := loadState(directory)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(jailRoot, "api.sock"), state.APIAddress)

	// The restore payload addresses the snapshots via the chroot-relative
	// paths and overrides the NIC tap with the in-namespace tap name.
	loads := fixture.server.recordedSnapshotLoads()
	require.Len(t, loads, 1)
	require.Equal(t, "/snapshots/"+vmstateSnapshotName, loads[0].SnapshotPath)
	require.Equal(t, "File", loads[0].MemBackend.BackendType)
	require.Equal(t, "/snapshots/"+memorySnapshotName, loads[0].MemBackend.BackendPath)
	require.Len(t, loads[0].NetworkOverrides, 1)
	require.Equal(t, "eth0", loads[0].NetworkOverrides[0].IfaceID)
	require.Equal(t, slot.GuestTap, loads[0].NetworkOverrides[0].HostDevName)
}

func TestDeleteSandboxRemovesJailRoot(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.enableJailer()
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	_, err := fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.NoError(t, err)
	jailDir := filepath.Dir(jailerRoot(filepath.Join(fixture.stateRoot, jailerChrootBaseDir), "firecracker", "sandbox-1"))
	_, err = os.Stat(jailDir)
	require.NoError(t, err)

	require.NoError(t, fixture.driver.DeleteSandbox(context.Background(), "sandbox-1"))
	_, err = os.Stat(jailDir)
	require.True(t, os.IsNotExist(err))
}

func TestRecoverRuntimeResourcesRemovesJailRoot(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.enableJailer()
	fixture.driver.probeProcess = func(int) error { return os.ErrNotExist }
	require.NoError(t, fixture.driver.Initialize(context.Background(), ""))

	directory, err := ensureSandboxDir(fixture.stateRoot, "sandbox-1")
	require.NoError(t, err)
	require.NoError(t, saveState(directory, &SandboxState{
		Config: fixture.sandboxSpec, Phase: PhaseRunning, PID: 777, APIAddress: filepath.Join(directory, "dead.sock"),
	}))
	jailDir := filepath.Dir(jailerRoot(filepath.Join(fixture.stateRoot, jailerChrootBaseDir), "firecracker", "sandbox-1"))
	require.NoError(t, os.MkdirAll(filepath.Join(jailDir, "root"), 0o750))

	require.NoError(t, fixture.driver.RecoverRuntimeResources(context.Background(), nil))
	_, err = os.Stat(jailDir)
	require.True(t, os.IsNotExist(err))
}
