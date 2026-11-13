package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/backendidentity"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/guestcontrol"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	"fast-sandbox/internal/ublkd"
)

func captureFileHash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type productionCaptureLease struct {
	driver   *Driver
	state    *SandboxState
	client   *Client
	device   *os.File
	record   captureRecord
	journal  *captureJournal
	boundary rootfsCaptureBoundary
}

func (l *productionCaptureLease) VerifyIdentity(ctx context.Context, r captureRecord) error {
	if r.Owner == nil || !runtimecontract.SameRuntimeIdentity(*r.Owner, l.state.Config.Identity) {
		return fmt.Errorf("%w: capture durable owner differs from source", errCaptureIdentityMismatch)
	}
	current, err := loadState(filepath.Join(l.driver.config.StateRoot, sandboxStateDir, l.state.Config.Identity.SandboxUID))
	if err != nil {
		return err
	}
	if !runtimecontract.SameRuntimeIdentity(current.Config.Identity, l.state.Config.Identity) || current.PID != l.state.PID || current.APIAddress != l.state.APIAddress {
		return fmt.Errorf("%w: capture runtime ownership changed", errCaptureIdentityMismatch)
	}
	identity, err := captureProcessIdentity(ctx, current.APIAddress, current.PID)
	if err != nil {
		return err
	}
	if identity != r.Incarnation {
		return fmt.Errorf("%w: capture VMM incarnation changed", errCaptureIdentityMismatch)
	}
	var config struct {
		Drives []struct {
			Path     string `json:"path_on_host"`
			Engine   string `json:"io_engine"`
			ReadOnly bool   `json:"is_read_only"`
		} `json:"drives"`
	}
	if err = l.client.do(ctx, "GET", "/vm/config", nil, &config); err != nil {
		return err
	}
	if len(config.Drives) != 1 || config.Drives[0].ReadOnly {
		return fmt.Errorf("%w: capture requires one writable rootfs drive", errCaptureIdentityMismatch)
	}
	path := config.Drives[0].Path
	if filepath.IsAbs(path) {
		path = filepath.Join(fmt.Sprintf("/proc/%d/root", current.PID), path)
	} else {
		path = filepath.Join(fmt.Sprintf("/proc/%d/cwd", current.PID), path)
	}
	actual, err := os.Stat(path)
	if err != nil {
		return err
	}
	held, err := l.device.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(actual, held) || actual.Mode()&os.ModeDevice == 0 || actual.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("%w: capture drive is not the owned block device", errCaptureIdentityMismatch)
	}
	expected, err := os.Stat(r.Device)
	if err != nil {
		return err
	}
	if !os.SameFile(expected, held) {
		return fmt.Errorf("%w: capture device ownership changed", errCaptureIdentityMismatch)
	}
	engine := config.Drives[0].Engine
	if engine == "" {
		engine = "Sync"
	}
	l.boundary = rootfsCaptureBoundary{IOEngine: engine, Filesystem: rootfsFilesystem, FilesystemExclusive: true}
	return l.boundary.validate()
}
func (l *productionCaptureLease) Verify(ctx context.Context) (rootfsCaptureBoundary, error) {
	if err := l.VerifyIdentity(ctx, l.record); err != nil {
		return rootfsCaptureBoundary{}, err
	}
	// A block-device rootfs.dev is insufficient to copy upper: verify that the daemon binds the device to the same config.
	devices, err := ublkd.ListSocket(ctx, filepath.Join(runtimecatalog.OCISocketDir, "ublkd.sock"))
	if err != nil {
		return rootfsCaptureBoundary{}, err
	}
	held, err := l.device.Stat()
	if err != nil {
		return rootfsCaptureBoundary{}, err
	}
	expectedConfig := filepath.Join(l.driver.ociRoot(), "sandboxes", l.state.Config.Identity.SandboxUID, "rootfs.config.v1.json")
	matched := false
	for _, dev := range devices {
		actual, e := os.Stat(dev.Dev)
		if e == nil && os.SameFile(actual, held) && dev.Config == expectedConfig && dev.Writable && dev.State == "running" && dev.Mode == "exclusive" {
			matched = true
		}
	}
	if !matched {
		return rootfsCaptureBoundary{}, errors.New("OverlayBD device is not exclusively bound to the captured upper config")
	}
	state, err := l.client.VMState(ctx)
	if err != nil {
		return rootfsCaptureBoundary{}, err
	}
	if state != vmRunning {
		return rootfsCaptureBoundary{}, errors.New("capture source must be Running")
	}
	return l.boundary, nil
}
func (l *productionCaptureLease) VMState(ctx context.Context) (string, error) {
	return l.client.VMState(ctx)
}
func (l *productionCaptureLease) Resume(ctx context.Context) error { return l.client.Resume(ctx) }
func (l *productionCaptureLease) Release() error                   { return nil } // This adapter does not own the lock; the coordinator releases the outer lock after the staging handoff.
func (l *productionCaptureLease) Quarantine(err error) error       { return l.journal.Quarantine(err) }

func (d *Driver) captureGuest(ctx context.Context, state *SandboxState, m artifacts.VMManifest, c *Client) (guestcontrol.Client, guestcontrol.Response, error) {
	var empty guestcontrol.Response
	guest, err := resolveCaptureGuest(ctx, state, m, c)
	if err != nil {
		return guest, empty, err
	}
	caps, err := guest.Call(ctx, guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"})
	if err != nil {
		return guest, caps, err
	}
	if err := validateCaptureHelper(m.GuestControl, caps); err != nil {
		return guest, caps, err
	}
	if caps.ProtectedMount != "/" || caps.Filesystem != rootfsFilesystem || caps.MaxLeaseMillis < 10000 || caps.Generation == math.MaxUint64 {
		return guest, caps, errors.New("guest root protection is incompatible")
	}
	return guest, caps, nil
}

// Capture and restart recovery share actual vsock path resolution; the Guest need not be running here.
func resolveCaptureGuest(ctx context.Context, state *SandboxState, m artifacts.VMManifest, c *Client) (guestcontrol.Client, error) {
	if m.GuestControl == nil || m.GuestControl.Port != 1234 {
		return guestcontrol.Client{}, errors.New("template missing supported guest control")
	}
	var config struct {
		VSock *struct {
			Path string `json:"uds_path"`
		} `json:"vsock"`
	}
	if err := c.do(ctx, "GET", "/vm/config", nil, &config); err != nil {
		return guestcontrol.Client{}, err
	}
	if config.VSock == nil {
		return guestcontrol.Client{}, errors.New("VMM has no vsock device")
	}
	// Restore may retain a build-time socket name; use the actual VMM configuration rather than guessing a new path.
	socket := config.VSock.Path
	if filepath.IsAbs(socket) {
		socket = filepath.Join(fmt.Sprintf("/proc/%d/root", state.PID), socket)
	} else {
		socket = filepath.Join(fmt.Sprintf("/proc/%d/cwd", state.PID), socket)
	}
	return guestcontrol.Client{SocketPath: socket, Port: m.GuestControl.Port, Timeout: 2 * time.Second}, nil
}

func (d *Driver) createRootfsSnapshot(ctx context.Context, input *runtimecontract.SnapshotInput) (result *runtimecontract.SnapshotResult, resultErr error) {
	if input.Kind == fastletapi.SnapshotKindCheckpoint {
		return nil, errors.New("RootfsOnly is template-only")
	}
	if input.Kind != "" && input.Kind != fastletapi.SnapshotKindTemplate {
		return nil, errors.New("unknown snapshot kind")
	}
	if input.Identity == nil {
		return nil, errors.New("RootfsOnly requires admitted runtime identity")
	}
	if err := validateSandboxID(input.SnapshotID); err != nil {
		return nil, err
	}
	release, err := lockSandboxLifecycle(ctx, d.config.StateRoot, input.SandboxID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	dir := filepath.Join(d.config.StateRoot, sandboxStateDir, input.SandboxID)
	state, err := loadState(dir)
	if err != nil {
		return nil, err
	}
	if !runtimecontract.SameRuntimeIdentity(state.Config.Identity, *input.Identity) || state.Phase != PhaseRunning {
		return nil, errors.New("snapshot source identity or phase changed")
	}
	if err = d.recoverRootfsCapture(ctx, state); err != nil {
		return nil, err
	}
	raw, manifest, source, err := d.readRootfsCaptureManifest(state)
	if err != nil {
		return nil, err
	}
	identity, err := d.qualifyRootfsSource(ctx, state, manifest)
	if err != nil {
		return nil, err
	}
	client := d.newClient(state.APIAddress)
	defer client.Close()
	version, err := client.Version(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkRootfsCaptureVersion(state.VMM, version); err != nil {
		return nil, err
	}
	guest, caps, err := d.captureGuest(ctx, state, manifest, client)
	if err != nil {
		return nil, err
	}
	manifest.Kernel.SHA256 = state.BootKernelSHA256
	manifest.GuestControl.HelperVersion = caps.HelperVersion
	devicePath := filepath.Join(dir, rootfsDeviceName)
	if d.config.JailerPath != "" {
		devicePath = filepath.Join(jailerRoot(filepath.Join(d.config.StateRoot, jailerChrootBaseDir), filepath.Base(d.config.BinaryPath), truncatedSandboxID(input.SandboxID)), rootfsDeviceName)
	}
	device, err := os.OpenFile(devicePath, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer device.Close()
	plan := dumpPlan{stateRoot: d.config.StateRoot, sandboxDir: dir, sandboxID: input.SandboxID, snapshotID: input.SnapshotID, staging: snapshotStagingPath(d.config.StateRoot, d.podUID, input.SnapshotID, true)}
	if _, err = checkpointDestinationForSource(source, input.SnapshotID); err != nil {
		return nil, err
	}
	// Budget only upper and pack workspace capacity, without reserving space for RAM.
	upperBytes, err := allocatedUpperBytes(filepath.Join(d.ociRoot(), "sandboxes", input.SandboxID, "upper"))
	if err != nil {
		return nil, err
	}
	if stagingFree(d.config.StateRoot) < 3*upperBytes+(512<<20) {
		return nil, runtimecontract.ErrInsufficientStorage
	}
	if err = d.cleanupSnapshotStaging(ctx, plan.staging); err != nil {
		return nil, err
	}
	record := captureRecord{Owner: &state.Config.Identity, Incarnation: identity, Device: devicePath, Staging: plan.staging, Request: guestcontrol.Request{Protocol: guestcontrol.Protocol, Epoch: caps.Epoch, Generation: caps.Generation + 1, OperationID: input.SnapshotID, LeaseMillis: 10000}}
	journal, err := prepareRootfsJournal(dir, record)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(plan.staging))
	}
	// Retain the copy and journal if recovery fails; delete only after source recovery and before publication takes ownership.
	publicationOwned := false
	defer func() {
		if !publicationOwned {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, d.cleanupUnpublishedCapture(cleanupCtx, state, journal))
		}
	}()
	if err = prepareCaptureStaging(plan.staging, state.Config.Identity); err != nil {
		return nil, err
	}
	lease := &productionCaptureLease{driver: d, state: state, client: client, device: device, record: record, journal: journal}
	tx := rootfsCapture{Progress: journal, Lease: lease, VM: client, Guest: guest, Request: record.Request, RecoveryTimeout: 10 * time.Second}
	if err = captureRootfsUpper(ctx, tx, device, d.ociRoot(), input.SandboxID, plan.staging); err != nil {
		return nil, err
	}
	// Once ownership and destination are fixed, pack/upload consume independent staging without holding the source lifecycle lock.
	publication, err := d.prepareRootfsPublication(ctx, plan, raw, manifest)
	if err != nil {
		return nil, err
	}
	journal.record.PublicationOwned = true
	if err = journal.persist(); err != nil {
		journal.record.PublicationOwned = false
		return nil, err
	}
	publicationOwned = true
	release()
	release = nil
	return d.publishPreparedOCISnapshot(ctx, plan, upperBytes, publication, input.OnPublishing)
}

// ReconcileSandbox retries source recovery without accepting or publishing an
// interrupted cut. The caller must supply the complete current runtime identity.
func (d *Driver) ReconcileSandbox(ctx context.Context, identity fastletapi.SandboxIdentity) error {
	release, err := lockSandboxLifecycle(ctx, d.config.StateRoot, identity.SandboxUID)
	if err != nil {
		return err
	}
	defer release()
	state, err := loadState(filepath.Join(d.config.StateRoot, sandboxStateDir, identity.SandboxUID))
	if err != nil {
		return err
	}
	if !runtimecontract.SameRuntimeIdentity(state.Config.Identity, identity) || (d.podUID != "" && d.podUID != identity.FastletPodUID) {
		return errCaptureIdentityMismatch
	}
	dead, err := d.abortExitedRootfsCapture(ctx, state, filepath.Join(d.config.StateRoot, sandboxStateDir, identity.SandboxUID))
	if err != nil || dead {
		return err
	}
	return d.recoverRootfsCapture(ctx, state)
}

func (d *Driver) recoverRootfsCapture(ctx context.Context, state *SandboxState) (resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("capture fence: %w", resultErr)
		}
	}()
	dir := filepath.Join(d.config.StateRoot, sandboxStateDir, state.Config.Identity.SandboxUID, "rootfs-capture")
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	journal, err := loadCaptureJournal(dir)
	if err != nil {
		return fmt.Errorf("capture fence: %w", err)
	}
	if journal.record.Phase == captureRecovered {
		return d.cleanupUnpublishedCapture(ctx, state, journal)
	}
	if journal.record.Phase == captureAborted {
		return nil
	}
	client := d.newClient(state.APIAddress)
	defer client.Close()
	device, err := os.OpenFile(journal.record.Device, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer device.Close()
	lease := &productionCaptureLease{driver: d, state: state, client: client, device: device, record: journal.record, journal: journal}
	// Verify peer identity before reading config or sending Resume; never act on a reused PID/socket.
	if err = lease.VerifyIdentity(ctx, journal.record); err != nil {
		return recoveryIdentityError(journal, err)
	}
	source, err := restoreReference(state.Config.Desired)
	if err != nil {
		return err
	}
	raw, err := readSourceManifest(d.config.StateRoot, d.ociRoot(), source, state.Config.Identity.SandboxUID)
	if err != nil {
		return err
	}
	m, err := artifacts.DecodeVMManifest(raw)
	if err != nil {
		return err
	}
	// A paused Guest cannot handshake. Read only static VMM device configuration here; the token verifies the epoch after recovery.
	guest, err := resolveCaptureGuest(ctx, state, m, client)
	if err != nil {
		return err
	}
	if err := reconcileCapture(ctx, journal, lease, guest, 10*time.Second); err != nil {
		return err
	}
	return d.cleanupUnpublishedCapture(ctx, state, journal)
}

// Preserve upstream extension fields; replace only the mode, state files, and file list owned by this capture.
func marshalRootfsSnapshotManifest(source []byte, manifest artifacts.VMManifest) ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(source, &document); err != nil {
		return nil, err
	}
	platform := map[string]json.RawMessage{}
	if raw := document["platform"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &platform); err != nil {
			return nil, err
		}
	}
	delete(platform, "checkpoint")
	delete(platform, "stateFiles")
	platform["snapshotMode"] = json.RawMessage(`"rootfsOnly"`)
	if len(manifest.Platform.StateFiles) > 0 {
		raw, err := json.Marshal(manifest.Platform.StateFiles)
		if err != nil {
			return nil, err
		}
		platform["stateFiles"] = raw
	}
	raw, err := json.Marshal(platform)
	if err != nil {
		return nil, err
	}
	document["platform"] = raw
	if err := setSnapshotKernelDigest(document, manifest.Kernel.SHA256); err != nil {
		return nil, err
	}
	delete(document, "files")
	return json.MarshalIndent(document, "", "  ")
}

// Do not Resume/Thaw a dead source; persist terminal state before reclaiming independent staging.
// Keep the source quarantined if its PID still exists, including reuse, or absence cannot be confirmed; API failure never proves death.
func (d *Driver) abortDeadRootfsCapture(ctx context.Context, state *SandboxState, directory string, confirmAbsent func(string) error) (bool, error) {
	path := filepath.Join(directory, "rootfs-capture")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	journal, err := loadCaptureJournal(path)
	if err != nil {
		return false, err
	}
	if journal.record.Phase == captureRecovered {
		return false, d.cleanupUnpublishedCapture(ctx, state, journal)
	}
	if journal.record.Owner == nil || !runtimecontract.SameRuntimeIdentity(*journal.record.Owner, state.Config.Identity) {
		return false, errors.New("dead capture owner mismatch")
	}
	if journal.record.Phase != captureAborted {
		absent, err := capturePIDAbsent(state.PID)
		if err != nil {
			return false, err
		}
		if !absent {
			return false, nil
		}
		// When cgroup management is configured, also confirm that the owned cgroup has no processes to avoid misclassification from a changed PID view.
		if confirmAbsent != nil {
			if err := confirmAbsent(state.Config.Identity.SandboxUID); err != nil {
				return false, err
			}
		}
		if err := journal.Record(captureAborted); err != nil {
			return false, err
		}
	}
	// Clear the stale PID before resource cleanup so retries cannot kill an unrelated process that reused it.
	d.mu.Lock()
	delete(d.processes, state.Config.Identity.SandboxUID)
	d.mu.Unlock()
	state.PID = 0
	state.APIAddress = ""
	state.Phase = PhaseFailed
	state.FailureMessage = "rootfs capture source exited; captured artifact discarded"
	if err := saveState(directory, state); err != nil {
		return true, err
	}
	expected := snapshotStagingPath(d.config.StateRoot, state.Config.Identity.FastletPodUID, journal.record.Request.OperationID, true)
	if journal.record.Staging != expected {
		return true, errors.New("dead capture staging ownership mismatch")
	}
	if err := d.cleanupSnapshotStaging(ctx, expected); err != nil {
		return true, err
	}
	return true, nil
}

func (d *Driver) qualifyRootfsSource(ctx context.Context, state *SandboxState, manifest artifacts.VMManifest) (string, error) {
	if manifest.Kernel.SHA256 == "" {
		return "", errors.New("source manifest does not record a kernel digest; rebuild the template and recreate the sandbox")
	}
	// The template records the actual boot kernel; an override cannot replace
	// this provenance check with a digest of a different node kernel.
	if err := qualifyBootKernel(state.BootKernelSHA256, manifest.Kernel.SHA256); err != nil {
		return "", err
	}
	q, err := readRootfsQualification(d.rootfsQualificationSources(state, manifest))
	if err != nil {
		return "", err
	}
	if q.KernelSHA256 != manifest.Kernel.SHA256 {
		return "", fmt.Errorf("qualification pins kernelSHA256 %s; the template records %s", q.KernelSHA256, manifest.Kernel.SHA256)
	}
	identity, err := captureProcessIdentity(ctx, state.APIAddress, state.PID)
	if err != nil {
		return "", err
	}
	if err := qualifyVMMExecutable(state, q.FirecrackerSHA256); err != nil {
		return "", err
	}
	if err := backendidentity.Verify(ctx, filepath.Join(runtimecatalog.OCISocketDir, "ublkd.sock"), q.OverlayBDSHA256); err != nil {
		return "", err
	}
	return identity, nil
}

func writeRootfsCapturedManifest(staging string, raw []byte, manifest artifacts.VMManifest) error {
	var err error
	if manifest.Platform == nil {
		manifest.Platform = &artifacts.VMPlatform{}
	}
	manifest.Platform.SnapshotMode = "rootfsOnly"
	manifest.Platform.Checkpoint = nil
	manifest.Platform.StateFiles = nil
	manifest.Files = nil
	for _, name := range []string{vaultBundleName, policyBundleName} {
		if _, e := os.Stat(filepath.Join(staging, name)); e == nil {
			entry, e := artifacts.FileEntry(filepath.Join(staging, name), map[string]string{})
			if e != nil {
				return e
			}
			if manifest.Platform.StateFiles == nil {
				manifest.Platform.StateFiles = map[string]artifacts.ManifestFile{}
			}
			manifest.Platform.StateFiles[name] = entry
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	metadata, err := marshalRootfsSnapshotManifest(raw, manifest)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(staging, snapshotManifestName), metadata, 0600); err != nil {
		return err
	}
	return nil
}

func (d *Driver) readRootfsCaptureManifest(state *SandboxState) ([]byte, artifacts.VMManifest, string, error) {
	source, err := restoreReference(state.Config.Desired)
	if err != nil {
		return nil, artifacts.VMManifest{}, "", err
	}
	if !strings.HasPrefix(source, "oci://") {
		return nil, artifacts.VMManifest{}, "", errors.New("RootfsOnly requires OCI rootfs")
	}
	raw, err := readSourceManifest(d.config.StateRoot, d.ociRoot(), source, state.Config.Identity.SandboxUID)
	if err != nil {
		return nil, artifacts.VMManifest{}, "", err
	}
	manifest, err := artifacts.DecodeVMManifest(raw)
	if err != nil {
		return nil, artifacts.VMManifest{}, "", err
	}
	if manifest.Kernel.Cmdline == "" {
		return nil, artifacts.VMManifest{}, "", errors.New("source template has no cold-boot cmdline; rebuild it")
	}
	return raw, manifest, source, nil
}

func prepareRootfsJournal(dir string, record captureRecord) (*captureJournal, error) {
	journalPath := filepath.Join(dir, "rootfs-capture")
	if old, e := loadCaptureJournal(journalPath); e == nil && old.record.Phase == captureRecovered {
		if !old.record.PublicationOwned {
			if _, err := os.Lstat(old.record.Staging); !os.IsNotExist(err) {
				return nil, errors.New("unpublished capture staging must be cleaned before replacing journal")
			}
		}
		if err := os.RemoveAll(journalPath); err != nil {
			return nil, err
		}
		if err := syncCaptureDirectory(dir); err != nil {
			return nil, err
		}
	}
	journal, err := newCaptureJournal(journalPath, record)
	if err != nil {
		return nil, err
	}
	return journal, nil
}

const (
	vmPaused                 = "Paused"
	vmRunning                = "Running"
	rootfsDeviceName         = "rootfs.dev"
	rootfsFirecrackerVersion = "1.16.1"
	rootfsFilesystem         = "ext4"
)

// The caller holds the source lifecycle lock; do not reclaim copies before recovery or after publication takes ownership.
func (d *Driver) cleanupUnpublishedCapture(ctx context.Context, state *SandboxState, journal *captureJournal) error {
	r := journal.record
	if r.Phase != captureRecovered || r.PublicationOwned {
		return nil
	}
	if r.Owner == nil || !runtimecontract.SameRuntimeIdentity(*r.Owner, state.Config.Identity) {
		return errors.New("capture staging owner mismatch")
	}
	expected := snapshotStagingPath(d.config.StateRoot, r.Owner.FastletPodUID, r.Request.OperationID, true)
	if r.Staging != expected {
		return errors.New("capture staging path mismatch")
	}
	return d.cleanupSnapshotStaging(ctx, expected)
}

// If template file or publication ownership preparation fails, the capture coordinator remains responsible for reclaiming the copy.
func (d *Driver) prepareRootfsPublication(ctx context.Context, plan dumpPlan, raw []byte, manifest artifacts.VMManifest) (ociPublication, error) {
	if err := d.stageTemplateState(ctx, plan.sandboxID, plan.staging); err != nil {
		return ociPublication{}, err
	}
	if err := writeRootfsCapturedManifest(plan.staging, raw, manifest); err != nil {
		return ociPublication{}, err
	}
	return prepareOCIPublication(plan)
}

func qualifyBootKernel(actual, qualified string) error {
	if actual == "" {
		return errors.New("running kernel provenance is missing; rebuild legacy Full templates and recreate the sandbox")
	}
	if actual != qualified {
		return errors.New("running guest kernel does not match qualified artifact")
	}
	return nil
}

func validateCaptureHelper(expected *artifacts.VMGuestControl, actual guestcontrol.Response) error {
	if expected == nil || actual.HelperVersion == "" || actual.HelperVersion != expected.HelperVersion {
		return errors.New("running helper version does not match template; rebuild with a verified helper")
	}
	return nil
}

// Both live reconciliation and cleanup retry use the configured cgroup fence.
func (d *Driver) abortExitedRootfsCapture(ctx context.Context, state *SandboxState, directory string) (bool, error) {
	d.mu.RLock()
	cgroups := d.runtimeCgroups
	d.mu.RUnlock()
	var confirmAbsent func(string) error
	if cgroups != nil {
		confirmAbsent = cgroups.ConfirmAbsent
	}
	return d.abortDeadRootfsCapture(ctx, state, directory, confirmAbsent)
}

// Deletion owns the lifecycle lock and never republishes the recovered source.
// A transient recovery failure keeps all source resources for a later retry.
func (d *Driver) reconcileCaptureForDelete(ctx context.Context, state *SandboxState, directory string) (resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("capture fence: %w", resultErr)
		}
	}()
	if err := checkSandboxCaptureFence(d.config.StateRoot, state.Config.Identity.SandboxUID); err == nil {
		return nil
	}
	if d.podUID != "" && state.Config.Identity.FastletPodUID != d.podUID {
		return errCaptureIdentityMismatch
	}
	dead, err := d.abortExitedRootfsCapture(ctx, state, directory)
	if err != nil || dead {
		return err
	}
	return d.recoverRootfsCapture(ctx, state)
}
