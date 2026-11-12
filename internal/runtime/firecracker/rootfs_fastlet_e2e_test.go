//go:build linux && firecracker

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registrytest "github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	fastletsandbox "fast-sandbox/internal/fastlet/sandbox"
	"fast-sandbox/internal/guestcontrol"
	"fast-sandbox/internal/ocitemplate"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
)

// Consume a verified real rootfs upper; publish only to this test's in-memory Registry.
// Requires a dedicated Linux/KVM node with fsb-vm and OverlayBD deployed; never change node configuration automatically.
func TestRootfsFastletChainE2E(t *testing.T) {
	seed := os.Getenv("FSB_TEST_FASTLET_SEED")
	if seed == "" {
		t.Skip("requires disposable rootfs Fastlet lab")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	server := httptest.NewServer(registrytest.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	t.Setenv("FAST_SANDBOX_OCI_ENABLED", "true")
	t.Setenv("FAST_SANDBOX_OCI_PLAIN_HTTP", "true")
	t.Setenv("FAST_SANDBOX_OCI_SNAPSHOT_REPOSITORY", host+"/rootfs/chain")
	root, err := os.MkdirTemp(filepath.Dir(seed), "fc-")
	require.NoError(t, err)
	keepRoot := false
	t.Cleanup(func() {
		if !keepRoot {
			_ = os.RemoveAll(root)
		}
	})
	credentialPath := filepath.Join(root, "registry.json")
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{Host: host}})
	require.NoError(t, err)
	raw, err := json.Marshal(compiled)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(credentialPath, raw, 0600))
	t.Setenv("FAST_SANDBOX_REGISTRY_CONFIG_PATH", credentialPath)
	binary, kernel := os.Getenv("FC_BINARY"), os.Getenv("FC_KERNEL")
	require.NotEmpty(t, binary)
	require.NotEmpty(t, kernel)
	pod := "rootfs-e2e-pod"
	profile := runtimecatalog.RuntimeProfile{Name: apiv1alpha2.RuntimeFirecracker, ProfileHash: "rootfs-e2e", Firecracker: &runtimecatalog.FirecrackerConfig{BinaryPath: binary, VMMDir: os.Getenv("FC_VMM_DIR"), KernelPath: kernel, JailerPath: os.Getenv("FC_JAILER"), RootfsPath: "/unused-oci-rootfs", StateRoot: filepath.Join(root, "state"), DefaultVCPUs: 1, DefaultMemory: "256Mi", BootTimeoutSeconds: 30}, Capabilities: runtimecatalog.Capabilities{DefaultState: runtimecatalog.CapabilityReady}}
	driver, err := New(profile)
	require.NoError(t, err)
	driver.SetFastletPodUID(pod)
	var counter atomic.Int64
	network, err := fastletnetwork.NewManager(fastletnetwork.Config{Capacity: 2, PodUID: pod, StateRoot: filepath.Join(root, "network"), NetNSRoot: "/run/netns", HostNetNSRoot: "/run/netns", IDGenerator: func() (string, error) { return fmt.Sprintf("rootfs-%d-%d", os.Getpid(), counter.Add(1)), nil }, Now: time.Now, ReplenishTimeout: time.Minute}, fastletnetwork.NewGuestVMNetNSDriver(fastletnetwork.LinuxDriverConfig{}), newMemoryStateStore())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll("/run/netns", 0755))
	require.NoError(t, os.MkdirAll("/run/fast-sandbox/netns", 0755))
	require.NoError(t, network.Initialize(ctx))
	defer network.Close(context.Background())
	driver.SetNetworkManager(network)
	require.NoError(t, driver.Initialize(ctx, ""))
	defer driver.Close()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		output, e := exec.CommandContext(cleanup, "fsb-vm", append(driver.fsbArgs(), "cleanup-snapfiles")...).CombinedOutput()
		if e != nil {
			keepRoot = true
			t.Errorf("keep backing files %s: cleanup failed: %v: %s", root, e, output)
		}
	}()
	manager, err := fastletsandbox.NewSandboxManagerWithConfig(driver, fastletsandbox.SandboxManagerConfig{Capacity: 2, FastletPodUID: pod, RuntimeName: apiv1alpha2.RuntimeFirecracker, RegistryProvider: registryconfig.NewFileProvider(credentialPath)})
	require.NoError(t, err)

	bundle := filepath.Join(root, "seed")
	require.NoError(t, os.Mkdir(bundle, 0700))
	for _, name := range []string{ocitemplate.UpperDataName, ocitemplate.UpperIndexName} {
		require.NoError(t, copyFrozenUpper(filepath.Join(seed, "bundle", name), filepath.Join(bundle, name)))
	}
	upper, err := ocitemplate.ReadRootfsUpper(filepath.Join(seed, "bundle"))
	require.NoError(t, err)
	upper.Repository = host + "/rootfs/seed"
	repository, _, err := ocitemplate.Repository(upper.Repository+":seed", nil, ocitemplate.WithPlainHTTP(true))
	require.NoError(t, err)
	for _, layer := range upper.Lowers {
		payload, e := os.ReadFile(filepath.Join(seed, layer.Digest.Encoded()+".zfile"))
		require.NoError(t, e)
		require.NoError(t, repository.Blobs().Push(ctx, layer, bytes.NewReader(payload)))
	}
	raw, err = json.Marshal(upper)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, ocitemplate.RootfsUpperName), raw, 0600))
	raw, err = os.ReadFile(filepath.Join(seed, "bundle", "manifest.json"))
	require.NoError(t, err)
	manifest, err := artifacts.DecodeVMManifest(raw)
	require.NoError(t, err)
	manifest.GuestNetwork = &artifacts.VMGuestNetwork{Iface: "eth0", MAC: e2ePrepMAC, IP: e2ePrepGuestIP, Gateway: "172.30.0.1", Netmask: "255.255.255.0", MTU: fastletnetwork.DefaultMTU}
	manifest.Kernel.Cmdline = "console=ttyS0 root=/dev/vda rw init=/init net.ifnames=0 ip=172.30.0.3::172.30.0.1:255.255.255.0::eth0:off"
	raw, err = artifacts.MarshalVMManifest(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), raw, 0600))
	args := append(driver.fsbArgs(), "pack", "--bundle-dir", bundle, "--output", "seed:test", "--rootfs-only")
	output, err := exec.CommandContext(ctx, "fsb-vm", args...).CombinedOutput()
	require.NoError(t, err, string(output))
	ref, _, err := ocitemplate.Push(ctx, filepath.Join(driver.ociRoot(), "artifacts", "seed_test"), upper.Repository+":seed", raw, nil, ocitemplate.WithPlainHTTP(true))
	require.NoError(t, err)

	initialBoot := 0
	for cycle := 0; cycle < 2; cycle++ {
		id := fmt.Sprintf("rootfs-chain-%d", cycle)
		identity := fastletapi.SandboxIdentity{SandboxUID: id, Namespace: "e2e", Name: id, FastletPodUID: pod, InstanceGeneration: 1, RuntimeInstanceID: id + "-runtime", AssignmentAttempt: 1, RouteGeneration: 1}
		req := &fastletapi.CreateSandboxRequest{RequestID: id, SpecGeneration: 1, Identity: identity, Sandbox: fastletapi.SandboxDesiredSpec{Image: ref}}
		response, e := manager.CreateSandbox(ctx, req)
		require.NoError(t, e)
		require.Equal(t, fastletapi.CreateDispositionCreated, response.Disposition)
		defer driver.DeleteSandbox(context.Background(), id)
		state, e := loadState(filepath.Join(driver.config.StateRoot, sandboxStateDir, id))
		require.NoError(t, e)
		if profile.Firecracker.VMMDir != "" {
			require.NotNil(t, state.VMM)
			require.Equal(t, manifest.Compatibility.FirecrackerVersion, state.VMM.Version)
			require.Equal(t, filepath.Join(profile.Firecracker.VMMDir, "versions", state.VMM.Version, "firecracker"), state.VMM.BinaryPath)
			require.NoError(t, qualifyVMMExecutable(state, ""))
		}
		actualNS, e := os.Stat(fmt.Sprintf("/proc/%d/ns/net", state.PID))
		require.NoError(t, e)
		expectedNS, e := os.Stat(state.Allocation.Network.NamespacePath)
		require.NoError(t, e)
		require.True(t, os.SameFile(actualNS, expectedNS), "VMM must run in its allocated netns")
		c := driver.newClient(state.APIAddress)
		var guest guestcontrol.Client
		require.Eventually(t, func() bool { var e error; guest, _, e = driver.captureGuest(ctx, state, manifest, c); return e == nil }, 30*time.Second, 100*time.Millisecond)
		log, e := os.ReadFile(filepath.Join(filepath.Dir(state.APIAddress), processLogName))
		require.NoError(t, e)
		marker := regexp.MustCompile(`FSB_ROOT_BOOT (\d+)`).FindSubmatch(log)
		require.Len(t, marker, 2)
		boot, e := strconv.Atoi(string(marker[1]))
		require.NoError(t, e)
		if cycle == 0 {
			initialBoot = boot
		} else {
			require.Equal(t, initialBoot+1, boot)
		}
		t.Logf("guest cold-boot disk counter=%d", boot)
		ping, e := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "3", state.Allocation.Network.IP).CombinedOutput()
		require.NoError(t, e, string(ping))
		if cycle == 0 {
			snap := &fastletapi.CreateSnapshotRequest{RequestID: "rootfs-snapshot", Identity: fastletapi.SnapshotIdentity{SnapshotUID: "rootfs-snapshot", Namespace: "e2e", Name: "rootfs-snapshot", Sandbox: identity}, Snapshot: fastletapi.SnapshotSpec{Mode: fastletapi.SnapshotModeRootfsOnly, Kind: fastletapi.SnapshotKindTemplate}}
			started := time.Now()
			accepted, e := manager.CreateSnapshot(ctx, snap)
			require.NoError(t, e)
			require.Equal(t, fastletapi.CreateDispositionCreated, accepted.Disposition)
			var result fastletapi.SnapshotStatus
			require.Eventually(t, func() bool {
				value, e := manager.InspectSnapshot(&fastletapi.InspectSnapshotRequest{Identity: snap.Identity})
				if e != nil {
					return false
				}
				result = *value.Snapshot
				return fastletapi.SnapshotPhaseTerminal(result.Phase)
			}, 2*time.Minute, 100*time.Millisecond)
			if os.Getenv("FSB_TEST_REJECT_UNFIXED_VMM") == "1" {
				require.Equal(t, "1.16.1", state.VMM.Version)
				require.NotEqual(t, fastletapi.SnapshotPhaseSucceeded, result.Phase)
				require.Contains(t, result.Message, "requires Firecracker 1.17.0")
				caps, err := guest.Call(ctx, guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"})
				require.NoError(t, err)
				require.Zero(t, caps.Generation, "rejection must happen before guest freeze")
				status, err := c.VMState(ctx)
				require.NoError(t, err)
				require.Equal(t, "Running", status)
				t.Log("stock managed 1.16.1 capture rejected before freeze; source remains usable")
				return
			}
			require.Equal(t, fastletapi.SnapshotPhaseSucceeded, result.Phase, result.Message)
			t.Logf("RootfsOnly create-to-published latency=%s (100ms polling, local Registry)", time.Since(started))
			ref = result.ManifestRef
			require.NotEmpty(t, ref)
			caps, e := guest.Call(ctx, guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"})
			require.NoError(t, e)
			require.NotEmpty(t, caps.Epoch)
			status, e := c.VMState(ctx)
			require.NoError(t, e)
			require.Equal(t, "Running", status)
			journal, e := loadCaptureJournal(filepath.Join(driver.config.StateRoot, sandboxStateDir, id, "rootfs-capture"))
			require.NoError(t, e)
			require.Equal(t, captureRecovered, journal.record.Phase)
			t.Logf("Fastlet snapshot published %s; source resumed and transaction recovered", ref)
		}
		if cycle == 1 {
			// Simulate loss of coordinator memory after Pause, then recover from the journal with a new Driver; this is not a SIGKILL test.
			caps, e := guest.Call(ctx, guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"})
			require.NoError(t, e)
			incarnation, e := captureProcessIdentity(ctx, state.APIAddress, state.PID)
			require.NoError(t, e)
			request := guestcontrol.Request{Protocol: guestcontrol.Protocol, Epoch: caps.Epoch, Generation: caps.Generation + 1, OperationID: "restart-recovery", LeaseMillis: 10000}
			directory := filepath.Join(driver.config.StateRoot, sandboxStateDir, id)
			release, e := lockSandboxLifecycle(ctx, driver.config.StateRoot, id)
			require.NoError(t, e)
			journal, e := newCaptureJournal(filepath.Join(directory, "rootfs-capture"), captureRecord{Owner: &identity, Incarnation: incarnation, Device: filepath.Join(filepath.Dir(state.APIAddress), "rootfs.dev"), Staging: snapshotStagingPath(driver.config.StateRoot, pod, request.OperationID, true), Request: request})
			release()
			require.NoError(t, e)
			require.NoError(t, journal.Record(captureFreezing))
			_, e = guest.FreezeAndWait(ctx, request)
			require.NoError(t, e)
			defer func() { _ = driver.recoverRootfsCapture(context.Background(), state) }()
			require.NoError(t, journal.Record(capturePausing))
			require.NoError(t, c.Pause(ctx))
			restarted, e := New(profile)
			require.NoError(t, e)
			restarted.SetFastletPodUID(pod)
			restarted.SetNetworkManager(network)
			require.NoError(t, restarted.RecoverRuntimeResources(ctx, nil, nil))
			journal, e = loadCaptureJournal(filepath.Join(directory, "rootfs-capture"))
			require.NoError(t, e)
			require.Equal(t, captureRecovered, journal.record.Phase)
			status, e := c.VMState(ctx)
			require.NoError(t, e)
			require.Equal(t, "Running", status)
			before, e := os.Stat(filepath.Join(filepath.Dir(state.APIAddress), processLogName))
			require.NoError(t, e)
			require.Eventually(t, func() bool {
				after, e := os.Stat(filepath.Join(filepath.Dir(state.APIAddress), processLogName))
				return e == nil && after.Size() > before.Size()
			}, 5*time.Second, 50*time.Millisecond)
			t.Log("fresh Driver recovered persisted paused capture; guest fsync writer continued")
		}
		c.Close()
	}
	t.Log("Fastlet admission -> real Driver -> rootfs capture -> local OCI publication -> Fastlet cold boot passed")
}
