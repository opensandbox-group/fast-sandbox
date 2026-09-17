//go:build firecracker

package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestFirecrackerDriverE2E boots a real Firecracker microVM through the
// driver on the local machine with a real Infra plan (GuestCopy delivery).
// It requires root (netns, tap, iptables), /dev/kvm, /dev/net/tun, a
// firecracker binary, a kernel image, and a converted rootfs.
// See scripts/firecracker-e2e.sh.
func TestFirecrackerDriverE2E(t *testing.T) {
	runE2EOnce(t, true)
}

// TestFirecrackerDriverE2ENoInfra exercises the same lifecycle without Infra
// delivery: no SetInfraManager, no infra.json, no components in the guest.
func TestFirecrackerDriverE2ENoInfra(t *testing.T) {
	runE2EOnce(t, false)
}

// TestFirecrackerDriverE2EConcurrent pre-provisions 5 network slots and
// restores 5 microVMs in parallel through the same driver and slot manager,
// verifying per-sandbox trace correlation, distinct processes, the
// shared-snapshot isolation contract (memory.snap COW), and the NoInfra
// restore path under concurrency (no GuestCopy delivery: the chain E2E
// bakes execd into the snapshot, so execd readiness is probed per instance).
// v1.16 clone networking: every clone runs in its own slot netns (jailer
// --netns) with the snapshot's baked guest MAC/IP, so per-instance
// reachability is asserted on every slot (ARP is namespace-isolated).
func TestFirecrackerDriverE2EConcurrent(t *testing.T) {
	const vmCount = 5
	env := newE2EEnvironment(t, vmCount, false)
	defer env.teardown()
	runCloneBatch(t, env, vmCount, false)
}

// TestFirecrackerDriverE2EConcurrentSerial is the sequential baseline of
// the clone batch. Production Sandbox creates arrive one by one, so serial
// is the default path; comparing the two modes exposes the per-stage
// bottleneck differences (slot acquire serialization, launch overlap).
func TestFirecrackerDriverE2EConcurrentSerial(t *testing.T) {
	const vmCount = 5
	env := newE2EEnvironment(t, vmCount, false)
	defer env.teardown()
	runCloneBatch(t, env, vmCount, true)
}

// runCloneBatch creates vmCount sandboxes from the same snapshot set in
// parallel (clone burst) or sequentially (production default), asserts
// per-instance reachability and execd readiness on every slot, and prints
// the per-stage bottleneck summary read from the durable sandbox state.
func runCloneBatch(t *testing.T, env *e2eEnvironment, vmCount int, sequential bool) {
	t.Helper()
	snapshot := env.manager.Snapshot()
	require.Equal(t, vmCount, snapshot.Clean, "all slots must be pre-provisioned before boot")
	require.Zero(t, snapshot.Bound)

	type bootResult struct {
		metadata *SandboxMetadata
		err      error
		traceID  trace.TraceID
	}
	results := make([]bootResult, vmCount)
	started := time.Now()
	if sequential {
		for index := 0; index < vmCount; index++ {
			spec := env.spec(index + 1)
			ctx, end := env.traceContext(spec.Identity.SandboxUID)
			results[index].traceID = trace.SpanContextFromContext(ctx).TraceID()
			results[index].metadata, results[index].err = env.driver.EnsureSandbox(ctx, ensureInput(spec))
			end()
		}
	} else {
		var wg sync.WaitGroup
		for index := 0; index < vmCount; index++ {
			spec := env.spec(index + 1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, end := env.traceContext(spec.Identity.SandboxUID)
				defer end()
				result := &results[index]
				result.traceID = trace.SpanContextFromContext(ctx).TraceID()
				result.metadata, result.err = env.driver.EnsureSandbox(ctx, ensureInput(spec))
			}()
		}
		wg.Wait()
	}
	mode := "parallel"
	if sequential {
		mode = "serial"
	}
	wall := time.Since(started)
	t.Logf("load-mode=%s vmCount=%d wall=%s", mode, vmCount, wall)

	snapshot = env.manager.Snapshot()
	require.Zero(t, snapshot.Clean, "all pre-provisioned slots must be bound")
	require.Equal(t, vmCount, snapshot.Bound)

	pids := make(map[int]string, vmCount)
	for index := 0; index < vmCount; index++ {
		spec := env.spec(index + 1)
		result := results[index]
		require.NoErrorf(t, result.err, "sandbox %s", spec.Identity.SandboxUID)
		env.assertBooted(result.metadata, spec, result.traceID)
		if owner, exists := pids[result.metadata.PID]; exists {
			t.Fatalf("VMs must be distinct processes: pid %d used by both %s and %s", result.metadata.PID, owner, spec.Identity.SandboxUID)
		}
		pids[result.metadata.PID] = spec.Identity.SandboxUID
		// NoInfra: no GuestCopy delivery, so no infra services are reported.
		require.Empty(t, result.metadata.InfraServices)
		require.Empty(t, result.metadata.InfraDiagnostics)
		// Per-clone netns isolation: every instance shares the baked guest
		// MAC/IP but each slot netns NATs its own slot IP to the guest, so
		// every sandbox is independently reachable (issue #26).
		env.assertGuestReachable(spec.Identity.SandboxUID)
		env.assertSlotDataPlane(spec.Identity.SandboxUID)
		// Full-chain evidence (chain E2E bakes execd into the snapshot):
		// every clone's execd answers through its own slot DNAT, proving
		// per-instance business readiness survived the restore. A guest-side
		// restore flake (execd listener not surviving resume) is retried
		// with one sandbox recreate, mirroring production scheduling.
		if os.Getenv("FC_EXECD_PROBE") == "1" {
			env.assertGuestExecdWithRecreate(spec)
		}
	}
	env.logLoadStageSummary(t, mode, vmCount)
}

// logLoadStageSummary prints the per-stage min/avg/max durations across the
// batch from the durable per-Sandbox state (recorded by the driver), so the
// serial vs parallel bottleneck differences are visible in one table.
func (env *e2eEnvironment) logLoadStageSummary(t *testing.T, mode string, vmCount int) {
	t.Helper()
	stages := []string{"acquire", "rootfs", "infra", "launch", "configure", "boot"}
	type stageStats struct {
		min   time.Duration
		max   time.Duration
		sum   time.Duration
		count int
	}
	rows := make(map[string]*stageStats, len(stages))
	for _, stage := range stages {
		rows[stage] = &stageStats{}
	}
	for index := 1; index <= vmCount; index++ {
		directory, err := sandboxDir(env.stateRoot, env.spec(index).Identity.SandboxUID)
		if err != nil {
			continue
		}
		state, err := loadState(directory)
		if err != nil {
			continue
		}
		for _, stage := range stages {
			duration := state.StageDurations[stage]
			row := rows[stage]
			row.count++
			row.sum += duration
			if row.count == 1 || duration < row.min {
				row.min = duration
			}
			if duration > row.max {
				row.max = duration
			}
		}
	}
	t.Logf("load-mode=%s stage breakdown (min/avg/max):", mode)
	for _, stage := range stages {
		row := rows[stage]
		if row.count == 0 {
			continue
		}
		t.Logf("  %-10s %s / %s / %s",
			stage,
			row.min.Round(time.Microsecond),
			(row.sum / time.Duration(row.count)).Round(time.Microsecond),
			row.max.Round(time.Microsecond))
	}
}

// TestFirecrackerDriverE2ELeak repeatedly creates and deletes a sandbox and
// asserts no host resource accumulates across rounds:
//
//   - per-sandbox resources release immediately on delete (jail dir, VMM
//     process);
//   - the slot pool never exceeds capacity (netns/veth count is bounded by
//     Replenish, not growing);
//   - draining the manager (Close) leaves zero per-slot resources.
//
// Round count: FC_LEAK_ROUNDS (default 10); a few hundred rounds make a
// soak run (create+delete ≈ 1 s per round).
func TestFirecrackerDriverE2ELeak(t *testing.T) {
	rounds := 10
	if value := os.Getenv("FC_LEAK_ROUNDS"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			rounds = parsed
		}
	}
	capacity := 1
	env := newE2EEnvironment(t, capacity, false)
	defer env.teardown()

	baseline := hostLeakSnapshot(t, env.stateRoot)
	for round := 1; round <= rounds; round++ {
		// Deleting the sandbox releases the slot; Replenish replaces it
		// asynchronously (netns + rules take ~15 ms), so wait for the clean
		// slot before the next create (Acquire does not block on an empty
		// pool).
		waitForCleanSlot(t, env.manager, capacity)
		spec := env.spec(1)
		sandboxUID := spec.Identity.SandboxUID
		ctx, end := env.traceContext(sandboxUID)
		metadata, err := env.driver.EnsureSandbox(ctx, ensureInput(spec))
		end()
		require.NoErrorf(t, err, "round %d: create", round)
		env.assertBooted(metadata, spec, trace.SpanContextFromContext(ctx).TraceID())
		env.assertGuestReachable(sandboxUID)
		if os.Getenv("FC_EXECD_PROBE") == "1" {
			env.assertGuestExecd(sandboxUID)
		}
		env.delete(sandboxUID)

		now := hostLeakSnapshot(t, env.stateRoot)
		// Per-sandbox resources must be gone immediately after delete.
		require.Equalf(t, baseline["jailDirs"], now["jailDirs"], "round %d: jail dirs leaked", round)
		require.Equalf(t, baseline["fcProcs"], now["fcProcs"], "round %d: VMM processes leaked", round)
		// The slot pool stays at capacity (Replenish replaces, never grows).
		require.LessOrEqualf(t, now["netns"], capacity, "round %d: netns pool overflowed", round)
		require.LessOrEqualf(t, now["bridgeDevs"], capacity, "round %d: bridge device pool overflowed", round)
	}
	t.Logf("leak soak: %d create/delete rounds, no accumulation (netns=%d bridgeDevs=%d jailDirs=%d fcProcs=%d)",
		rounds, baseline["netns"], baseline["bridgeDevs"], baseline["jailDirs"], baseline["fcProcs"])

	// Draining the manager leaves zero per-slot resources on the host.
	require.NoError(t, env.manager.Close(context.Background()))
	after := hostLeakSnapshot(t, env.stateRoot)
	require.Zerof(t, after["netns"], "netns left after Manager.Close")
	require.Zerof(t, after["bridgeDevs"], "bridge devices left after Manager.Close")
	require.Zerof(t, after["jailDirs"], "jail dirs left after Manager.Close")
	require.Zerof(t, after["fcProcs"], "VMM processes left after Manager.Close")
}

// waitForCleanSlot polls until the manager has a clean slot (Replenish
// replaces released slots asynchronously). Fails the test on timeout.
func waitForCleanSlot(t *testing.T, manager *fastletnetwork.Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if manager.Snapshot().Clean >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Failf(t, "no clean network slot within 5s", "Clean=%d want>=%d", manager.Snapshot().Clean, want)
}

// hostLeakSnapshot counts the per-run host resources of the E2E: fsb* netns,
// fsb0-attached fh* veths, jail roots, and live e2e firecracker processes.
func hostLeakSnapshot(t *testing.T, stateRoot string) map[string]int {
	t.Helper()
	snapshot := map[string]int{"netns": 0, "bridgeDevs": 0, "jailDirs": 0, "fcProcs": 0}
	if out, err := exec.Command("ip", "netns", "list").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "fsb") {
				snapshot["netns"]++
			}
		}
	}
	if out, err := exec.Command("ip", "-o", "link", "show", "master", "fsb0").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if strings.HasPrefix(strings.TrimSuffix(fields[1], ":"), "fh") {
				snapshot["bridgeDevs"]++
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(stateRoot, jailerChrootBaseDir, "firecracker")); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				snapshot["jailDirs"]++
			}
		}
	}
	if out, err := exec.Command("pgrep", "-f", "firecracker .*--id e2e-").CombinedOutput(); err == nil {
		snapshot["fcProcs"] = strings.Count(string(out), "\n")
	}
	return snapshot
}

// TestFirecrackerDriverE2EImageGC verifies the independent LFU image cache
// GC on a real StateRoot: unreferenced low-frequency images are evicted when
// the cache is over its limit, a running Sandbox pins its image, and deleting
// the Sandbox releases it for eviction.
func TestFirecrackerDriverE2EImageGC(t *testing.T) {
	env := newE2EEnvironment(t, 1, false)
	defer env.teardown()

	waitGone := func(t *testing.T, path string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		require.Failf(t, "image was not collected: %s", path)
	}
	waitPresent := func(t *testing.T, path string) {
		t.Helper()
		time.Sleep(500 * time.Millisecond)
		_, err := os.Stat(path)
		require.NoError(t, err, "image must be pinned: %s", path)
	}
	seedImage := func(t *testing.T, image string) string {
		t.Helper()
		path, err := imageCachePath(env.stateRoot, image)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte("unused rootfs"), 0o640))
		return path
	}

	// The cache limit only fits the real e2e:ubuntu image: any unreferenced
	// extra image is over the limit and gets evicted by the independent loop.
	usedPath, err := imageCachePath(env.stateRoot, env.imageRef)
	require.NoError(t, err)
	info, err := os.Stat(usedPath)
	require.NoError(t, err)
	env.driver.imageCacheLimitBytes = info.Size() + 1
	unusedPath := seedImage(t, "e2e:unused")
	env.driver.TriggerImageGC()
	waitGone(t, unusedPath)

	// A live Sandbox pins its cached image even when the cache is over the
	// limit; booting also records a use for it.
	spec := env.spec(1)
	ctx, end := env.traceContext(spec.Identity.SandboxUID)
	defer end()
	metadata := env.boot(ctx, spec)
	env.assertGuestReachable(spec.Identity.SandboxUID)
	env.driver.TriggerImageGC()
	waitPresent(t, usedPath)

	// Deleting the Sandbox releases the reference; with the cache over its
	// limit the now-unreferenced image is evicted on the next collection.
	env.delete(spec.Identity.SandboxUID)
	env.driver.imageCacheLimitBytes = 1
	env.driver.TriggerImageGC()
	waitGone(t, usedPath)
	require.Equal(t, string(PhaseRunning), metadata.Phase)
}

// runE2EOnce drives one full create/inspect/delete lifecycle.
func runE2EOnce(t *testing.T, useInfra bool) {
	env := newE2EEnvironment(t, 1, useInfra)
	defer env.teardown()
	t.Logf("infra=%t", useInfra)

	spec := env.spec(1)
	ctx, end := env.traceContext(spec.Identity.SandboxUID)
	defer end()

	started := time.Now()
	metadata := env.boot(ctx, spec)
	t.Logf("VM running in %s (pid=%d)", time.Since(started), metadata.PID)

	if useInfra {
		env.assertInfra(metadata, spec.Identity.SandboxUID)
	} else {
		require.Empty(t, metadata.InfraServices)
		require.Empty(t, metadata.InfraDiagnostics)
	}

	env.assertGuestReachable(spec.Identity.SandboxUID)
	env.assertSlotDataPlane(spec.Identity.SandboxUID)
	// The chain E2E builder bakes execd into the golden snapshot when
	// CHAIN_EXECD is set; probing its /ping endpoint proves the template's
	// runtime bootstrap survived the restore (not just ICMP reachability).
	if os.Getenv("FC_EXECD_PROBE") == "1" {
		env.assertGuestExecd(spec.Identity.SandboxUID)
	}

	// Inspect reports the running VM.
	inspected, err := env.driver.InspectSandbox(ctx, spec.Identity.SandboxUID)
	require.NoError(t, err)
	require.Equal(t, string(PhaseRunning), inspected.Phase)

	// Delete stops the VM, releases the slot, and removes the state.
	env.delete(spec.Identity.SandboxUID)
}

// e2ePrepVersion marks the golden snapshot set format. It must be bumped
// whenever the preparation recipe changes (NIC baking, drive path, boot
// args, machine tuple): a cached set from an older recipe is incompatible
// with the current restore driver (e.g. it lacks the baked NIC that
// network_overrides expects), so the reuse check must reject it.
const e2ePrepVersion = 2

// bootVM starts the microVM and waits until the machine state is Running.
// It serves the golden-snapshot prep path only (cold boot: InstanceStart);
// the production driver is restore-only and resumes via resumeVM.
func bootVM(ctx context.Context, client *Client, timeoutSeconds int32) (int, error) {
	if err := client.Start(ctx); err != nil {
		return 0, fmt.Errorf("start Firecracker instance: %w", err)
	}
	return waitVMRunning(ctx, client, timeoutSeconds)
}

// prepareE2EGoldenSnapshot produces (or reuses) the golden snapshot set of
// the E2E image: a
// preparation VM cold-boots the kernel once with a NIC and a static guest
// IP, pauses, and dumps a Full snapshot; the golden set is assembled into
// the driver cache layout:
//
//	<StateRoot>/images/<sha256(image)>/{rootfs.img, vmstate.snap, memory.snap, manifest.json}
//
// v1.16 restore semantics: the vmstate bakes the root drive path, the NIC
// (iface id, guest MAC, virtio state) and the guest network configuration.
// The prep drive is attached with the RELATIVE path "rootfs.img" and the
// prep Firecracker runs with cwd = the cache image dir, so instances
// (started with cwd = their state dir) resolve the same baked path to
// their own reflink copy. The guest IP baked by the prep boot args must
// match the first slot's guest address for reachability assertions.
//
// External-artifact mode (FC_SKIP_PREP=1): the chain E2E
// (scripts/firecracker-chain-e2e.sh) pulls the golden set through the real
// runtime-agent from the builder's published output, so the prep would
// overwrite the very artifacts under test. The cache layout is validated
// instead of bootstrapped.
func prepareE2EGoldenSnapshot(t *testing.T, binary, kernel, rootfs, stateRoot, imageRef, bootArgs string) {
	t.Helper()
	dir := filepath.Join(stateRoot, imageCacheDir, imageKey(imageRef))
	rootfsImg := filepath.Join(dir, rootfsImageName)
	vmstate := filepath.Join(dir, vmstateSnapshotName)
	memory := filepath.Join(dir, memorySnapshotName)
	manifestPath := filepath.Join(dir, "manifest.json")
	versionMarker := filepath.Join(dir, ".prep-version")
	if os.Getenv("FC_SKIP_PREP") == "1" {
		// The golden set must be complete (the agent's pull commits the
		// manifest last) and carry the machine tuple restore validates.
		for _, path := range []string{rootfsImg, vmstate, memory, manifestPath} {
			require.FileExists(t, path, "external golden set is incomplete (FC_SKIP_PREP)")
		}
		require.FileExists(t, kernel, "FC_KERNEL")
		require.FileExists(t, rootfs, "FC_ROOTFS")
		_, ok, err := readCachedManifestMachine(stateRoot, imageRef)
		require.NoError(t, err)
		require.True(t, ok, "external manifest carries no machine tuple (machine=1/512Mi required for the E2E spec)")
		t.Logf("reusing externally pulled golden snapshot set %s (FC_SKIP_PREP)", dir)
		return
	}
	complete := func() bool {
		for _, path := range []string{rootfsImg, vmstate, memory, manifestPath} {
			if info, err := os.Stat(path); err != nil || info.IsDir() {
				return false
			}
		}
		// Reject snapshot sets prepared by an older recipe: the restore
		// driver depends on the baked NIC and the relative drive path.
		payload, err := os.ReadFile(versionMarker)
		return err == nil && strings.TrimSpace(string(payload)) == fmt.Sprint(e2ePrepVersion)
	}
	if complete() {
		t.Logf("reusing prepared golden snapshot set %s", dir)
		return
	}
	if _, err := os.Stat(vmstate); err == nil {
		t.Logf("discarding incompatible cached golden snapshot set %s (prep version mismatch)", dir)
		require.NoError(t, os.RemoveAll(dir))
		require.NoError(t, os.MkdirAll(dir, 0o750))
	}

	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, copyFile(rootfs, rootfsImg))
	require.NoError(t, os.WriteFile(versionMarker, []byte(fmt.Sprint(e2ePrepVersion)), 0o640))

	// The prep VM needs a host tap so the baked NIC has a backing device.
	prepTap := "fc-prep-tap"
	if output, err := exec.Command("ip", "tuntap", "add", "dev", prepTap, "mode", "tap").CombinedOutput(); err != nil {
		require.Failf(t, "create prep tap %s: %v\n%s", prepTap, err.Error(), output)
	}
	defer func() {
		_, _ = exec.Command("ip", "link", "del", prepTap).CombinedOutput()
	}()

	prepDir := t.TempDir()
	apiSock := filepath.Join(prepDir, "api.sock")
	logPath := filepath.Join(prepDir, "firecracker.log")
	process, err := launch(context.Background(), ExecProcessRunner{}, launchConfig{
		BinaryPath: binary, SandboxID: "e2e-prep", APIAddress: apiSock,
		// cwd = cache image dir so the relative "rootfs.img" drive path
		// baked in the vmstate resolves to the golden rootfs here and to
		// each instance's reflink copy on restore.
		WorkingDir: dir, LogPath: logPath,
	})
	require.NoError(t, err)
	defer process.Kill()

	client := NewClient(apiSock)
	defer client.Close()
	require.NoError(t, waitForAPISocket(context.Background(), apiSock, firecrackerSocketWaitTimeout))

	// The preparation machine must match the manifest machine tuple that
	// restore validates (vmstate pins mem_size_mib): 1 vCPU / 512 MiB, the
	// same as the e2e Sandbox spec.
	require.NoError(t, client.ConfigureMachine(context.Background(), MachineConfigRequest{VCPUs: 1, MemSizeMiB: 512}))
	// The static guest network is baked into the snapshot via the boot
	// args: the restored guest resumes with eth0 = 172.30.0.3 (the first
	// slot's guest address), which is the v1.16 clone networking model.
	require.NoError(t, client.ConfigureBootSource(context.Background(), BootSourceRequest{
		KernelImagePath: kernel, BootArgs: e2ePrepBootArgs(bootArgs),
	}))
	require.NoError(t, client.AttachDrive(context.Background(), DriveRequest{
		// Relative path baked in the vmstate; instances resolve it via
		// their process cwd. Booted read-write exactly like a restored
		// instance so the snapshot's filesystem state matches the golden
		// rootfs the instances reflink-copy from.
		DriveID: "root", PathOnHost: "rootfs.img", IsRootDevice: true, IsReadOnly: false,
	}))
	require.NoError(t, client.AttachNetworkInterface(context.Background(), NetworkInterfaceRequest{
		// The NIC (iface id + MAC + state) is baked in the vmstate; each
		// restored instance overrides only the host tap name.
		IfaceID: "eth0", HostDevName: prepTap, GuestMAC: e2ePrepMAC,
	}))
	// bootVM performs the InstanceStart and the Running poll; the VM must
	// not be started before it (a second InstanceStart is rejected).
	_, err = bootVM(context.Background(), client, 90)
	require.NoError(t, err)
	require.NoError(t, client.Pause(context.Background()))
	require.NoError(t, waitVMState(context.Background(), client, "Paused", 30*time.Second))

	require.NoError(t, client.CreateSnapshot(context.Background(), SnapshotCreateRequest{
		SnapshotType: "Full", SnapshotPath: vmstate, MemFilePath: memory,
	}))
	require.NoError(t, process.Kill())

	// The manifest records the machine tuple restore validates and the
	// artifact digests of the golden set (commit point: manifest last).
	rootfsDigest, err := fileSHA256(rootfsImg)
	require.NoError(t, err)
	vmstateDigest, err := fileSHA256(vmstate)
	require.NoError(t, err)
	memoryDigest, err := fileSHA256(memory)
	require.NoError(t, err)
	manifest, err := json.Marshal(map[string]any{
		"machine": map[string]any{"vcpu": "1", "memory": "512Mi"},
		// The baked guest network (clone model): every restored instance
		// owns the same static address; per-instance identity is the slot
		// IP, translated to this address by the netns DNAT.
		"guestNetwork": map[string]any{
			"iface": "eth0", "mac": e2ePrepMAC, "ip": e2ePrepGuestIP,
			"gateway": "172.30.0.1", "netmask": "255.255.255.0",
		},
		"files": map[string]any{
			"rootfs.ext4":  map[string]any{"sha256": rootfsDigest, "sizeBytes": fileSize(t, rootfsImg)},
			"vmstate.snap": map[string]any{"sha256": vmstateDigest, "sizeBytes": fileSize(t, vmstate)},
			"memory.snap":  map[string]any{"sha256": memoryDigest, "sizeBytes": fileSize(t, memory)},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, manifest, 0o640))
	t.Logf("prepared golden snapshot set %s (rootfs=%s)", dir, filepath.Base(rootfs))
}

// e2ePrepMAC is the guest MAC baked into the golden snapshot NIC. Every
// restored instance resumes with it (v1.16 restores the NIC config); the
// per-clone netns data plane keeps the shared MAC/IP in namespace-isolated
// ARP domains, so concurrent clones coexist without collisions.
const e2ePrepMAC = "02:00:00:00:00:01"

// e2ePrepGuestIP is the static guest address baked into the snapshot (via
// the prep boot args and recorded in the manifest guestNetwork). Every
// restored instance owns it; the netns DNAT translates each slot IP to it.
const e2ePrepGuestIP = "172.30.0.3"

// e2ePrepBootArgs appends the static guest network of the preparation VM
// (baked into the snapshot): eth0 = 172.30.0.3 (the baked guest address),
// gateway 172.30.0.1, /24.
func e2ePrepBootArgs(base string) string {
	if !strings.Contains(base, " ip=") {
		return base + " ip=" + e2ePrepGuestIP + "::172.30.0.1:255.255.255.0::eth0:off"
	}
	return base
}

// guestIPLabel returns the baked guest address the slot netns translates to
// (applied from the manifest), falling back to the per-slot derivation.
func guestIPLabel(slot *fastletnetwork.Slot) string {
	if slot.GuestIP != "" {
		return slot.GuestIP
	}
	if ip, err := fastletnetwork.GuestVMIP(slot); err == nil {
		return ip
	}
	return "?"
}

// waitVMState polls the Firecracker machine state until it matches want.
func waitVMState(ctx context.Context, client *Client, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		state, err := client.VMState(ctx)
		if err != nil {
			return err
		}
		if state == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Firecracker VM did not reach %s within %s (state %q)", want, timeout, state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bootPollInterval):
		}
	}
}

func fileSHA256(path string) (string, error) {
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, input); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

// e2eEnvironment bundles the host checks, StateRoot, slot manager, driver,
// and per-sandbox helpers shared by the E2E tests.
type e2eEnvironment struct {
	t            *testing.T
	manager      *fastletnetwork.Manager
	driver       *Driver
	infraMgr     *fastletinfra.Manager // nil when Infra delivery is disabled
	imageRef     string
	stateRoot    string
	podUID       string
	infraEnabled bool
	jailer       bool
	created      []string
}

// purgeStaleFSBResources removes leftover per-run netns and bridge devices
// from earlier tests in this process (and earlier runs). A netns whose
// deletion failed (EBUSY racing a dying VMM) stays on the shared bridge
// still owning its slot IP: it answers ARP and pings for that address and
// shadows the live netns, so the probes never reach the new VM (all
// iptables counters stay 0). Tests run sequentially, so nothing live is
// touched.
func purgeStaleFSBResources(t *testing.T) {
	t.Helper()
	out, err := exec.Command("ip", "netns", "list").CombinedOutput()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.Fields(line)
		if len(name) == 0 || !strings.HasPrefix(name[0], "fsb") {
			continue
		}
		_, _ = exec.Command("ip", "netns", "del", name[0]).CombinedOutput()
		t.Logf("purged stale netns %s", name[0])
	}
	out, err = exec.Command("ip", "-o", "link", "show", "master", "fsb0").CombinedOutput()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		device := strings.TrimSuffix(fields[1], ":")
		if !strings.HasPrefix(device, "fh") && !strings.HasPrefix(device, "vmtap") {
			continue
		}
		_, _ = exec.Command("ip", "link", "del", device).CombinedOutput()
		t.Logf("purged stale bridge device %s", device)
	}
}

// newE2EEnvironment validates the host, prepares the golden snapshot set,
// pre-provisions capacity network slots, and wires the driver (with a real
// Infra plan when infraEnabled).
func newE2EEnvironment(t *testing.T, capacity int, infraEnabled bool) *e2eEnvironment {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root: netns, tap, and iptables setup")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("requires /dev/kvm")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("requires /dev/net/tun")
	}

	binary := envOr("FC_BINARY", "")
	kernel := envOr("FC_KERNEL", "")
	rootfs := envOr("FC_ROOTFS", "")
	imageRef := envOr("FC_IMAGE_REF", "e2e:ubuntu")
	// The jailer is the carrier of the per-clone netns (jailer --netns) and
	// the chroot; when absent the driver keeps the direct launch mode.
	jailer := envOr("FC_JAILER", "")
	for name, path := range map[string]string{"FC_BINARY": binary, "FC_KERNEL": kernel, "FC_ROOTFS": rootfs} {
		require.FileExists(t, path, name)
	}
	if jailer != "" {
		require.FileExists(t, jailer, "FC_JAILER")
	}

	ctx := context.Background()
	stateRoot := os.Getenv("FC_STATE_ROOT")
	if stateRoot == "" {
		stateRoot = t.TempDir()
	} else {
		require.NoError(t, os.MkdirAll(stateRoot, 0o750))
	}
	// random.trust_cpu=on: the microVM has no entropy source (no virtio-rng,
	// RDRAND masked for snapshot determinism), so without it the guest CRNG
	// never initializes and getrandom() callers (Go crypto/rand, uuid, etc.)
	// block forever — execd POST /command hangs on its session id while /ping
	// and malformed-JSON 400 stay instant (OpenSandbox #1695). Keep in sync
	// with the builder's prep boot args in cmd/sandboxtemplate-builder.
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off net.ifnames=0 biosdevname=0 random.trust_cpu=on"
	// The golden snapshot set is the runtime asset: a preparation VM boots
	// the kernel once, pauses, and dumps vmstate + memory; subsequent
	// Sandboxes restore from it (no kernel at runtime). A complete set is
	// reused, so a persistent FC_STATE_ROOT skips the prep boot on reruns.
	prepareE2EGoldenSnapshot(t, binary, kernel, rootfs, stateRoot, imageRef, bootArgs)

	// Unique Pod UID per run keeps the resourceName-derived netns/bridge names
	// from colliding with leftovers of earlier runs.
	podUID := "e2e-pod-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	// In-memory slot store: the manager's async Replenish keeps writing
	// durable files, which would race with t.TempDir cleanup.
	netStateRoot, err := os.MkdirTemp("", "fc-e2e-netstate")
	require.NoError(t, err)
	t.Cleanup(func() {
		for attempt := 0; attempt < 20; attempt++ {
			if removeErr := os.RemoveAll(netStateRoot); removeErr == nil || os.IsNotExist(removeErr) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = os.RemoveAll(netStateRoot)
	})
	var slotCounter atomic.Int64
	// A stale netns from an earlier test (EBUSY deletion race) would shadow
	// the slot IPs on the shared bridge; purge before preparing new slots.
	purgeStaleFSBResources(t)
	manager, err := fastletnetwork.NewManager(fastletnetwork.Config{
		Capacity: capacity, PodUID: podUID,
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
			BinaryPath: binary, KernelPath: kernel, RootfsPath: rootfs, StateRoot: stateRoot,
			JailerPath:   jailer,
			DefaultVCPUs: 1, DefaultMemory: "512Mi", BootTimeoutSeconds: 90,
			BootArgs: bootArgs,
		},
		Capabilities: runtimecatalog.Capabilities{DefaultState: runtimecatalog.CapabilityReady},
	}
	driver, err := New(profile)
	require.NoError(t, err)
	driver.SetNetworkManager(manager)
	// When the chain E2E runs a real runtime-agent (FC_AGENT_SOCKET), wire
	// the driver to it: DeleteSandbox then releases/unpins through the UDS
	// API instead of staying in local mode.
	if agentSocket := os.Getenv("FC_AGENT_SOCKET"); agentSocket != "" {
		driver.SetFastletPodUID(podUID)
		driver.SetAgentSocket(agentSocket)
	}

	env := &e2eEnvironment{
		t: t, manager: manager, driver: driver, imageRef: imageRef,
		stateRoot: stateRoot, podUID: podUID, infraEnabled: infraEnabled,
		jailer: jailer != "",
	}
	if infraEnabled {
		// Real Infra plan: EnsureSandbox performs a genuine GuestCopy delivery
		// (loop mount + copy) of sandbox-init and an "execd" component into the
		// instance rootfs, verified later with debugfs.
		sandboxInit := filepath.Join(t.TempDir(), "sandbox-init")
		require.NoError(t, os.WriteFile(sandboxInit, []byte("#!/bin/sh\n"), 0o755))
		env.infraMgr = newE2EInfraManager(t, profile, sandboxInit)
		driver.SetInfraManager(env.infraMgr)
		require.NoError(t, driver.Initialize(ctx, ""))
	}
	require.NoError(t, driver.Initialize(ctx, ""))

	// Local sampled provider so spans carry real trace IDs without an OTLP
	// endpoint; the root trace ID is derived from the sandbox id.
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	t.Logf("restoring Firecracker microVMs from golden snapshot set (kernel=%s)", kernel)
	for index := 1; index <= capacity; index++ {
		env.created = append(env.created, fmt.Sprintf("e2e-sandbox-%d", index))
	}
	return env
}

// spec builds the SandboxSpec for the i-th sandbox ("e2e-sandbox-<i>").
func (env *e2eEnvironment) spec(index int) *fastletapi.RuntimeSandboxConfig {
	sandboxID := fmt.Sprintf("e2e-sandbox-%d", index)
	spec := &fastletapi.RuntimeSandboxConfig{
		Spec: fastletapi.SandboxSpec{
			Image: env.imageRef, CPU: "1", Memory: "512Mi",
			RuntimeProfileHash: "e2e-profile", ResourceProfileHash: "e2e-resource",
		},
		Identity: fastletapi.SandboxIdentity{SandboxUID: sandboxID, Name: sandboxID, Namespace: "e2e",
			InstanceGeneration: 1, RuntimeInstanceID: fmt.Sprintf("e2e-ri-%d", index), AssignmentAttempt: 1,
			FastletPodUID: env.podUID},
	}
	if env.infraEnabled {
		spec.Spec.InfraRevision = env.infraMgr.Revision()
	}
	return spec
}

// traceContext returns a context carrying a sampled root span whose trace ID
// is derived from the sandbox id, exercising the identity correlation used by
// the production Fastlet.
func (env *e2eEnvironment) traceContext(sandboxID string) (context.Context, func()) {
	digest := sha256.Sum256([]byte(sandboxID))
	rootContext := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID(digest[:16]),
		SpanID:     trace.SpanID(digest[16:24]),
		TraceFlags: trace.FlagsSampled,
	}))
	ctx, span := observability.Start(rootContext, "e2e.firecracker.create",
		attribute.String("sandbox_uid", sandboxID),
		attribute.String("image", env.imageRef),
	)
	return ctx, func() { observability.End(span, nil) }
}

// boot creates one sandbox through the driver and validates the result.
// It must be called from the test goroutine (it uses require).
func (env *e2eEnvironment) boot(ctx context.Context, spec *fastletapi.RuntimeSandboxConfig) *SandboxMetadata {
	env.t.Helper()
	metadata, err := env.driver.EnsureSandbox(ctx, ensureInput(spec))
	require.NoError(env.t, err)
	env.assertBooted(metadata, spec, trace.SpanContextFromContext(ctx).TraceID())
	return metadata
}

// assertBooted validates the metadata, trace correlation, and process
// identity of one successful boot.
func (env *e2eEnvironment) assertBooted(metadata *SandboxMetadata, spec *fastletapi.RuntimeSandboxConfig, traceID trace.TraceID) {
	t := env.t
	t.Helper()
	digest := sha256.Sum256([]byte(spec.Identity.SandboxUID))
	require.True(t, traceID.IsValid(), "trace context must carry a valid trace ID")
	require.Equal(t, trace.TraceID(digest[:16]), traceID, "trace ID must be derived from the sandbox id")
	t.Logf("traceId=%s sandboxId=%s", traceID.String(), spec.Identity.SandboxUID)
	require.Equal(t, string(PhaseRunning), metadata.Phase)
	require.Positive(t, metadata.PID)
	// The launched process must match the NodeJanitor residual-process
	// matcher: binary "firecracker" and --id equal to the truncated sandbox id.
	cmdline := readProcCmdline(t, metadata.PID)
	require.Equal(t, "firecracker", filepath.Base(cmdline[0]))
	require.Equal(t, spec.Identity.SandboxUID, cmdline[indexOf(cmdline, "--id")+1])
}

// assertInfra verifies the delivered Infra plan: services for route
// publication and the artifacts inside the instance rootfs image (checked
// with debugfs against the image file).
func (env *e2eEnvironment) assertInfra(metadata *SandboxMetadata, sandboxID string) {
	t := env.t
	t.Helper()
	require.True(t, env.infraEnabled, "infra assertions require an infra environment")
	require.Len(t, metadata.InfraServices, 1)
	require.Equal(t, "execd", metadata.InfraServices[0].Component)
	require.Equal(t, uint32(44772), metadata.InfraServices[0].Port)
	instanceRootfs := env.instanceRootfs(sandboxID)
	assertGuestFile(t, instanceRootfs, "/.fast/run/infra.json", sandboxID)
	assertGuestFile(t, instanceRootfs, "/.fast/components/execd/execd", "fake-execd")
	assertGuestFile(t, instanceRootfs, "/.fast/bin/sandbox-init", "#!/bin/sh")
}

// instanceRootfs returns the host path of the per-instance rootfs copy (the
// jail root copy in jailer mode, the state directory copy in direct mode).
func (env *e2eEnvironment) instanceRootfs(sandboxID string) string {
	if env.jailer {
		return filepath.Join(env.stateRoot, jailerChrootBaseDir, filepath.Base(env.driver.config.BinaryPath),
			truncatedSandboxID(sandboxID), jailerChrootRootDir, instanceRootfsName)
	}
	return filepath.Join(env.stateRoot, sandboxStateDir, sandboxID, instanceRootfsName)
}

// assertGuestReachable verifies the guest VM answers ICMP through the slot
// netns data plane. The host pings the slot IP: the in-namespace DNAT
// rewrites it to the baked guest address, so the probe travels the real
// host -> veth -> slot netns DNAT -> tap -> guest path and back (reply
// SNATed to the slot IP). This is the per-instance ingress contract that
// fastlet-proxy dials (slot IP).
func (env *e2eEnvironment) assertGuestReachable(sandboxID string) {
	t := env.t
	t.Helper()
	if _, err := exec.LookPath("ping"); err != nil {
		t.Logf("ping not installed; skipping reachability check")
		return
	}
	slot, ok := env.manager.Lookup(sandboxID)
	require.True(t, ok)
	// Earlier tests in this process reused the same private addresses and
	// left the host neighbour cache pointing at their (now dead) VMs and
	// stale bridge ports; a frame to a stale MAC is flooded to a gone tap and
	// dropped. Flush the bridge neighbours so ARP is re-resolved against the
	// live slot.
	_, _ = exec.Command("ip", "neigh", "flush", "dev", slot.Bridge).CombinedOutput()
	output, err := exec.Command("ping", "-c", "1", "-W", "3", slot.IP).CombinedOutput()
	if err != nil {
		t.Logf("ping failed; dumping guest network diagnostics:\n%s", dumpNetworkDiagnostics(t, slot, env.stateRoot, env.jailer))
		require.NoErrorf(t, err, "guest %s (slot %s) not reachable: %s", guestIPLabel(slot), slot.IP, output)
	}
	t.Logf("guest reachable via slot %s (guest %s, tap %s)", slot.IP, guestIPLabel(slot), slot.GuestTap)
}

// assertGuestExecd verifies the execd baked into the golden snapshot (by
// the chain E2E builder, CHAIN_EXECD) answers its /ping endpoint inside
// the restored guest. The probe dials the SLOT IP: the in-namespace DNAT
// maps it to the baked guest address, so the request travels the real
// host -> bridge -> slot netns DNAT -> tap -> guest path and lands on the
// execd process that survived restore. The slot IP is the per-instance
// identity (the shared baked guest IP is not routable from the host nor
// distinguishable across clones).
//
// VM Running (firecracker state) does NOT imply execd readiness: the guest
// resumes, its virtio-net reconnects, and execd's listener re-readies, so
// the probe retries for a window and reports the delta between the VM
// delivery and the business-runtime readiness (the sandbox-usable SLO).
func (env *e2eEnvironment) assertGuestExecd(sandboxID string) {
	env.t.Helper()
	require.NoError(env.t, env.probeExecd(sandboxID))
}

// probeExecd returns nil when the guest's execd answers /ping through the
// slot DNAT. On failure it dumps the network diagnostics (netns nat/filter
// tables, routes, guest serial log, direct from-netns listener check).
func (env *e2eEnvironment) probeExecd(sandboxID string) error {
	t := env.t
	slot, ok := env.manager.Lookup(sandboxID)
	if !ok {
		return fmt.Errorf("slot not found")
	}
	endpoint := fmt.Sprintf("http://%s:44772/ping", slot.IP)
	client := &http.Client{Timeout: 2 * time.Second}
	probeStarted := time.Now()
	// 30s window: execd is restored from the snapshot with the VM; in rare
	// cases its listener re-readies slowly after resume (a guest-side
	// restore flake), and the delta is reported either way.
	deadline := probeStarted.Add(30 * time.Second)
	var lastErr error
	for {
		response, err := client.Get(endpoint)
		if err == nil && response.StatusCode == http.StatusOK {
			response.Body.Close()
			t.Logf("execd /ping OK at %s after %s", endpoint, time.Since(probeStarted))
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("status %d", response.StatusCode)
			response.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Logf("execd probe failed; dumping guest network diagnostics:\n%s", dumpNetworkDiagnostics(t, slot, env.stateRoot, env.jailer))
			return lastErr
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// assertGuestExecdWithRecreate probes execd readiness and, when the
// listener did not survive the snapshot restore (a guest-side restore
// flake; the data plane is verified separately by assertSlotDataPlane),
// recreates the sandbox once and re-probes — mirroring production
// restore-failure retry scheduling. The failed state is dumped before the
// recreate.
func (env *e2eEnvironment) assertGuestExecdWithRecreate(spec *fastletapi.RuntimeSandboxConfig) {
	t := env.t
	t.Helper()
	sandboxUID := spec.Identity.SandboxUID
	if err := env.probeExecd(sandboxUID); err == nil {
		return
	}
	t.Logf("execd not ready on %s; recreating the sandbox once (restore retry, mirrors production scheduling)", sandboxUID)
	env.delete(sandboxUID)
	ctx, end := env.traceContext(sandboxUID)
	defer end()
	// Release replenishes the slot pool asynchronously; the recreate retries
	// until a clean slot is available (transient acquire error) or the wait
	// budget is exhausted.
	var metadata *SandboxMetadata
	var recreateErr error
	for attempt := 0; attempt < 5; attempt++ {
		metadata, recreateErr = env.driver.EnsureSandbox(ctx, ensureInput(spec))
		if recreateErr == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	require.NoErrorf(t, recreateErr, "recreate %s after execd restore flake", sandboxUID)
	env.assertBooted(metadata, spec, trace.SpanContextFromContext(ctx).TraceID())
	env.assertGuestReachable(sandboxUID)
	env.assertSlotDataPlane(sandboxUID)
	require.NoErrorf(t, env.probeExecd(sandboxUID), "execd /ping unreachable on %s after one re-create", sandboxUID)
	t.Logf("execd ready on %s after one re-create", sandboxUID)
}

// assertSlotDataPlane asserts the per-restore guest data plane is present
// in the slot netns (ingress DNAT slot IP -> baked guest IP). A missing
// data plane makes the netns answer the slot IP locally: ping passes
// (fake) while TCP is refused with no local listener — so this check
// distinguishes a data-plane failure from a guest-side issue.
func (env *e2eEnvironment) assertSlotDataPlane(sandboxID string) {
	t := env.t
	t.Helper()
	slot, ok := env.manager.Lookup(sandboxID)
	require.True(t, ok)
	check := []string{"netns", "exec", slot.NetNSName, "iptables", "-t", "nat", "-C",
		"PREROUTING", "-d", slot.IP + "/32", "-j", "DNAT", "--to-destination", guestIPLabel(slot)}
	if _, err := exec.Command("ip", check...).CombinedOutput(); err != nil {
		t.Logf("slot data plane missing; dumping guest network diagnostics:\n%s", dumpNetworkDiagnostics(t, slot, env.stateRoot, env.jailer))
		require.NoErrorf(t, err, "slot %s data plane: DNAT %s -> %s missing", slot.ID, slot.IP, guestIPLabel(slot))
	}
}

// delete stops the VM, releases the slot, and removes the state.
func (env *e2eEnvironment) delete(sandboxID string) {
	t := env.t
	t.Helper()
	require.NoError(t, env.driver.DeleteSandbox(context.Background(), sandboxID))
	directory, err := sandboxDir(env.stateRoot, sandboxID)
	require.NoError(t, err)
	_, err = os.Stat(directory)
	require.True(t, os.IsNotExist(err))
	_, exists := env.manager.Lookup(sandboxID)
	require.False(t, exists)
}

// teardown removes any sandbox that is still around and closes the driver.
// Individual tests call delete() first; teardown is the idempotent safety net.
func (env *e2eEnvironment) teardown() {
	for _, sandboxID := range env.created {
		_ = env.driver.DeleteSandbox(context.Background(), sandboxID)
	}
	_ = env.driver.Close()
	// The manager replenishes released slots asynchronously; Close stops
	// that and destroys every remaining slot so the next environment starts
	// on a clean bridge (a leftover Clean netns would otherwise shadow the
	// slot IPs and be purged by the next environment).
	if env.manager != nil {
		_ = env.manager.Close(context.Background())
	}
}

// dumpNetworkDiagnostics collects the netns and guest state for failure logs.
func dumpNetworkDiagnostics(t *testing.T, slot *fastletnetwork.Slot, stateRoot string, jailed bool) string {
	t.Helper()
	var builder strings.Builder
	for _, args := range [][]string{
		{"addr", "show"},
		{"route", "show"},
		{"neigh", "show", "dev", slot.Bridge},
		{"netns", "exec", slot.NetNSName, "ip", "addr", "show"},
		{"netns", "exec", slot.NetNSName, "ip", "route", "show"},
		{"netns", "exec", slot.NetNSName, "sysctl", "net.ipv4.ip_forward"},
		{"netns", "exec", slot.NetNSName, "iptables", "-t", "nat", "-L", "-n", "-v"},
		{"netns", "exec", slot.NetNSName, "iptables", "-L", "-n", "-v"},
		{"netns", "exec", slot.NetNSName, "iptables", "-S"},
	} {
		output, err := exec.Command("ip", args...).CombinedOutput()
		fmt.Fprintf(&builder, "$ ip %s\n%s\n", strings.Join(args, " "), output)
		if err != nil {
			fmt.Fprintf(&builder, "(exit %v)\n", err)
		}
	}
	// Direct guest-listener check from inside the netns (bypasses the
	// DNAT/SNAT): distinguishes a data-plane problem from a guest-side
	// execd absence.
	if slot.GuestIP != "" {
		output, curlErr := exec.Command("ip", "netns", "exec", slot.NetNSName,
			"curl", "-s", "--max-time", "2", "-o", "/dev/null", "-w", "http_code=%{http_code}",
			fmt.Sprintf("http://%s:44772/ping", slot.GuestIP)).CombinedOutput()
		fmt.Fprintf(&builder, "$ netns curl %s:44772/ping\n%s\n", slot.GuestIP, output)
		if curlErr != nil {
			fmt.Fprintf(&builder, "(guest listener check failed: %v)\n", curlErr)
		}
	}
	// The firecracker process log carries the guest serial console, which
	// shows the guest-side network configuration. It lives next to the
	// instance rootfs (the jail root in jailer mode).
	id := truncatedSandboxID(slot.Owner.SandboxUID)
	logCandidates := []string{filepath.Join(stateRoot, "sandboxes", slot.Owner.SandboxUID, processLogName)}
	if jailed {
		logCandidates = []string{
			filepath.Join(stateRoot, jailerChrootBaseDir, "firecracker", id, jailerChrootRootDir, processLogName),
			filepath.Join(stateRoot, jailerChrootBaseDir, "firecracker", id, jailerChrootRootDir, "fc.log"),
		}
	}
	for _, candidate := range logCandidates {
		if payload, readErr := os.ReadFile(candidate); readErr == nil {
			tail := payload
			if len(tail) > 8192 {
				tail = tail[len(tail)-8192:]
			}
			fmt.Fprintf(&builder, "--- %s (guest serial) ---\n%s\n", filepath.Base(candidate), tail)
			break
		}
	}
	return builder.String()
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func readProcCmdline(t *testing.T, pid int) []string {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	require.NoError(t, err)
	return strings.Split(strings.TrimRight(string(payload), "\x00"), "\x00")
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
