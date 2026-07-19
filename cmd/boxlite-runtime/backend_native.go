//go:build boxlite_native

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fast-sandbox/internal/boxlitesidecar"
	"fast-sandbox/internal/boxlitestate"
	"fast-sandbox/internal/boxlitewire"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"

	boxlite "github.com/boxlite-ai/boxlite/sdks/go"
	"k8s.io/apimachinery/pkg/api/resource"
)

type nativeRecord = boxlitestate.SandboxRecord

type nativeBackend struct {
	mu           sync.Mutex
	runtime      *boxlite.Runtime
	podUID       string
	homeDir      string
	metadataRoot string
	bundleRoot   string
	artifactRoot string
	records      map[string]*nativeRecord
	tunnels      map[string]*boxlite.Execution
}

func newBackend(stateRoot string) (boxlitesidecar.Backend, error) {
	podUID := strings.TrimSpace(os.Getenv("POD_UID"))
	if podUID == "" {
		return nil, errors.New("POD_UID is required")
	}
	if stateRoot == "" {
		return nil, errors.New("BoxLite state root is required")
	}
	homeDir := boxlitestate.HomeDirectory(stateRoot, podUID)
	backend := &nativeBackend{
		podUID: podUID, homeDir: homeDir, metadataRoot: filepath.Join(homeDir, boxlitestate.MetadataDirectoryName),
		bundleRoot:   filepath.Join(homeDir, boxlitestate.BundleDirectoryName),
		artifactRoot: strings.TrimSpace(os.Getenv("FAST_SANDBOX_INFRA_STORE_ROOT")),
		records:      make(map[string]*nativeRecord), tunnels: make(map[string]*boxlite.Execution),
	}
	if backend.artifactRoot == "" {
		backend.artifactRoot = "/opt/fast-sandbox/infra"
	}
	for _, directory := range []string{homeDir, backend.metadataRoot, backend.bundleRoot} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, err
		}
	}
	if err := ensureOwnerRecord(homeDir, podUID); err != nil {
		return nil, err
	}
	if err := backend.loadRecords(); err != nil {
		return nil, err
	}
	runtimeOptions, err := nativeRuntimeOptions(homeDir)
	if err != nil {
		return nil, err
	}
	runtime, err := boxlite.NewRuntime(runtimeOptions...)
	if err != nil {
		return nil, err
	}
	backend.runtime = runtime
	return backend, nil
}

func ensureOwnerRecord(homeDir, podUID string) error {
	path := filepath.Join(homeDir, boxlitestate.OwnerFileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		record := boxlitestate.OwnerRecord{Version: boxlitestate.Version, FastletPodUID: podUID, CreatedAt: time.Now().Unix()}
		encodeErr := json.NewEncoder(file).Encode(record)
		syncErr := file.Sync()
		closeErr := file.Close()
		return errors.Join(encodeErr, syncErr, closeErr)
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	file, err = os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var record boxlitestate.OwnerRecord
	if err := json.NewDecoder(file).Decode(&record); err != nil {
		return err
	}
	if record.Version != boxlitestate.Version || record.FastletPodUID != podUID || record.CreatedAt <= 0 {
		return errors.New("BoxLite state owner fence does not match this Fastlet Pod")
	}
	return nil
}

func nativeRuntimeOptions(homeDir string) ([]boxlite.RuntimeOption, error) {
	options := []boxlite.RuntimeOption{boxlite.WithHomeDir(homeDir)}
	host := strings.TrimSpace(os.Getenv("FAST_SANDBOX_BOXLITE_REGISTRY_HOST"))
	if host == "" {
		return options, nil
	}
	transport := boxlite.RegistryTransportHTTPS
	switch configured := strings.TrimSpace(os.Getenv("FAST_SANDBOX_BOXLITE_REGISTRY_TRANSPORT")); configured {
	case "", string(boxlite.RegistryTransportHTTPS):
	case string(boxlite.RegistryTransportHTTP):
		transport = boxlite.RegistryTransportHTTP
	default:
		return nil, fmt.Errorf("unsupported BoxLite registry transport %q", configured)
	}
	skipVerify, err := optionalBoolEnv("FAST_SANDBOX_BOXLITE_REGISTRY_SKIP_VERIFY")
	if err != nil {
		return nil, err
	}
	search, err := optionalBoolEnv("FAST_SANDBOX_BOXLITE_REGISTRY_SEARCH")
	if err != nil {
		return nil, err
	}
	options = append(options, boxlite.WithImageRegistry(boxlite.ImageRegistry{
		Host: host, Transport: transport, SkipVerify: skipVerify, Search: search,
	}))
	return options, nil
}

func optionalBoolEnv(name string) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func (b *nativeBackend) Capabilities(context.Context) boxlitewire.Capabilities {
	capabilities := map[string]bool{
		boxlitewire.CapabilityOwnerFence:     true,
		boxlitewire.CapabilityArtifactVolume: true,
		boxlitewire.CapabilityLocalForward:   true,
		boxlitewire.CapabilityResourceLimit:  false,
		boxlitewire.CapabilityRecovery:       true,
		boxlitewire.CapabilityImageCache:     true,
	}
	return boxlitewire.Capabilities{
		ProtocolVersion: boxlitewire.ProtocolVersionV1, Ready: false,
		Reason:       "BoxLiteResourceEnforcementIncomplete",
		Message:      "native BoxLite lifecycle and authenticated LocalForward are available, but resource enforcement is incomplete",
		Capabilities: capabilities,
	}
}

func (b *nativeBackend) Ensure(ctx context.Context, request boxlitewire.EnsureRequest) (boxlitewire.Box, error) {
	if err := validateEnsureRequest(request); err != nil {
		return boxlitewire.Box{}, invalid(err)
	}
	hash, err := ensureHash(request)
	if err != nil {
		return boxlitewire.Box{}, internal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	record := b.records[request.Sandbox.SandboxID]
	if record != nil && record.SpecHash != hash {
		return boxlitewire.Box{}, immutableSpecConflict("Sandbox UID is already bound to a different immutable BoxLite create spec")
	}
	if record == nil {
		hostPort, err := allocateHostPort()
		if err != nil {
			return boxlitewire.Box{}, unavailableError(err)
		}
		tunnelCredential, err := fastletnetwork.GenerateLocalForwardCredential()
		if err != nil {
			return boxlitewire.Box{}, internal(fmt.Errorf("generate LocalForward credential: %w", err))
		}
		record = &nativeRecord{
			Version: boxlitestate.Version, Namespace: request.Namespace, SpecHash: hash, Request: request,
			HostPort: hostPort, TunnelCredential: tunnelCredential, CreatedAt: time.Now().Unix(),
			BundleRoot: b.expectedBundleRoot(request.Sandbox.SandboxID, hash),
		}
		if err := b.prepareBundle(record); err != nil {
			return boxlitewire.Box{}, invalid(err)
		}
		if err := b.persistRecord(record); err != nil {
			return boxlitewire.Box{}, internal(err)
		}
		b.records[request.Sandbox.SandboxID] = record
	}
	box, err := b.ensureBoxLocked(ctx, record)
	if err != nil {
		return boxlitewire.Box{}, mapNativeError(err)
	}
	return box, nil
}

func (b *nativeBackend) Inspect(ctx context.Context, sandboxUID string) (boxlitewire.Box, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record := b.records[sandboxUID]
	if record == nil {
		return boxlitewire.Box{}, notFound("BoxLite Sandbox metadata was not found")
	}
	info, err := b.runtime.GetInfo(ctx, recordIdentity(record))
	if err != nil {
		return boxlitewire.Box{}, mapNativeError(err)
	}
	return wireBox(record, info), nil
}

func (b *nativeBackend) Recover(ctx context.Context, sandboxUID string) (boxlitewire.Box, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record := b.records[sandboxUID]
	if record == nil {
		return boxlitewire.Box{}, notFound("BoxLite Sandbox metadata was not found")
	}
	box, err := b.ensureBoxLocked(ctx, record)
	if err != nil {
		return boxlitewire.Box{}, mapNativeError(err)
	}
	return box, nil
}

func (b *nativeBackend) Delete(ctx context.Context, sandboxUID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	record := b.records[sandboxUID]
	if record == nil {
		return nil
	}
	b.closeTunnelLocked(sandboxUID)
	if err := b.runtime.ForceRemove(ctx, recordIdentity(record)); err != nil && !boxlite.IsNotFound(err) {
		return mapNativeError(err)
	}
	if err := os.Remove(b.recordPath(sandboxUID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return internal(err)
	}
	if err := os.RemoveAll(filepath.Join(b.bundleRoot, safePathSegment(sandboxUID))); err != nil {
		return internal(err)
	}
	delete(b.records, sandboxUID)
	return nil
}

func (b *nativeBackend) List(ctx context.Context, namespace string) ([]boxlitewire.Box, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	infos, err := b.runtime.ListInfo(ctx)
	if err != nil {
		return nil, mapNativeError(err)
	}
	byIdentity := make(map[string]*boxlite.BoxInfo, len(infos)*2)
	for index := range infos {
		info := &infos[index]
		byIdentity[info.ID] = info
		byIdentity[info.Name] = info
	}
	boxes := make([]boxlitewire.Box, 0, len(b.records))
	for _, record := range b.records {
		if namespace != "" && record.Namespace != namespace {
			continue
		}
		info := byIdentity[recordIdentity(record)]
		if info == nil {
			continue
		}
		boxes = append(boxes, wireBox(record, info))
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Sandbox.SandboxID < boxes[j].Sandbox.SandboxID })
	return boxes, nil
}

func (b *nativeBackend) ListImages(ctx context.Context) ([]string, error) {
	images, err := b.runtime.Images()
	if err != nil {
		return nil, mapNativeError(err)
	}
	defer images.Close()
	entries, err := images.List(ctx)
	if err != nil {
		return nil, mapNativeError(err)
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Reference != "" {
			result = append(result, entry.Reference)
		}
	}
	sort.Strings(result)
	return result, nil
}

func (b *nativeBackend) PullImage(ctx context.Context, image string) error {
	images, err := b.runtime.Images()
	if err != nil {
		return mapNativeError(err)
	}
	defer images.Close()
	_, err = images.Pull(ctx, image)
	return mapNativeError(err)
}

func (b *nativeBackend) Close() error {
	b.mu.Lock()
	for sandboxUID := range b.tunnels {
		b.closeTunnelLocked(sandboxUID)
	}
	runtime := b.runtime
	b.runtime = nil
	b.mu.Unlock()
	if runtime != nil {
		return runtime.Close()
	}
	return nil
}

func (b *nativeBackend) ensureBoxLocked(ctx context.Context, record *nativeRecord) (boxlitewire.Box, error) {
	options, err := boxOptions(record)
	if err != nil {
		return boxlitewire.Box{}, err
	}
	box, _, err := b.runtime.GetOrCreate(ctx, record.Request.Sandbox.Image, options...)
	if err != nil {
		return boxlitewire.Box{}, err
	}
	defer box.Close()
	if record.BoxID != box.ID() {
		record.BoxID = box.ID()
		if err := b.persistRecord(record); err != nil {
			return boxlitewire.Box{}, err
		}
	}
	if err := box.Start(ctx); err != nil {
		return boxlitewire.Box{}, err
	}
	if err := b.ensureTunnelLocked(ctx, record, box); err != nil {
		return boxlitewire.Box{}, err
	}
	info, err := box.Info(ctx)
	if err != nil {
		return boxlitewire.Box{}, err
	}
	return wireBox(record, info), nil
}

func boxOptions(record *nativeRecord) ([]boxlite.BoxOption, error) {
	request := record.Request
	cpus, memoryMiB, err := resourceOptions(request.Sandbox.CPU, request.Sandbox.Memory)
	if err != nil {
		return nil, err
	}
	options := []boxlite.BoxOption{
		boxlite.WithName(request.Sandbox.SandboxID), boxlite.WithCPUs(cpus), boxlite.WithMemory(memoryMiB),
		boxlite.WithPort(boxlite.PortSpec{Host: int(record.HostPort), Guest: int(request.TunnelGuestPort), Protocol: boxlite.PortProtocolTcp}),
		boxlite.WithVolumeReadOnly(record.BundleRoot, "/.fast"), boxlite.WithAutoRemove(false), boxlite.WithDetach(true),
		boxlite.WithNetwork(boxlite.NetworkSpec{Mode: boxlite.NetworkModeEnabled}),
	}
	for key, value := range request.Sandbox.Env {
		options = append(options, boxlite.WithEnv(key, value))
	}
	if request.Sandbox.WorkingDir != "" {
		options = append(options, boxlite.WithWorkDir(request.Sandbox.WorkingDir))
	}
	wrapper := hasArtifact(request.Artifacts, fastletinfra.SandboxInitContainerPath)
	if wrapper {
		if len(request.Sandbox.Command) == 0 {
			return nil, errors.New("BoxLite sandbox-init requires an explicit user command until image config introspection is available")
		}
		options = append(options, boxlite.WithEntrypoint(fastletinfra.SandboxInitContainerPath))
		args := []string{"--config", fastletinfra.InstanceConfigPath, "--"}
		args = append(args, request.Sandbox.Command...)
		args = append(args, request.Sandbox.Args...)
		options = append(options, boxlite.WithCmd(args...))
	} else {
		if len(request.Sandbox.Command) > 0 {
			options = append(options, boxlite.WithEntrypoint(request.Sandbox.Command...))
		}
		if len(request.Sandbox.Args) > 0 {
			options = append(options, boxlite.WithCmd(request.Sandbox.Args...))
		}
	}
	return options, nil
}

func resourceOptions(cpu, memory string) (int, int, error) {
	cpuQuantity, err := resource.ParseQuantity(cpu)
	if err != nil || cpuQuantity.Sign() <= 0 {
		return 0, 0, fmt.Errorf("invalid BoxLite CPU quantity %q", cpu)
	}
	memoryQuantity, err := resource.ParseQuantity(memory)
	if err != nil || memoryQuantity.Sign() <= 0 {
		return 0, 0, fmt.Errorf("invalid BoxLite memory quantity %q", memory)
	}
	cpus := int((cpuQuantity.MilliValue() + 999) / 1000)
	bytes := memoryQuantity.Value()
	memoryMiB := int((bytes + (1 << 20) - 1) >> 20)
	return cpus, memoryMiB, nil
}

func (b *nativeBackend) ensureTunnelLocked(ctx context.Context, record *nativeRecord, box *boxlite.Box) error {
	if b.tunnels[record.Request.Sandbox.SandboxID] != nil {
		return nil
	}
	execution, err := box.StartExecution(ctx, fastletinfra.SandboxTunnelContainerPath,
		[]string{
			"--listen", ":" + strconv.Itoa(int(record.Request.TunnelGuestPort)),
			"--credential", record.TunnelCredential,
		}, &boxlite.ExecutionOptions{})
	if err != nil {
		return err
	}
	b.tunnels[record.Request.Sandbox.SandboxID] = execution
	sandboxUID := record.Request.Sandbox.SandboxID
	go func() {
		waitCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, _ = execution.Wait(waitCtx)
		b.mu.Lock()
		if b.tunnels[sandboxUID] == execution {
			delete(b.tunnels, sandboxUID)
		}
		b.mu.Unlock()
		_ = execution.Close()
	}()
	return waitForTunnel(ctx, record.HostPort, record.TunnelCredential)
}

func waitForTunnel(ctx context.Context, port uint32, credential string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	healthPreamble, err := fastletnetwork.EncodeLocalForwardHealthPreamble(credential)
	if err != nil {
		return err
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	var lastErr error
	for {
		connection, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", address)
		if connection != nil {
			preambleErr := fastletnetwork.WriteLocalForwardPreamble(connection, healthPreamble)
			_ = connection.SetReadDeadline(time.Now().Add(time.Second))
			buffer := make([]byte, 1)
			_, readErr := connection.Read(buffer)
			_ = connection.Close()
			if preambleErr == nil && errors.Is(readErr, io.EOF) {
				return nil
			}
			lastErr = errors.Join(preambleErr, readErr)
		} else {
			lastErr = err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-probeCtx.Done():
			timer.Stop()
			return errors.Join(probeCtx.Err(), lastErr)
		case <-timer.C:
		}
	}
}

func (b *nativeBackend) prepareBundle(record *nativeRecord) error {
	if err := os.RemoveAll(record.BundleRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(record.BundleRoot, 0700); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(b.artifactRoot)
	if err != nil {
		return fmt.Errorf("resolve Infra artifact root: %w", err)
	}
	for _, artifact := range record.Request.Artifacts {
		source, err := filepath.EvalSymlinks(artifact.Source)
		if err != nil {
			return fmt.Errorf("resolve artifact source: %w", err)
		}
		if source != root && !strings.HasPrefix(source, root+string(os.PathSeparator)) {
			return fmt.Errorf("artifact source %q is outside the shared Infra store", artifact.Source)
		}
		cleanDestination := filepath.Clean(artifact.Destination)
		if cleanDestination != "/.fast" && !strings.HasPrefix(cleanDestination, "/.fast/") {
			return fmt.Errorf("artifact destination %q is outside /.fast", artifact.Destination)
		}
		relative := strings.TrimPrefix(cleanDestination, "/.fast")
		destination := filepath.Join(record.BundleRoot, relative)
		if err := copyArtifact(source, destination); err != nil {
			return err
		}
	}
	return nil
}

func copyArtifact(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact source %s is not a regular file", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	mode := info.Mode().Perm()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func wireBox(record *nativeRecord, info *boxlite.BoxInfo) boxlitewire.Box {
	return boxlitewire.Box{
		Sandbox: record.Request.Sandbox, BoxID: info.ID, PID: info.PID, Phase: string(info.State),
		CreatedAt: record.CreatedAt,
		Access: fastletnetwork.AccessDescriptor{
			Kind: fastletnetwork.AccessKindLocalForward, Address: net.JoinHostPort("127.0.0.1", strconv.Itoa(int(record.HostPort))),
			Credential: record.TunnelCredential,
		},
	}
}

func (b *nativeBackend) loadRecords() error {
	entries, err := os.ReadDir(b.metadataRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		file, err := os.Open(filepath.Join(b.metadataRoot, entry.Name()))
		if err != nil {
			return err
		}
		var record nativeRecord
		decodeErr := json.NewDecoder(file).Decode(&record)
		closeErr := file.Close()
		if decodeErr != nil || closeErr != nil {
			return errors.Join(decodeErr, closeErr)
		}
		if record.Version != boxlitestate.Version || record.SpecHash == "" || record.HostPort == 0 || record.HostPort > 65535 {
			return fmt.Errorf("invalid BoxLite metadata record %s", entry.Name())
		}
		if err := fastletnetwork.ValidateLocalForwardCredential(record.TunnelCredential); err != nil {
			return fmt.Errorf("invalid BoxLite tunnel credential for %s: %w", entry.Name(), err)
		}
		if err := validateEnsureRequest(record.Request); err != nil {
			return fmt.Errorf("invalid BoxLite metadata record %s: %w", entry.Name(), err)
		}
		sandboxUID := record.Request.Sandbox.SandboxID
		if entry.Name() != boxlitestate.RecordFileName(sandboxUID) ||
			record.Request.Sandbox.FastletPodUID != b.podUID ||
			record.Namespace != record.Request.Namespace {
			return fmt.Errorf("BoxLite metadata owner fence mismatch for %s", sandboxUID)
		}
		hash, err := ensureHash(record.Request)
		if err != nil || hash != record.SpecHash {
			return fmt.Errorf("BoxLite metadata hash mismatch for %s", sandboxUID)
		}
		if record.BundleRoot != b.expectedBundleRoot(sandboxUID, hash) {
			return fmt.Errorf("BoxLite metadata bundle fence mismatch for %s", sandboxUID)
		}
		b.records[sandboxUID] = &record
	}
	return nil
}

func (b *nativeBackend) persistRecord(record *nativeRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(b.metadataRoot, ".record-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, b.recordPath(record.Request.Sandbox.SandboxID))
}

func (b *nativeBackend) recordPath(sandboxUID string) string {
	return filepath.Join(b.metadataRoot, boxlitestate.RecordFileName(sandboxUID))
}

func (b *nativeBackend) expectedBundleRoot(sandboxUID, specHash string) string {
	return filepath.Join(b.bundleRoot, safePathSegment(sandboxUID), specHash)
}

func (b *nativeBackend) closeTunnelLocked(sandboxUID string) {
	if execution := b.tunnels[sandboxUID]; execution != nil {
		_ = execution.Close()
		delete(b.tunnels, sandboxUID)
	}
}

func ensureHash(request boxlitewire.EnsureRequest) (string, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func validateEnsureRequest(request boxlitewire.EnsureRequest) error {
	spec := request.Sandbox
	if request.Namespace == "" || spec.SandboxID == "" || spec.FastletPodUID == "" || spec.InstanceGeneration <= 0 || spec.AssignmentAttempt <= 0 {
		return errors.New("complete namespace and Sandbox owner fence are required")
	}
	if spec.Image == "" || spec.CPU == "" || spec.Memory == "" || request.TunnelGuestPort == 0 || request.TunnelGuestPort > 65535 {
		return errors.New("image, CPU, memory, and tunnel guest port are required")
	}
	if !hasArtifact(request.Artifacts, fastletinfra.SandboxTunnelContainerPath) {
		return errors.New("sandbox-tunnel artifact is required")
	}
	return nil
}

func hasArtifact(artifacts []boxlitewire.Artifact, destination string) bool {
	for _, artifact := range artifacts {
		if filepath.Clean(artifact.Destination) == destination {
			return true
		}
	}
	return false
}

func allocateHostPort() (uint32, error) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.Port <= 0 {
		return 0, errors.New("could not allocate a TCP host port")
	}
	return uint32(address.Port), nil
}

func recordIdentity(record *nativeRecord) string {
	return record.Request.Sandbox.SandboxID
}

func safePathSegment(value string) string {
	return boxlitestate.SafeSegment(value)
}

func mapNativeError(err error) error {
	if err == nil {
		return nil
	}
	var native *boxlite.Error
	if errors.As(err, &native) {
		switch native.Code {
		case boxlite.ErrNotFound:
			return notFound(native.Error())
		case boxlite.ErrAlreadyExists, boxlite.ErrInvalidState:
			return conflict(native.Error())
		case boxlite.ErrInvalidArgument, boxlite.ErrConfig:
			return invalid(native)
		case boxlite.ErrResourceExhausted, boxlite.ErrStorage, boxlite.ErrImage, boxlite.ErrNetwork,
			boxlite.ErrEngine, boxlite.ErrRpc, boxlite.ErrRpcTransport, boxlite.ErrPortal:
			return unavailableError(native)
		}
	}
	return internal(err)
}

func invalid(err error) error {
	return &boxlitesidecar.Error{Code: boxlitewire.ErrorInvalid, Message: err.Error(), Cause: err}
}
func notFound(message string) error {
	return &boxlitesidecar.Error{Code: boxlitewire.ErrorNotFound, Message: message}
}
func conflict(message string) error {
	return &boxlitesidecar.Error{Code: boxlitewire.ErrorConflict, Message: message}
}
func immutableSpecConflict(message string) error {
	return &boxlitesidecar.Error{Code: boxlitewire.ErrorImmutableSpecConflict, Message: message}
}
func unavailableError(err error) error {
	return &boxlitesidecar.Error{Code: boxlitewire.ErrorUnavailable, Message: err.Error(), Cause: err}
}
func internal(err error) error {
	return &boxlitesidecar.Error{Code: boxlitewire.ErrorInternal, Message: err.Error(), Cause: err}
}
