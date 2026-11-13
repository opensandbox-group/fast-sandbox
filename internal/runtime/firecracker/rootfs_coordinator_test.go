package firecracker

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/firecrackervmm"
	"fast-sandbox/internal/guestcontrol"
	"fast-sandbox/internal/janitor"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

func TestRootfsQualificationFailClosed(t *testing.T) {
	sum := strings.Repeat("a", 64)
	valid := rootfsQualification{FirecrackerSHA256: sum, KernelSHA256: sum, OverlayBDSHA256: sum, StorageRevision: "validated", ExclusiveExt4: true}
	for _, tc := range []struct {
		name   string
		mutate func(*rootfsQualification)
		ok     bool
	}{
		{"qualified", func(*rootfsQualification) {}, true},
		{"unqualified-storage", func(q *rootfsQualification) { q.OverlayBDSHA256 = "" }, false},
		{"raw-writers", func(q *rootfsQualification) { q.ExclusiveExt4 = false }, false},
		{"bad-kernel", func(q *rootfsQualification) { q.KernelSHA256 = "unknown" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := valid
			tc.mutate(&q)
			raw, err := json.Marshal(q)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "qualification.json")
			require.NoError(t, os.WriteFile(path, raw, 0600))
			t.Setenv("FAST_SANDBOX_ROOTFS_QUALIFICATION", path)
			_, err = readRootfsQualification(rootfsQualificationSources{})
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	t.Setenv("FAST_SANDBOX_ROOTFS_QUALIFICATION", "")
	_, err := readRootfsQualification(rootfsQualificationSources{})
	require.Error(t, err)
}

func TestRootfsCoordinatorRejectsBeforeSideEffects(t *testing.T) {
	d := &Driver{}
	for _, input := range []*runtimecontract.SnapshotInput{
		{Mode: fastletapi.SnapshotModeRootfsOnly, Kind: fastletapi.SnapshotKindCheckpoint},
		{Mode: fastletapi.SnapshotModeRootfsOnly, Kind: fastletapi.SnapshotKindTemplate},
		{Mode: "unknown"},
	} {
		input.SandboxID = "source"
		input.SnapshotID = "snapshot"
		_, err := d.CreateSnapshot(t.Context(), input)
		require.Error(t, err)
	}
}

func TestRootfsBootCompatibilityAndKernelTraversal(t *testing.T) {
	m := artifacts.NewVMManifest(artifacts.VMMachine{VCPUCount: 1, MemoryMiB: 256, RootfsSizeMiB: 1024, RootfsDriveID: "rootfs", RootfsDrivePath: "rootfs.dev"}, "6.1.177", "", artifacts.CPUTemplateNone)
	m.Compatibility.CPUArch = artifacts.HostCPUArch()
	m.Compatibility.FirecrackerVersion = "1.16.1"
	m.Platform = &artifacts.VMPlatform{SnapshotMode: "rootfsOnly"}
	m.Kernel.SHA256 = strings.Repeat("c", 64)
	m.Kernel.Cmdline = "root=/dev/vda rw"
	require.NoError(t, checkRootfsBootCompatibility(m, "1.16.1"))
	require.Error(t, checkRootfsBootCompatibility(m, "1.17.0"))
	m.Compatibility.FirecrackerVersion = "1.17.0"
	require.NoError(t, checkRootfsBootCompatibility(m, "1.17.0"))
	m.Compatibility.CPUArch = "unsupported"
	require.Error(t, checkRootfsBootCompatibility(m, "1.16.1"))
	m.Kernel.Version = "../../kernel"
	_, err := (&Driver{}).rootfsKernel(m)
	require.Error(t, err)
}

func TestRootfsSnapshotPreservesExtensionsAndDropsMemory(t *testing.T) {
	m := artifacts.NewVMManifest(artifacts.VMMachine{VCPUCount: 1, MemoryMiB: 256, RootfsSizeMiB: 1024, RootfsDriveID: "rootfs", RootfsDrivePath: "rootfs.dev"}, "6.1.177", "", artifacts.CPUTemplateNone)
	m.Compatibility.CPUArch = artifacts.HostCPUArch()
	m.Compatibility.FirecrackerVersion = "1.16.1"
	m.Kernel.SHA256 = strings.Repeat("c", 64)
	m.Kernel.Cmdline = "root=/dev/vda rw"
	m.Platform = &artifacts.VMPlatform{SnapshotMode: "rootfsOnly"}
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	var source map[string]any
	require.NoError(t, json.Unmarshal(raw, &source))
	source["extension"] = map[string]any{"keep": true}
	source["files"] = map[string]any{"memory.snap": map[string]any{}}
	source["platform"].(map[string]any)["extension"] = "keep"
	source["platform"].(map[string]any)["checkpoint"] = map[string]any{"sandboxUID": "old"}
	raw, err = json.Marshal(source)
	require.NoError(t, err)
	m.Kernel.SHA256 = strings.Repeat("d", 64)
	result, err := marshalRootfsSnapshotManifest(raw, m)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(result, &got))
	require.Equal(t, source["extension"], got["extension"])
	require.Equal(t, m.Kernel.SHA256, got["kernel"].(map[string]any)["sha256"])
	require.NotContains(t, got, "files")
	platform := got["platform"].(map[string]any)
	require.Equal(t, "keep", platform["extension"])
	require.NotContains(t, platform, "checkpoint")
	decoded, err := artifacts.DecodeVMManifest(result)
	require.NoError(t, err)
	require.True(t, decoded.IsRootfsOnly())
}

func TestRootfsPublicationRetainsOwnerAfterSourceDeletion(t *testing.T) {
	t.Setenv(ociSnapshotRepositoryEnv, "registry.example/snapshots")
	root := t.TempDir()
	plan := dumpPlan{sandboxDir: filepath.Join(root, "source"), staging: filepath.Join(root, "staging"), snapshotID: "snapshot"}
	require.NoError(t, os.MkdirAll(plan.sandboxDir, 0700))
	require.NoError(t, os.MkdirAll(plan.staging, 0700))
	state := &SandboxState{Phase: PhaseRunning}
	state.Config.Identity = fastletapi.SandboxIdentity{SandboxUID: "source", FastletPodUID: "pod", Namespace: "test"}
	state.Config.Desired.Image = "oci://registry.example/templates@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(t, saveState(plan.sandboxDir, state))
	publication, err := prepareOCIPublication(plan)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(plan.sandboxDir))
	require.Equal(t, "registry.example/snapshots:snapshot", publication.destination)
	owner, err := os.ReadFile(filepath.Join(plan.staging, ".pack-owner.json"))
	require.NoError(t, err)
	require.Contains(t, string(owner), "source")
	require.Contains(t, string(owner), "pod")
}
func TestRootfsUpperBudgetUsesAllocatedBlocks(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"writable_data", "writable_index"} {
		f, err := os.Create(filepath.Join(root, name))
		require.NoError(t, err)
		require.NoError(t, f.Truncate(1<<30))
		_, err = f.WriteAt([]byte("extent"), 1<<29)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	size, err := allocatedUpperBytes(root)
	require.NoError(t, err)
	require.Positive(t, size)
	require.Less(t, size, int64(1<<20))
}

func TestRootfsFailedCaptureStagingRecovery(t *testing.T) {
	for _, scenario := range []string{"partial-upper", "partial-upper-live", "prepare-failed", "published", "quarantined", "wrong-path", "cleanup-failed"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			d := &Driver{config: firecrackerConfigForTest(t, root), newClient: NewClient}
			state := &SandboxState{Phase: PhaseRunning}
			state.Config.Identity = fastletapi.SandboxIdentity{SandboxUID: "source", FastletPodUID: "pod", Namespace: "test"}
			directory := filepath.Join(root, sandboxStateDir, "source")
			require.NoError(t, os.MkdirAll(directory, 0700))
			staging := snapshotStagingPath(root, "pod", "failed", true)
			require.NoError(t, os.MkdirAll(staging, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(staging, "writable_data"), []byte("partial"), 0600))
			fixture := &captureFixture{}
			tx := fixture.transaction()
			tx.Request.OperationID = "failed"
			journal, err := newCaptureJournal(filepath.Join(directory, "rootfs-capture"), captureRecord{
				Owner: &state.Config.Identity, Incarnation: "boot", Device: "/dev/ublkb0", Staging: staging, Request: tx.Request,
			})
			require.NoError(t, err)
			tx.Progress = journal
			if strings.HasPrefix(scenario, "partial-upper") {
				tx.Cut = func() error { return errors.New("index copy failed") }
				require.ErrorContains(t, tx.run(t.Context()), "index copy failed")
			} else {
				require.NoError(t, tx.run(t.Context()))
			}
			switch scenario {
			case "prepare-failed":
				_, err = d.prepareRootfsPublication(t.Context(), dumpPlan{sandboxID: "source", sandboxDir: directory, staging: staging}, nil, artifacts.VMManifest{})
				require.Error(t, err)
			case "partial-upper-live":
				require.NoError(t, d.cleanupUnpublishedCapture(t.Context(), state, journal))
				require.NoDirExists(t, staging)
			case "published":
				journal.record.PublicationOwned = true
			case "quarantined":
				journal.record.Phase = captureQuarantined
			case "wrong-path":
				journal.record.Staging = filepath.Join(root, "unowned")
			case "cleanup-failed":
				// Use ENOTDIR to simulate cleanup failure; retain the old journal so the next capture cannot overwrite it.
				require.NoError(t, os.RemoveAll(staging))
				require.NoError(t, os.WriteFile(staging, []byte("not a directory"), 0600))
			}
			require.NoError(t, journal.persist())
			// Reload the disk record to simulate process exit after source recovery but before publication takes ownership.
			err = d.recoverRootfsCapture(t.Context(), state)
			switch scenario {
			case "quarantined", "wrong-path", "cleanup-failed":
				require.Error(t, err)
				require.FileExists(t, filepath.Join(directory, "rootfs-capture", "capture.json"))
				if scenario == "cleanup-failed" {
					_, err = prepareRootfsJournal(directory, journal.record)
					require.ErrorContains(t, err, "must be cleaned")
				}
			case "published":
				require.NoError(t, err)
				require.FileExists(t, filepath.Join(staging, "writable_data"))
			default:
				require.NoError(t, err)
				require.NoDirExists(t, staging)
				// Cleanup is idempotent; a subsequent capture with a new ID leaves no old data behind.
				require.NoError(t, d.recoverRootfsCapture(t.Context(), state))
				record := journal.record
				record.Request.OperationID = "next"
				record.Staging = snapshotStagingPath(root, "pod", "next", true)
				_, err = prepareRootfsJournal(directory, record)
				require.NoError(t, err)
			}
		})
	}
}

func TestRootfsCaptureOwnerVisibleBeforePublicationAfterPodReplacement(t *testing.T) {
	root := t.TempDir()
	old := fastletapi.SandboxIdentity{SandboxUID: "source", FastletPodUID: "old-pod"}
	staging := snapshotStagingPath(root, old.FastletPodUID, "capture", true)
	require.NoError(t, os.MkdirAll(staging, 0700))
	require.NoError(t, prepareCaptureStaging(staging, old))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "writable_data"), []byte("partial upper"), 0600))
	// Publication has not started; a different Driver must not adopt the old Pod.
	replacement := &Driver{config: firecrackerConfigForTest(t, root)}
	replacement.SetFastletPodUID("replacement-pod")
	directory := filepath.Join(root, sandboxStateDir, old.SandboxUID)
	require.NoError(t, os.MkdirAll(directory, 0700))
	state := &SandboxState{Phase: PhaseRunning}
	state.Config.Identity = old
	require.NoError(t, saveState(directory, state))
	fixture := &captureFixture{}
	request := fixture.transaction().Request
	request.OperationID = "capture"
	_, err := newCaptureJournal(filepath.Join(directory, "rootfs-capture"), captureRecord{Owner: &old, Incarnation: "old-vmm", Device: "/dev/ublkb0", Staging: staging, Request: request})
	require.NoError(t, err)
	require.NoError(t, replacement.RecoverRuntimeResources(t.Context(), nil, nil))
	require.FileExists(t, filepath.Join(staging, "writable_data"), "replacement must leave old Pod ownership to Janitor")
	resources, err := janitor.NewOCIBackend(replacement.config.StateRoot).Scan(t.Context())
	require.NoError(t, err)
	require.Len(t, resources, 1)
	require.Equal(t, "old-pod", resources[0].FastletPodUID)
	require.Equal(t, "pack/old-pod/capture", resources[0].ResourceID)
	require.NoFileExists(t, filepath.Join(staging, "manifest.json"))
	entries, err := os.ReadDir(staging)
	require.NoError(t, err)
	require.Len(t, entries, 2, "atomic ownership publication must not leave temporary markers")
}

func TestRootfsQualificationUsesRunningKernelProvenance(t *testing.T) {
	qualified := strings.Repeat("a", 64)
	require.NoError(t, qualifyBootKernel(qualified, qualified))
	require.ErrorContains(t, qualifyBootKernel("", qualified), "provenance is missing")
	// Full RAM may hold B while this host has a qualified A installation.
	// No current host path is consulted to promote B to a qualified kernel.
	require.ErrorContains(t, qualifyBootKernel(strings.Repeat("b", 64), qualified), "running guest kernel")
	state := &SandboxState{BootKernelSHA256: qualified, Config: fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{SandboxUID: "kernel-source"}}, Phase: PhaseRunning}
	directory := t.TempDir()
	require.NoError(t, saveState(directory, state))
	restored, err := loadState(directory)
	require.NoError(t, err)
	require.Equal(t, qualified, restored.BootKernelSHA256)
}

func TestRootfsCaptureChecksActualHelperImplementation(t *testing.T) {
	for _, version := range []string{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64)} {
		manifest := &artifacts.VMGuestControl{Protocol: guestcontrol.Protocol, HelperVersion: version}
		require.NoError(t, validateCaptureHelper(manifest, guestcontrol.Response{Protocol: guestcontrol.Protocol, HelperVersion: version}))
		require.Error(t, validateCaptureHelper(manifest, guestcontrol.Response{Protocol: guestcontrol.Protocol, HelperVersion: "1"}))
		require.Error(t, validateCaptureHelper(manifest, guestcontrol.Response{Protocol: guestcontrol.Protocol}))
	}
}

func TestRootfsKernelProvenancePreservesExtensions(t *testing.T) {
	document := map[string]json.RawMessage{"kernel": json.RawMessage(`{"version":"6.1","sha256":"old","future":{"keep":true}}`)}
	require.NoError(t, setSnapshotKernelDigest(document, strings.Repeat("b", 64)))
	require.Contains(t, string(document["kernel"]), `"future":{"keep":true}`)
	require.Contains(t, string(document["kernel"]), strings.Repeat("b", 64))
	require.NoError(t, setSnapshotKernelDigest(document, ""))
	require.NotContains(t, string(document["kernel"]), `"sha256"`)
}

func qualificationSourcesFixture(t *testing.T) rootfsQualificationSources {
	t.Helper()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "oci")
	require.NoError(t, os.Mkdir(bundle, 0700))
	raw := []byte(strings.Repeat("b", 64) + "  libexec/overlaybd-ublkd\n" + strings.Repeat("d", 64) + "  bin/overlaybd-ublkd\n")
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "SHA256SUMS"), raw, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "REVISION"), []byte(fmt.Sprintf("%x\n", sha256.Sum256(raw))), 0600))
	return rootfsQualificationSources{KernelSHA256: strings.Repeat("c", 64), ManagedVMM: true, OverlayBDBundle: bundle, StandardTopology: true}
}

func TestRootfsQualificationDefaultsAndOverrides(t *testing.T) {
	t.Setenv("FAST_SANDBOX_ROOTFS_QUALIFICATION", "")
	source := qualificationSourcesFixture(t)
	q, err := readRootfsQualification(source)
	require.NoError(t, err)
	require.Empty(t, q.FirecrackerSHA256)
	require.Equal(t, strings.Repeat("b", 64), q.OverlayBDSHA256, "use executable, not wrapper digest")
	require.Equal(t, source.KernelSHA256, q.KernelSHA256)
	require.True(t, q.ExclusiveExt4)
	require.Len(t, q.StorageRevision, 64)
	for _, test := range []struct {
		name, body string
		ok         bool
	}{
		{"partial", `{"storageRevision":"operator-tested"}`, true},
		{"explicit-disable", `{"exclusiveExt4":false}`, false},
		{"empty-digest", `{"firecrackerSHA256":""}`, false},
		{"invalid-digest", `{"kernelSHA256":"wrong"}`, false},
		{"null", `{"exclusiveExt4":null}`, false},
		{"typo", `{"exclusiveEXT4":true}`, false},
		{"not-object", `null`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.json")
			require.NoError(t, os.WriteFile(path, []byte(test.body), 0600))
			t.Setenv("FAST_SANDBOX_ROOTFS_QUALIFICATION", path)
			_, err := readRootfsQualification(source)
			if test.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	source.StandardTopology = false
	_, err = readRootfsQualification(source)
	require.ErrorContains(t, err, "custom topologies")
	path := filepath.Join(t.TempDir(), "profile.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"exclusiveExt4":true}`), 0600))
	t.Setenv("FAST_SANDBOX_ROOTFS_QUALIFICATION", path)
	_, err = readRootfsQualification(source)
	require.NoError(t, err)
}

func TestRootfsQualificationBrokenBundlesFailClosed(t *testing.T) {
	t.Setenv("FAST_SANDBOX_ROOTFS_QUALIFICATION", "")
	for _, test := range []string{"missing-manifest", "missing-entry", "wrong-revision"} {
		t.Run(test, func(t *testing.T) {
			source := qualificationSourcesFixture(t)
			switch test {
			case "missing-manifest":
				require.NoError(t, os.Remove(filepath.Join(source.OverlayBDBundle, "SHA256SUMS")))
			case "missing-entry":
				raw := []byte(strings.Repeat("a", 64) + "  bin/overlaybd-ublkd\n")
				require.NoError(t, os.WriteFile(filepath.Join(source.OverlayBDBundle, "SHA256SUMS"), raw, 0600))
				require.NoError(t, os.WriteFile(filepath.Join(source.OverlayBDBundle, "REVISION"), []byte(fmt.Sprintf("%x", sha256.Sum256(raw))), 0600))
			case "wrong-revision":
				require.NoError(t, os.WriteFile(filepath.Join(source.OverlayBDBundle, "REVISION"), []byte("stale"), 0600))
			}
			_, err := readRootfsQualification(source)
			require.Error(t, err)
			if test == "missing-entry" {
				require.ErrorContains(t, err, "bundle does not list libexec/overlaybd-ublkd")
			}
		})
	}
}

func TestRootfsQualificationTopologyDefaults(t *testing.T) {
	for _, test := range []struct {
		dir, state        string
		managed, standard bool
	}{
		{firecrackervmm.DefaultDir, "/var/lib/fast-sandbox/firecracker", true, true},
		{firecrackervmm.DefaultDir, "/custom/state", true, false},
		{"/custom/vmm", "/var/lib/fast-sandbox/firecracker", true, false},
		{"", "/var/lib/fast-sandbox/firecracker", false, false},
	} {
		d := &Driver{config: runtimecatalog.FirecrackerConfig{VMMDir: test.dir, StateRoot: test.state}}
		state := &SandboxState{}
		if test.managed {
			state.VMM = &firecrackervmm.Selection{Version: "1.17.0", BinaryPath: filepath.Join(test.dir, "versions", "1.17.0", "firecracker")}
		}
		source := d.rootfsQualificationSources(state, artifacts.VMManifest{})
		require.Equal(t, test.standard, source.StandardTopology)
		require.Equal(t, test.managed, source.ManagedVMM)
	}
}

func TestRootfsQualificationCustomVMMNeedsExplicitDigest(t *testing.T) {
	t.Setenv("FAST_SANDBOX_ROOTFS_QUALIFICATION", "")
	source := qualificationSourcesFixture(t)
	source.ManagedVMM = false
	_, err := readRootfsQualification(source)
	require.ErrorContains(t, err, "custom VMM")
	path := filepath.Join(t.TempDir(), "profile.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"firecrackerSHA256":"`+strings.Repeat("a", 64)+`"}`), 0600))
	t.Setenv("FAST_SANDBOX_ROOTFS_QUALIFICATION", path)
	_, err = readRootfsQualification(source)
	require.NoError(t, err)
}

func TestRootfsQualificationKernelDiagnostics(t *testing.T) {
	d := &Driver{}
	state := &SandboxState{BootKernelSHA256: strings.Repeat("a", 64)}
	_, err := d.qualifyRootfsSource(t.Context(), state, artifacts.VMManifest{})
	require.ErrorContains(t, err, "rebuild the template and recreate the sandbox")
	profile := filepath.Join(t.TempDir(), "qualification.json")
	q := rootfsQualification{FirecrackerSHA256: strings.Repeat("a", 64), KernelSHA256: strings.Repeat("b", 64), OverlayBDSHA256: strings.Repeat("c", 64), StorageRevision: "tested", ExclusiveExt4: true}
	raw, err := json.Marshal(q)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(profile, raw, 0600))
	t.Setenv("FAST_SANDBOX_ROOTFS_QUALIFICATION", profile)
	manifest := artifacts.VMManifest{}
	manifest.Kernel.SHA256 = state.BootKernelSHA256
	_, err = d.qualifyRootfsSource(t.Context(), state, manifest)
	require.ErrorContains(t, err, "qualification pins kernelSHA256")
}
