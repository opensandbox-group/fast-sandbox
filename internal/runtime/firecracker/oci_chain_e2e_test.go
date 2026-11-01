//go:build firecracker

package firecracker

// Real pause/checkpoint chain E2E for OCI-rooted sandboxes: repeatedly dump,
// publish a checkpoint (fsb-vm pack + push) and restore from the published
// manifestRef, asserting every published rootfs manifest stays within the
// pack squash bound. Run scripts/firecracker-oci-chain-e2e.sh to wire up the
// host (Linux, root, KVM, TUN, registry, overlaybd-ublkd).
//
// Environment: FC_OCI_TEMPLATE_REF (required), FC_CHAIN_CYCLES (default
// 100), FC_CHAIN_MAX_LAYERS (default 24, must match the fsb-vm pack
// default), FAST_SANDBOX_OCI_SNAPSHOT_REPOSITORY, FAST_SANDBOX_OCI_PLAIN_HTTP,
// FAST_SANDBOX_REGISTRY_CONFIG_PATH.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	orasreg "oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

func TestFirecrackerOCICheckpointChainE2E(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root: netns, tap, and iptables setup")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("requires /dev/kvm")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("requires /dev/net/tun")
	}
	templateRef := envOr("FC_OCI_TEMPLATE_REF", "")
	if templateRef == "" {
		t.Skip("requires FC_OCI_TEMPLATE_REF (oci:// template pair published by the E2E script)")
	}
	// The script exports this too; set it here so the test cannot silently
	// exercise a disabled OCI path.
	t.Setenv("FAST_SANDBOX_OCI_ENABLED", "true")
	cycles := 100
	if value := os.Getenv("FC_CHAIN_CYCLES"); value != "" {
		parsed, err := strconv.Atoi(value)
		require.NoError(t, err)
		require.Greater(t, parsed, 1)
		cycles = parsed
	}
	// Must match the fsb-vm pack --max-rootfs-layers default; every cycle
	// appends a layer, so the candidate chain reaches the cap and the pack
	// squash must keep every published manifest at or below it.
	bound := 24
	if value := os.Getenv("FC_CHAIN_MAX_LAYERS"); value != "" {
		parsed, err := strconv.Atoi(value)
		require.NoError(t, err)
		require.Greater(t, parsed, 2)
		bound = parsed
	}

	binary := envOr("FC_BINARY", "")
	kernel := envOr("FC_KERNEL", "")
	jailer := envOr("FC_JAILER", "")
	require.NotEmpty(t, binary, "FC_BINARY")
	require.NotEmpty(t, kernel, "FC_KERNEL")
	require.FileExists(t, binary)
	require.FileExists(t, kernel)
	if jailer != "" {
		require.FileExists(t, jailer)
	}
	if rootfs := envOr("FC_ROOTFS", ""); rootfs != "" {
		require.FileExists(t, rootfs, "FC_ROOTFS")
	}

	ctx := context.Background()
	stateRoot := os.Getenv("FC_STATE_ROOT")
	if stateRoot == "" {
		stateRoot = t.TempDir()
	} else {
		require.NoError(t, os.MkdirAll(stateRoot, 0o750))
	}
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off net.ifnames=0 biosdevname=0 random.trust_cpu=on"
	podUID := "oci-chain-pod-" + strconv.FormatInt(time.Now().UnixNano(), 16)

	netStateRoot := t.TempDir()
	var slotCounter atomic.Int64
	purgeStaleFSBResources(t)
	manager, err := fastletnetwork.NewManager(fastletnetwork.Config{
		Capacity: 1, PodUID: podUID,
		StateRoot: netStateRoot, NetNSRoot: "/run/netns", HostNetNSRoot: "/run/fast-sandbox/netns",
		IDGenerator:      func() (string, error) { return fmt.Sprintf("e2e-slot-%d", slotCounter.Add(1)), nil },
		Now:              time.Now,
		ReplenishTimeout: time.Minute,
	}, fastletnetwork.NewGuestVMNetNSDriver(fastletnetwork.LinuxDriverConfig{}),
		newMemoryStateStore())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll("/run/netns", 0o755))
	require.NoError(t, os.MkdirAll("/run/fast-sandbox/netns", 0o755))
	require.NoError(t, manager.Initialize(ctx))

	profile := runtimecatalog.RuntimeProfile{
		Name: apiv1alpha2.RuntimeFirecracker, ProfileHash: "e2e-profile",
		Firecracker: &runtimecatalog.FirecrackerConfig{
			BinaryPath: binary, KernelPath: kernel, StateRoot: stateRoot,
			JailerPath:   jailer,
			DefaultVCPUs: 1, DefaultMemory: "512Mi", BootTimeoutSeconds: 120,
			BootArgs: bootArgs,
		},
		Capabilities: runtimecatalog.Capabilities{DefaultState: runtimecatalog.CapabilityReady},
	}
	driver, err := New(profile)
	require.NoError(t, err)
	driver.SetNetworkManager(manager)
	driver.SetFastletPodUID(podUID)
	require.NoError(t, driver.Initialize(ctx, ""))
	t.Cleanup(func() {
		_ = driver.DeleteSandbox(context.Background(), ociChainSandboxID)
		_ = driver.Close()
	})

	manifestRef, artifactDigest := templateRef, ""
	maxObserved, squashes, prevLayers := 0, 0, 0
	var totalRestore, totalCheckpoint, totalDelete time.Duration
	chainStart := time.Now()
	for cycle := 1; cycle <= cycles; cycle++ {
		spec := ociChainSpec(podUID)
		if cycle == 1 {
			spec.Desired.Image = templateRef
		} else {
			spec.Desired.Restore = &fastletapi.RestoreSpec{ManifestRef: manifestRef, ArtifactDigest: artifactDigest}
		}
		cycleCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		restoreStart := time.Now()
		metadata, err := driver.EnsureSandbox(cycleCtx, ensureInput(spec))
		restoreDur := time.Since(restoreStart)
		require.NoErrorf(t, err, "cycle %d: restore", cycle)
		require.Positive(t, metadata.PID, "cycle %d: VM must be running", cycle)

		snapshotID := fmt.Sprintf("chain-%06d", cycle)
		// CreateSnapshot covers the pause window plus offline pack + push;
		// the driver's klog line breaks the pause window down further.
		checkpointStart := time.Now()
		result, err := driver.CreateSnapshot(cycleCtx, &runtimecontract.SnapshotInput{
			SandboxID:  spec.Identity.SandboxUID,
			SnapshotID: snapshotID,
			Kind:       fastletapi.SnapshotKindCheckpoint,
		})
		checkpointDur := time.Since(checkpointStart)
		require.NoErrorf(t, err, "cycle %d: checkpoint", cycle)

		layers := fetchPublishedRootfsLayers(t, templateRef, snapshotID)
		deleteStart := time.Now()
		require.NoError(t, driver.DeleteSandbox(context.Background(), spec.Identity.SandboxUID))
		waitForCleanSlot(t, manager, 1)
		deleteDur := time.Since(deleteStart)
		totalRestore, totalCheckpoint, totalDelete = totalRestore+restoreDur, totalCheckpoint+checkpointDur, totalDelete+deleteDur
		t.Logf("CHAIN cycle=%d layers=%d restore=%s checkpoint=%s delete=%s manifestRef=%s",
			cycle, layers, restoreDur.Round(time.Millisecond), checkpointDur.Round(time.Millisecond),
			deleteDur.Round(time.Millisecond), result.ManifestRef)
		require.LessOrEqualf(t, layers, bound, "cycle %d exceeds the layer bound", cycle)
		// Every cycle writes one layer, so a published count below the
		// previous cycle's proves the pack squash merged the chain.
		if prevLayers > 0 && layers < prevLayers {
			squashes++
		}
		prevLayers = layers
		maxObserved = max(maxObserved, layers)
		cancel()
		manifestRef, artifactDigest = result.ManifestRef, result.ArtifactDigest
	}
	wall := time.Since(chainStart)
	t.Logf("CHAIN summary cycles=%d maxLayers=%d wall=%s avgCycle=%s avgRestore=%s avgCheckpoint=%s avgDelete=%s",
		cycles, maxObserved, wall.Round(time.Second),
		(wall / time.Duration(cycles)).Round(time.Millisecond),
		(totalRestore / time.Duration(cycles)).Round(time.Millisecond),
		(totalCheckpoint / time.Duration(cycles)).Round(time.Millisecond),
		(totalDelete / time.Duration(cycles)).Round(time.Millisecond))
	// A working squash collapses the candidate chain at the cap before
	// publication, so the observed maximum stays below the bound; require a
	// squash to have happened instead of an exact published maximum.
	require.Greaterf(t, squashes, 0, "the cap must trigger a squash within %d cycles", cycles)
}

const ociChainSandboxID = "oci-chain-sandbox"

func ociChainSpec(podUID string) *fastletapi.RuntimeSandboxConfig {
	return &fastletapi.RuntimeSandboxConfig{
		Desired: fastletapi.SandboxDesiredSpec{
			RuntimeProfileHash: "e2e-profile", ResourceProfileHash: "e2e-resource",
		},
		Limits: fastletapi.SandboxRuntimeLimits{CPU: "1", Memory: "512Mi"},
		Identity: fastletapi.SandboxIdentity{SandboxUID: ociChainSandboxID, Name: ociChainSandboxID, Namespace: "e2e",
			InstanceGeneration: 1, RuntimeInstanceID: "oci-chain-ri", AssignmentAttempt: 1,
			FastletPodUID: podUID},
	}
}

// fetchPublishedRootfsLayers returns the layer count of the cycle's
// published rootfs manifest (tag <snapshotID>_rootfs).
func fetchPublishedRootfsLayers(t *testing.T, templateRef, snapshotID string) int {
	t.Helper()
	repository := envOr("FAST_SANDBOX_OCI_SNAPSHOT_REPOSITORY", "")
	if repository == "" {
		parsed, err := orasreg.ParseReference(templateRef)
		require.NoError(t, err)
		repository = parsed.Registry + "/" + parsed.Repository
	}
	rootfsRef, err := orasreg.ParseReference(repository + ":" + snapshotID + "_rootfs")
	require.NoError(t, err)
	repo, err := remote.NewRepository(rootfsRef.Registry + "/" + rootfsRef.Repository)
	require.NoError(t, err)
	repo.PlainHTTP = ociPlainHTTP()
	descriptor, err := repo.Manifests().Resolve(context.Background(), rootfsRef.Reference)
	require.NoErrorf(t, err, "resolve rootfs manifest %s", rootfsRef.Reference)
	manifestReader, err := repo.Manifests().Fetch(context.Background(), descriptor)
	defer manifestReader.Close()
	var manifest struct {
		Layers []json.RawMessage `json:"layers"`
	}
	require.NoError(t, json.NewDecoder(manifestReader).Decode(&manifest))
	return len(manifest.Layers)
}
