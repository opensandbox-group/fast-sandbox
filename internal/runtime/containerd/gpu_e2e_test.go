package containerd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/fastlet/gpu"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const cudaProbe = `
import subprocess, sys, time
import torch

assert torch.cuda.is_available()
assert torch.cuda.device_count() == 1
if sys.argv[1] == "check-uuid":
    devices = subprocess.check_output(["nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader"], text=True).split()
    assert len(devices) == 1, devices
    print("FSB_GPU_UUID=" + devices[0], flush=True)
x = torch.ones((32, 32), device="cuda")
y = x @ x
torch.cuda.synchronize()
assert y.sum().item() == 32768
print("FSB_GPU_CUDA_OK", torch.cuda.get_device_name(0), flush=True)
time.sleep(600)
`

// The two-device case requires native GPU isolation and verifies it inside the
// containers. The single-device probe also supports WSL2.
func TestGPUContainerdCUDA(t *testing.T) {
	devices := os.Getenv("FSB_GPU_E2E_DEVICES")
	if devices == "" {
		t.Skip("set FSB_GPU_E2E_DEVICES to one or two explicitly assigned GPU UUIDs")
	}
	ids := strings.Split(devices, ",")
	require.LessOrEqual(t, len(ids), 2)
	require.Equal(t, "linux", runtime.GOOS, "real containerd GPU validation requires Linux or WSL2")
	require.Zero(t, os.Geteuid(), "run on an authorized GPU test host as root")
	image := os.Getenv("FSB_GPU_E2E_IMAGE")
	require.NotEmpty(t, image, "set FSB_GPU_E2E_IMAGE to a pre-pulled image containing Python and CUDA-enabled PyTorch")
	socket := os.Getenv("FSB_GPU_E2E_SOCKET")
	if socket == "" {
		socket = "/run/containerd/containerd.sock"
	}
	snapshotter := os.Getenv("FSB_GPU_E2E_SNAPSHOTTER")
	if snapshotter == "" {
		snapshotter = "overlayfs"
	}
	const namespace = "k8s.io"
	runID := fmt.Sprintf("fsb-gpu-e2e-%d", time.Now().UnixNano())
	t.Setenv("POD_UID", "")
	t.Setenv("POD_NAME", "")
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	driver := newWithConfig(apiv1alpha2.RuntimeContainer, "gpu-e2e-profile", RuntimeConfig{Handler: "io.containerd.runc.v2", Namespace: namespace, Snapshotter: snapshotter})
	require.NoError(t, driver.Initialize(ctx, socket))
	t.Cleanup(func() { require.NoError(t, driver.Close()) })
	driver.fastletPodUID = runID
	driver.SetNamespace("gpu-e2e")
	root := t.TempDir()
	newManager := func() *gpu.Manager {
		manager, err := gpu.New(root, ids, 1, runID, namespace, socket)
		require.NoError(t, err)
		if err := driver.setGPUManager(manager); err != nil {
			_ = manager.Close()
			t.Fatal(err)
		}
		return manager
	}
	manager := newManager()

	// An image record copied to another namespace does not grant access to
	// its content on containerd 1.7. Keep the cache and test objects together.
	imageCtx := driver.withNamespace(ctx)
	source, err := driver.client.GetImage(imageCtx, image)
	require.NoError(t, err, "pre-pull FSB_GPU_E2E_IMAGE into the k8s.io containerd namespace")
	unpacked, err := source.IsUnpacked(imageCtx, snapshotter)
	require.NoError(t, err)
	require.True(t, unpacked, "wait until containerd finishes unpacking the test image")
	var cleanupIDs []string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cleanupCancel()
		for _, uid := range cleanupIDs {
			if err := driver.DeleteSandbox(cleanupCtx, uid); err != nil {
				t.Errorf("cleanup %s: %v", uid, err)
			}
			_ = os.Remove(filepath.Join("/var/log/fast-sandbox", uid+".log"))
		}
	})
	probeMode := "cuda-only"
	if len(ids) == 2 {
		probeMode = "check-uuid"
	}
	inputFor := func(suffix string) *fastletapi.EnsureSandboxInput {
		uid := runID + "-" + suffix
		cleanupIDs = append(cleanupIDs, uid)
		return &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{
			Identity: fastletapi.SandboxIdentity{SandboxUID: uid, Name: uid, Namespace: "gpu-e2e", FastletPodUID: runID, RuntimeInstanceID: uid + "-instance", InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1},
			Desired:  fastletapi.SandboxDesiredSpec{Image: image, RuntimeProfileHash: "gpu-e2e-profile", ResourceProfileHash: "gpu-e2e-resource", Command: []string{"python", "-c", cudaProbe, probeMode}},
			Limits:   fastletapi.SandboxRuntimeLimits{CPU: "1", Memory: "4Gi", PIDs: 512},
		}}
	}
	awaitCUDA := func(input *fastletapi.EnsureSandboxInput, metadata *SandboxMetadata) string {
		t.Helper()
		require.NotNil(t, metadata.Allocation.GPU)
		require.Len(t, metadata.Allocation.GPU.Devices, 1)
		device := strings.TrimPrefix(metadata.Allocation.GPU.Devices[0], "nvidia.com/gpu=")
		require.Contains(t, ids, device)
		logPath := filepath.Join("/var/log/fast-sandbox", input.Sandbox.Identity.SandboxUID+".log")
		var log string
		err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 90*time.Second, true, func(context.Context) (bool, error) {
			raw, _ := os.ReadFile(logPath)
			log = string(raw)
			return strings.Contains(log, "FSB_GPU_CUDA_OK"), nil
		})
		require.NoError(t, err, "CUDA output from %s:\n%s", logPath, log)
		if len(ids) == 2 {
			require.Contains(t, strings.Split(log, "\n"), "FSB_GPU_UUID="+device)
		}
		t.Logf("%s: %s", input.Sandbox.Identity.SandboxUID, strings.TrimSpace(log))
		return device
	}

	inputs := []*fastletapi.EnsureSandboxInput{inputFor("a")}
	if len(ids) == 2 {
		inputs = append(inputs, inputFor("b"))
	}
	var running []*SandboxMetadata
	for _, input := range inputs {
		metadata, err := driver.EnsureSandbox(ctx, input)
		require.NoError(t, err)
		running = append(running, metadata)
	}
	var assigned []string
	for i, input := range inputs {
		assigned = append(assigned, awaitCUDA(input, running[i]))
	}
	require.ElementsMatch(t, ids, assigned)
	replacement := inputFor("replacement")
	_, err = driver.EnsureSandbox(ctx, replacement)
	require.ErrorIs(t, err, gpu.ErrExhausted)

	// Reopen the journal with both tasks still running.
	require.NoError(t, manager.Close())
	driver.gpuManager = nil
	manager = newManager()
	managed, err := driver.ListManagedSandboxes(ctx)
	require.NoError(t, err)
	require.Len(t, managed, len(inputs))
	require.NoError(t, driver.RecoverRuntimeResources(ctx, managed, nil))
	for i, input := range inputs {
		recovered, err := driver.EnsureSandbox(ctx, input)
		require.NoError(t, err)
		require.Equal(t, running[i].PID, recovered.PID)
		require.Equal(t, running[i].Allocation.GPU, recovered.Allocation.GPU)
	}

	owner := inputs[0].Sandbox.Identity
	if len(ids) == 2 {
		container, err := driver.client.LoadContainer(imageCtx, owner.SandboxUID)
		require.NoError(t, err)
		task, err := container.Task(imageCtx, nil)
		require.NoError(t, err)
		require.NoError(t, task.Kill(imageCtx, syscall.SIGKILL))
		require.Eventually(t, func() bool { return driver.gpuTaskExited(imageCtx, owner.SandboxUID) }, 20*time.Second, 250*time.Millisecond)
		require.NoError(t, driver.RecordTerminalFailure(ctx, owner, "test GPU task exit"))
		_, err = driver.EnsureSandbox(ctx, replacement)
		require.ErrorIs(t, err, gpu.ErrExhausted, "a failed task must keep its GPU until cleanup is authorized")
		require.NoError(t, driver.CleanupTerminalFailure(ctx, owner))
		failed, err := driver.InspectSandbox(ctx, owner.SandboxUID)
		require.NoError(t, err)
		require.Equal(t, gpuFailedPhase, failed.Phase)
	} else {
		require.NoError(t, driver.DeleteSandbox(ctx, owner.SandboxUID))
	}
	_, found, err := manager.Lookup(owner.SandboxUID)
	require.NoError(t, err)
	require.False(t, found)
	reused, err := driver.EnsureSandbox(ctx, replacement)
	require.NoError(t, err)
	require.Equal(t, assigned[0], awaitCUDA(replacement, reused))
	if len(ids) == 2 {
		peer, err := driver.EnsureSandbox(ctx, inputs[1])
		require.NoError(t, err)
		require.Equal(t, running[1].PID, peer.PID)
		require.Equal(t, running[1].Allocation.GPU, peer.Allocation.GPU)
	}
	for _, uid := range cleanupIDs {
		require.NoError(t, driver.DeleteSandbox(ctx, uid))
	}
	for _, uid := range cleanupIDs {
		_, err := driver.client.LoadContainer(imageCtx, uid)
		require.True(t, errdefs.IsNotFound(err), "container %s still exists or cannot be verified: %v", uid, err)
		_, found, err := manager.Lookup(uid)
		require.NoError(t, err)
		require.False(t, found)
	}
}
