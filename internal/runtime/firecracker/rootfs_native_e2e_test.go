//go:build linux && firecracker

package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	fastletsandbox "fast-sandbox/internal/fastlet/sandbox"
	"fast-sandbox/internal/guestcontrol"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	agentpull "fast-sandbox/internal/runtime/firecracker/agent"
	agentserver "fast-sandbox/internal/runtime/firecracker/agent/server"
	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"
)

// This test uses real KVM, XFS, Fastlet, runtime-agent UDS RPCs and signed HTTP
// artifact requests. FSB_TEST_S3_CONFIG selects a real, disposable S3 bucket;
// otherwise the object endpoint is a local fixture without authentication.
func TestNativeRootfsFastletChainE2E(t *testing.T) {
	seed := os.Getenv("FSB_TEST_NATIVE_SEED")
	if seed == "" {
		t.Skip("requires a disposable ext4 disk with the rootfs smoke init/helper")
	}
	binary, kernel := os.Getenv("FC_BINARY"), os.Getenv("FC_KERNEL")
	require.NotEmpty(t, binary)
	require.NotEmpty(t, kernel)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	root, err := os.MkdirTemp(filepath.Dir(seed), "ns3-")
	require.NoError(t, err)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("retained evidence: %s", root)
		} else {
			_ = os.RemoveAll(root)
		}
	})
	objects := filepath.Join(root, "objects")
	require.NoError(t, os.MkdirAll(objects, 0700))
	var rejectUpload atomic.Bool
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Join(objects, filepath.Clean("/"+r.URL.Path))
		switch r.Method {
		case http.MethodPut:
			if rejectUpload.Load() {
				http.Error(w, "injected upload rejection", http.StatusForbidden)
				return
			}
			if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			f, err := os.CreateTemp(filepath.Dir(name), ".upload-")
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			defer os.Remove(f.Name())
			_, copyErr := io.Copy(f, r.Body)
			closeErr := f.Close()
			if copyErr != nil || closeErr != nil {
				http.Error(w, "write failed", 500)
				return
			}
			if err := os.Rename(f.Name(), name); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			http.ServeFile(w, r, name)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer store.Close()
	credential := registryconfig.Credential{Host: "test", Username: "reader", Password: "test", WriteUsername: "writer", WritePassword: "test"}
	storeRoot, endpoint, region := "s3://test/snapshots", store.URL, "us-east-1"
	var rejectedSignatures atomic.Int64
	options := []agentpull.Option{agentpull.WithEndpoint(endpoint)}
	if configPath := os.Getenv("FSB_TEST_S3_CONFIG"); configPath != "" {
		// Credentials are read from a private file and never logged. Each run
		// publishes under its own prefix, without overwriting existing indexes.
		var config struct {
			StoreRoot  string                    `json:"storeRoot"`
			Endpoint   string                    `json:"endpoint"`
			Region     string                    `json:"region"`
			Credential registryconfig.Credential `json:"credential"`
		}
		payload, err := os.ReadFile(configPath)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(payload, &config))
		require.NotEmpty(t, config.StoreRoot)
		require.NotEmpty(t, config.Endpoint)
		storeRoot, endpoint, region, credential = strings.TrimRight(config.StoreRoot, "/")+"/"+filepath.Base(root), config.Endpoint, config.Region, config.Credential
		options = []agentpull.Option{agentpull.WithEndpoint(endpoint), agentpull.WithRegion(region), agentpull.WithHTTPClient(&http.Client{
			Timeout: time.Minute, Transport: nativeS3FaultTransport{reject: &rejectUpload, forbidden: &rejectedSignatures},
		})}
		t.Logf("real S3 service enabled; isolated artifact prefix: %s", storeRoot)
	}
	pull, err := agentpull.NewClient(storeRoot, credential, options...)
	require.NoError(t, err)
	pod := "native-rootfs-e2e"
	profile := runtimecatalog.RuntimeProfile{Name: apiv1alpha2.RuntimeFirecracker, ProfileHash: "native-rootfs", Firecracker: &runtimecatalog.FirecrackerConfig{BinaryPath: binary, KernelPath: kernel, JailerPath: os.Getenv("FC_JAILER"), RootfsPath: "/unused", StateRoot: filepath.Join(root, "state"), DefaultVCPUs: 1, DefaultMemory: "256Mi", BootTimeoutSeconds: 30}, Capabilities: runtimecatalog.Capabilities{DefaultState: runtimecatalog.CapabilityReady}}
	driver, err := New(profile)
	require.NoError(t, err)
	driver.SetFastletPodUID(pod)
	var counter atomic.Int64
	network, err := fastletnetwork.NewManager(fastletnetwork.Config{Capacity: 2, PodUID: pod, StateRoot: filepath.Join(root, "network"), NetNSRoot: "/run/netns", HostNetNSRoot: "/run/netns", IDGenerator: func() (string, error) { return fmt.Sprintf("ns3-%d-%d", os.Getpid(), counter.Add(1)), nil }, Now: time.Now, ReplenishTimeout: time.Minute}, fastletnetwork.NewGuestVMNetNSDriver(fastletnetwork.LinuxDriverConfig{}), newMemoryStateStore())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll("/run/netns", 0755))
	require.NoError(t, os.MkdirAll("/run/fast-sandbox/netns", 0755))
	require.NoError(t, network.Initialize(ctx))
	defer network.Close(context.Background())
	driver.SetNetworkManager(network)
	require.NoError(t, driver.Initialize(ctx, ""))
	defer driver.Close()
	state, err := agentstate.New(filepath.Join(root, "agent-state"))
	require.NoError(t, err)
	server := agentserver.New(agentserver.NewService(pull, state, driver.config.StateRoot), filepath.Join(root, "agent.sock"))
	serverCtx, stopServer := context.WithCancel(ctx)
	defer stopServer()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(serverCtx) }()
	defer func() { stopServer(); <-serverDone }()
	require.Eventually(t, func() bool { _, err := os.Stat(server.SocketPath()); return err == nil }, 5*time.Second, 10*time.Millisecond)
	driver.agentSocket = server.SocketPath()
	driver.newAgentClient = func(path string) (AgentClient, error) { return NewAgentClient(path, "e2e", pod) }
	resourceProfile := apiv1alpha2.NewLegacySandboxResourceProfile(resource.MustParse("1"), resource.MustParse("256Mi"), 128)
	manager, err := fastletsandbox.NewSandboxManagerWithConfig(driver, fastletsandbox.SandboxManagerConfig{Capacity: 2, FastletPodUID: pod, RuntimeName: apiv1alpha2.RuntimeFirecracker, ResourceProfile: &resourceProfile})
	require.NoError(t, err)
	sums := map[string]string{}
	kernelEntry, err := artifacts.FileEntry(kernel, sums)
	require.NoError(t, err)
	vmmEntry, err := artifacts.FileEntry(binary, sums)
	require.NoError(t, err)
	qualification := rootfsQualification{Backend: "native", KernelSHA256: kernelEntry.SHA256, FirecrackerSHA256: vmmEntry.SHA256, StorageRevision: "disposable-XFS-KVM-test", ExclusiveExt4: true}
	raw, err := json.Marshal(qualification)
	require.NoError(t, err)
	qualificationPath := filepath.Join(root, "qualification.json")
	require.NoError(t, os.WriteFile(qualificationPath, raw, 0600))
	t.Setenv("FAST_SANDBOX_ROOTFS_QUALIFICATION", qualificationPath)
	require.Contains(t, manager.SnapshotCapabilities(), fastletapi.CapabilityNativeRootfsOnlySnapshots)
	bundle := filepath.Join(root, "seed")
	require.NoError(t, os.Mkdir(bundle, 0700))
	require.NoError(t, copyReflinkOrCopy(seed, filepath.Join(bundle, publishedRootfsName)))
	disk, err := artifacts.FileEntry(filepath.Join(bundle, publishedRootfsName), sums)
	require.NoError(t, err)
	m := artifacts.NewVMManifest(artifacts.VMMachine{VCPUCount: 1, MemoryMiB: 256, RootfsSizeMiB: (disk.SizeBytes + (1 << 20) - 1) >> 20, RootfsDriveID: "rootfs", RootfsDrivePath: "rootfs.img"}, "6.1.177", binary, artifacts.CPUTemplateNone)
	m.Platform = &artifacts.VMPlatform{SnapshotMode: "rootfsOnly"}
	m.Kernel.SHA256 = kernelEntry.SHA256
	m.Kernel.Cmdline = "console=ttyS0 root=/dev/vda rw init=/init net.ifnames=0 ip=172.30.0.3::172.30.0.1:255.255.255.0::eth0:off"
	m.GuestControl = &artifacts.VMGuestControl{Protocol: guestcontrol.Protocol, Transport: "vsock", Port: 1234, HelperVersion: os.Getenv("FSB_TEST_HELPER_VERSION")}
	require.NotEmpty(t, m.GuestControl.HelperVersion)
	m.GuestNetwork = &artifacts.VMGuestNetwork{Iface: "eth0", MAC: e2ePrepMAC, IP: e2ePrepGuestIP, Gateway: "172.30.0.1", Netmask: "255.255.255.0", MTU: fastletnetwork.DefaultMTU}
	m.Files = map[string]artifacts.ManifestFile{publishedRootfsName: disk}
	raw, err = artifacts.MarshalVMManifest(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, snapshotManifestName), raw, 0600))
	require.NoError(t, artifacts.WriteSHA256SUMS(bundle, []string{publishedRootfsName}, sums))
	ref := "native-seed"
	_, err = pull.PublishImage(ctx, "", ref, bundle)
	require.NoError(t, err)
	if os.Getenv("FSB_TEST_S3_CONFIG") != "" {
		badCredential := credential
		badCredential.Password = "intentionally-invalid-test-secret"
		bad, err := agentpull.NewClient(storeRoot, badCredential, agentpull.WithEndpoint(endpoint), agentpull.WithRegion(region))
		require.NoError(t, err)
		err = bad.PullImage(ctx, filepath.Join(root, "bad-credentials"), ref)
		require.ErrorContains(t, err, "403", "the real service must reject an invalid read signature")
		readOnly := credential
		readOnly.WriteUsername, readOnly.WritePassword = "", ""
		reader, err := agentpull.NewClient(storeRoot, readOnly, agentpull.WithEndpoint(endpoint), agentpull.WithRegion(region))
		require.NoError(t, err)
		_, err = reader.PublishImage(ctx, "", "read-only-must-fail", bundle)
		require.ErrorIs(t, err, agentpull.ErrNotWritable)
		// Supplying the reader key as a write key must also fail at the
		// service, independently of the client's no-write-credential gate.
		readOnly.WriteUsername, readOnly.WritePassword = readOnly.Username, readOnly.Password
		deniedWriter, err := agentpull.NewClient(storeRoot, readOnly, agentpull.WithEndpoint(endpoint), agentpull.WithRegion(region))
		require.NoError(t, err)
		_, err = deniedWriter.PublishImage(ctx, "", "reader-key-must-fail", bundle)
		require.ErrorContains(t, err, "403", "reader credentials must have no PutObject permission")
		t.Log("invalid read signature and reader-key upload rejected by S3 (403); read-only client refused publication")
	}
	firstBoot := 0
	for cycle := 0; cycle < 2; cycle++ {
		// Force every boot to consume freshly downloaded artifacts, including
		// the snapshot whose prior pull already checked publication metadata.
		require.NoError(t, os.RemoveAll(filepath.Dir(cachedManifestPath(driver.config.StateRoot, ref))))
		require.NoError(t, pull.PullImage(ctx, driver.config.StateRoot, ref))
		if cycle == 0 {
			// A same-size mutation must fail digest validation and be repaired
			// from storage before the driver sees this image as bootable.
			file, err := os.OpenFile(agentpull.ImageRootfsPath(driver.config.StateRoot, ref), os.O_RDWR, 0)
			require.NoError(t, err)
			var original [1]byte
			_, err = file.ReadAt(original[:], 0)
			require.NoError(t, err)
			_, err = file.WriteAt([]byte{original[0] ^ 0xff}, 0)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			ready, err := agentpull.ImageReady(driver.config.StateRoot, ref)
			require.NoError(t, err)
			require.False(t, ready)
			require.NoError(t, pull.PullImage(ctx, driver.config.StateRoot, ref))
			ready, err = agentpull.ImageReady(driver.config.StateRoot, ref)
			require.NoError(t, err)
			require.True(t, ready)
			t.Log("same-size cache corruption detected and repaired by storage re-download")
		}
		id := fmt.Sprintf("native-%d", cycle)
		identity := fastletapi.SandboxIdentity{SandboxUID: id, Namespace: "e2e", Name: id, FastletPodUID: pod, InstanceGeneration: 1, RuntimeInstanceID: id + "-runtime", AssignmentAttempt: 1, RouteGeneration: 1}
		response, err := manager.CreateSandbox(ctx, &fastletapi.CreateSandboxRequest{RequestID: id, SpecGeneration: 1, Identity: identity, Sandbox: fastletapi.SandboxDesiredSpec{Image: ref, ResourceProfileHash: resourceProfile.Hash()}})
		require.NoError(t, err)
		require.Equal(t, fastletapi.CreateDispositionCreated, response.Disposition)
		defer driver.DeleteSandbox(context.Background(), id)
		live, err := loadState(filepath.Join(driver.config.StateRoot, sandboxStateDir, id))
		require.NoError(t, err)
		vm := driver.newClient(live.APIAddress)
		defer vm.Close()
		require.Eventually(t, func() bool { _, _, err := driver.captureGuest(ctx, live, m, vm); return err == nil }, 30*time.Second, 100*time.Millisecond)
		log, err := os.ReadFile(filepath.Join(filepath.Dir(live.APIAddress), processLogName))
		require.NoError(t, err)
		marker := regexp.MustCompile(`FSB_ROOT_BOOT (\d+)`).FindSubmatch(log)
		require.Len(t, marker, 2)
		boot, err := strconv.Atoi(string(marker[1]))
		require.NoError(t, err)
		if cycle == 0 {
			firstBoot = boot
		} else {
			require.Equal(t, firstBoot+1, boot)
		}
		snapID := fmt.Sprintf("native-snapshot-%d", cycle)
		ref = fmt.Sprintf("native-template-%d", cycle)
		snap := &fastletapi.CreateSnapshotRequest{RequestID: snapID, Identity: fastletapi.SnapshotIdentity{SnapshotUID: snapID, Namespace: "e2e", Name: snapID, Sandbox: identity}, Snapshot: fastletapi.SnapshotSpec{Mode: fastletapi.SnapshotModeRootfsOnly, Kind: fastletapi.SnapshotKindTemplate, TemplateName: ref}}
		if cycle == 0 {
			rejectUpload.Store(true)
			failed := *snap
			failed.RequestID = "native-upload-failure"
			failed.Identity.SnapshotUID = "native-upload-failure"
			failed.Identity.Name = "native-upload-failure"
			_, err := manager.CreateSnapshot(ctx, &failed)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				r, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: failed.Identity})
				return err == nil && fastletapi.SnapshotPhaseTerminal(r.Snapshot.Phase)
			}, time.Minute, 50*time.Millisecond)
			r, err := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: failed.Identity})
			require.NoError(t, err)
			require.Equal(t, fastletapi.SnapshotPhaseFailed, r.Snapshot.Phase)
			if os.Getenv("FSB_TEST_S3_CONFIG") != "" {
				require.Positive(t, rejectedSignatures.Load(), "upload rejection must originate from the real S3 service")
				t.Log("invalid upload signature rejected by S3 (403); source recovery and next capture checked")
			}
			current, err := vm.VMState(ctx)
			require.NoError(t, err)
			require.Equal(t, vmRunning, current)
			j, err := loadCaptureJournal(filepath.Join(driver.config.StateRoot, sandboxStateDir, id, "rootfs-capture"))
			require.NoError(t, err)
			require.Equal(t, captureRecovered, j.record.Phase)
			require.NoDirExists(t, j.record.Staging)
			rejectUpload.Store(false)
		}
		_, err = manager.CreateSnapshot(ctx, snap)
		require.NoError(t, err)
		var result fastletapi.SnapshotStatus
		require.Eventually(t, func() bool {
			r, e := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snap.Identity})
			if e != nil {
				return false
			}
			result = *r.Snapshot
			return fastletapi.SnapshotPhaseTerminal(result.Phase)
		}, time.Minute, 50*time.Millisecond)
		require.Equal(t, fastletapi.SnapshotPhaseSucceeded, result.Phase, result.Message)
		require.Equal(t, fastletapi.SnapshotModeRootfsOnly, result.Mode)
		require.True(t, strings.HasPrefix(result.ManifestRef, "s3://"))
		current, err := vm.VMState(ctx)
		require.NoError(t, err)
		require.Equal(t, vmRunning, current)
		journal, err := loadCaptureJournal(filepath.Join(driver.config.StateRoot, sandboxStateDir, id, "rootfs-capture"))
		require.NoError(t, err)
		require.Equal(t, 2, journal.record.Version)
		require.Equal(t, captureRecovered, journal.record.Phase)
		require.NoDirExists(t, journal.record.Staging)
		require.NoError(t, pull.PullImage(ctx, driver.config.StateRoot, ref))
		payload, err := os.ReadFile(cachedManifestPath(driver.config.StateRoot, ref))
		require.NoError(t, err)
		published, err := artifacts.DecodeVMManifest(payload)
		require.NoError(t, err)
		require.Len(t, published.Files, 1)
		if cycle == 1 {
			// Reconstruct a new coordinator after a persisted Pause. No captured
			// bytes may be published by recovery, and the next capture may proceed.
			guest, caps, err := driver.captureGuest(ctx, live, m, vm)
			require.NoError(t, err)
			directory := filepath.Join(driver.config.StateRoot, sandboxStateDir, id)
			incarnation, err := captureProcessIdentity(ctx, live.APIAddress, live.PID)
			require.NoError(t, err)
			diskPath := filepath.Join(directory, instanceRootfsName)
			f, err := os.OpenFile(diskPath, os.O_RDWR, 0)
			require.NoError(t, err)
			dev, ino, err := captureNativeIdentity(f)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			record := captureRecord{Backend: "native", FileDevice: dev, FileInode: ino, Owner: &identity, Incarnation: incarnation, Device: diskPath,
				Staging: snapshotStagingPath(driver.config.StateRoot, pod, "restart", true), Request: guestcontrol.Request{Protocol: guestcontrol.Protocol, Epoch: caps.Epoch, Generation: caps.Generation + 1, OperationID: "restart", LeaseMillis: 10000}}
			release, err := lockSandboxLifecycle(ctx, driver.config.StateRoot, id)
			require.NoError(t, err)
			journal, err := prepareRootfsJournal(directory, record)
			require.NoError(t, err)
			require.NoError(t, prepareCaptureStaging(record.Staging, identity))
			require.NoError(t, journal.Record(captureFreezing))
			_, err = guest.FreezeAndWait(ctx, record.Request)
			require.NoError(t, err)
			require.NoError(t, journal.Record(capturePausing))
			require.NoError(t, vm.Pause(ctx))
			release()
			restarted := &Driver{config: driver.config, podUID: pod, newClient: NewClient}
			require.NoError(t, os.Rename(diskPath, diskPath+".held"))
			require.NoError(t, os.WriteFile(diskPath, []byte("replacement-inode"), 0600))
			require.Error(t, restarted.ReconcileSandbox(ctx, identity), "a replaced disk must never authorize Resume")
			paused, err := vm.VMState(ctx)
			require.NoError(t, err)
			require.Equal(t, vmPaused, paused)
			require.NoError(t, os.Remove(diskPath))
			require.NoError(t, os.Rename(diskPath+".held", diskPath))
			require.NoError(t, restarted.ReconcileSandbox(ctx, identity))
			journal, err = loadCaptureJournal(filepath.Join(directory, "rootfs-capture"))
			require.NoError(t, err)
			require.Equal(t, captureRecovered, journal.record.Phase)
			require.NoDirExists(t, record.Staging)
		}
		t.Logf("cycle=%d boot_counter=%d native RootfsOnly published and pulled; source recovered", cycle, boot)
		if cycle == 0 && os.Getenv("FSB_TEST_NATIVE_FULL_LAYOUT") == "1" {
			verifyNativeFullLayout(t, ctx, driver, network, live)
		}
	}
}

// Fault injection keeps real storage and signature verification in the path.
// Only PUT signatures are corrupted; all requests still reach the S3 service.
type nativeS3FaultTransport struct {
	reject    *atomic.Bool
	forbidden *atomic.Int64
}

func (f nativeS3FaultTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if f.reject.Load() && request.Method == http.MethodPut {
		request = request.Clone(request.Context())
		authorization := request.Header.Get("Authorization")
		if prefix, _, ok := strings.Cut(authorization, "Signature="); ok {
			request.Header.Set("Authorization", prefix+"Signature="+strings.Repeat("0", 64))
		}
	}
	response, err := http.DefaultTransport.RoundTrip(request)
	if err == nil && response.StatusCode == http.StatusForbidden && request.Method == http.MethodPut {
		f.forbidden.Add(1)
	}
	return response, err
}

// Exercise the actual Full dump/restore storage contract independently of CPU
// manifest admission. ARM64 cannot currently publish an unmasked Full manifest;
// this test must not invent CPU identity or weaken that production check.
func verifyNativeFullLayout(t *testing.T, ctx context.Context, driver *Driver, network *fastletnetwork.Manager, source *SandboxState) {
	t.Helper()
	id := source.Config.Identity.SandboxUID
	plan := dumpPlan{identity: &source.Config.Identity, stateRoot: driver.config.StateRoot, sandboxDir: filepath.Join(driver.config.StateRoot, sandboxStateDir, id), sandboxID: id,
		snapshotID: "native-full-layout", binaryName: filepath.Base(driver.config.BinaryPath), jailed: driver.config.JailerPath != "", bootTimeout: 30,
		staging: filepath.Join(driver.config.StateRoot, "full-layout-dump")}
	defer os.RemoveAll(plan.staging)
	size, err := driver.dumpFootprintBytes(plan)
	require.NoError(t, err, "native cold boot must satisfy the Full/checkpoint capacity gate")
	require.Positive(t, size)
	require.NoError(t, driver.dumpRunningSandbox(ctx, &plan))
	cache := filepath.Dir(cachedManifestPath(driver.config.StateRoot, "full-layout"))
	require.NoError(t, os.MkdirAll(cache, 0700))
	defer os.RemoveAll(cache)
	for _, entry := range []struct{ published, cached string }{{publishedRootfsName, rootfsImageName}, {vmstateSnapshotName, vmstateSnapshotName}, {memorySnapshotName, memorySnapshotName}} {
		require.NoError(t, copyReflinkOrCopy(filepath.Join(plan.staging, entry.published), filepath.Join(cache, entry.cached)))
	}
	for _, sourcePresent := range []bool{true, false} {
		if !sourcePresent {
			require.NoError(t, driver.DeleteSandbox(ctx, id))
			require.NoDirExists(t, plan.sandboxDir)
		}
		t.Run(fmt.Sprintf("full-restore/source-present=%t", sourcePresent), func(t *testing.T) {
			cloneID := fmt.Sprintf("full-clone-%t", sourcePresent)
			owner := fastletnetwork.Owner{SandboxUID: cloneID, InstanceGeneration: 1, AssignmentAttempt: 1, RuntimeInstanceID: cloneID}
			require.NoError(t, network.Replenish(ctx))
			slot, err := network.Acquire(ctx, owner)
			require.NoError(t, err)
			defer func() { require.NoError(t, network.Release(context.Background(), owner)) }()
			directory := filepath.Join(driver.config.StateRoot, sandboxStateDir, cloneID)
			require.NoError(t, os.MkdirAll(directory, 0700))
			defer os.RemoveAll(directory)
			vmstate, memory := filepath.Join(cache, vmstateSnapshotName), filepath.Join(cache, memorySnapshotName)
			disk, jail, socket, err := driver.prepareInstance(driver.config.StateRoot, cloneID, "full-layout", directory, vmstate, memory)
			require.NoError(t, err)
			defer driver.removeJailRoot(cloneID)
			require.Equal(t, rootfsImageName, filepath.Base(disk))
			require.NoFileExists(t, filepath.Join(filepath.Dir(disk), rootfsDeviceName), "restore must not rely on an OCI path alias")
			log := filepath.Join(filepath.Dir(socket), processLogName)
			process, err := driver.launchVM(ctx, launchConfig{BinaryPath: driver.config.BinaryPath, SandboxID: cloneID, APIAddress: socket, WorkingDir: directory, LogPath: log}, slot)
			require.NoError(t, err)
			defer func() { _ = process.Kill(); _ = process.Wait() }()
			require.NoError(t, driver.waitSocket(ctx, socket, firecrackerSocketWaitTimeout))
			if jail != "" {
				require.NoError(t, driver.alignJailKVM(hostKVMDevicePath, jailedKVMDevicePath(jail)))
			}
			client := NewClient(socket)
			defer client.Close()
			require.NoError(t, configureRestoreVM(ctx, client, slot, vmstate, memory, jail != ""))
			_, err = resumeVM(ctx, client, 30)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(log)
				return err == nil && strings.Contains(string(data), "P0_WRITE_DONE")
			}, 15*time.Second, 50*time.Millisecond, "restored guest must resume its writer")
			version, err := guestcontrol.ProbeRootFilesystem(ctx, filepath.Join(filepath.Dir(socket), "guest-control.sock"), 1234)
			require.NoError(t, err, "restored rootfs and helper must support freeze/thaw")
			require.Equal(t, os.Getenv("FSB_TEST_HELPER_VERSION"), version)
			t.Logf("Full dump restored with source_present=%t; writer and root freeze/thaw passed", sourcePresent)
		})
		if t.Failed() {
			return
		}
	}
}
