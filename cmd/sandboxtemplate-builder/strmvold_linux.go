//go:build linux

package main

// strmvold_linux.go drives the OCI image packaging pipeline (attach empty
// raw volume → byte-exact write → commit → push) against a strmvold daemon
// started as a child process. The builder talks to it over a private unix
// socket in the build workspace; the daemon's state (volume metadata,
// manifests, blobs) lives and dies with the build Pod.
//
// Depending on pkg/service (in-process embedding) would pull the private
// dadi-snapshotter / overlaybd-convert modules into the build graph; the
// gRPC client surface (api/strmvold) is the stable, dependency-light
// contract — the same one strmvolctl uses.
//
// Runtime prerequisites inside the builder container: the strmvold binary
// (streamingvolume release), the overlaybd toolchain
// (/opt/overlaybd/bin/overlaybd-commit and the ublk stack), the overlaybd
// api server on 127.0.0.1:9862 (see /etc/overlaybd/overlaybd.json),
// /dev/ublk-control from the host, and mkfs.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	rpc "gitlab.alibaba-inc.com/sbu/streamingvolume/api/strmvold"
	"gitlab.alibaba-inc.com/sbu/streamingvolume/pkg/labels"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"

	"k8s.io/klog/v2"
)

const (
	// strmvoldBin is the daemon binary expected in the builder image.
	strmvoldBin = "strmvold"

	// emptyVolumeRefPrefix mirrors the upstream service.EmptyImagePrefix:
	// the ref requesting a fresh empty (mkfs-formatted) writable volume.
	emptyVolumeRefPrefix = "snapshot.volume.base"

	// registryAuthEnv holds the docker config JSON ({"auths":{...}})
	// consumed as streamingvolume secret.type=dockerAuth. The controller
	// wiring for registrySecretRef lands separately; until then E2E and
	// local runs feed the variable directly.
	registryAuthEnv = "SANDBOX_TEMPLATE_REGISTRY_AUTH"

	// registryPlainHTTPEnv marks the target registry as plain-HTTP (E2E
	// with a local registry:2 container).
	registryPlainHTTPEnv = "SANDBOX_TEMPLATE_REGISTRY_PLAINHTTP"

	// blockDriverEnv selects the streamingvolume block driver: "ublk"
	// (default, kernel >= 5.19, needs the overlaybd api server) or "tcmu"
	// (kernel >= 4.x, needs target_core_user + configfs and the
	// overlaybd-tcmu handler — the fallback for older kernels).
	blockDriverEnv = "SANDBOX_TEMPLATE_BLOCKDRIVER"

	// strmvoldStartupTimeout bounds how long we wait for the daemon socket.
	strmvoldStartupTimeout = 30 * time.Second
)

// strmvoldSession owns the child daemon: its process, socket, and client.
type strmvoldSession struct {
	client   rpc.StreamingVolumeServiceClient
	conn     *grpc.ClientConn
	root     string
	command  *exec.Cmd
	socket   string
	shutdown func()
}

// startStrmvold writes the daemon config rooted in the build workspace,
// starts the binary, and waits for its socket to accept a Ping.
func startStrmvold(ctx context.Context, workdir string) (*strmvoldSession, error) {
	root := filepath.Join(workdir, "strmvol")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	socket := filepath.Join(root, "strmvold.sock")
	_ = os.Remove(socket)

	blockDriver := os.Getenv(blockDriverEnv)
	if blockDriver == "" {
		blockDriver = "ublk"
	}
	if blockDriver != "ublk" && blockDriver != "tcmu" {
		return nil, fmt.Errorf("invalid %s=%q: must be ublk or tcmu", blockDriverEnv, blockDriver)
	}

	configPath := filepath.Join(root, "config.json")
	bootConfig := map[string]any{
		"root":    root,
		"address": "unix://" + socket,
		"log":     map[string]any{"level": "info", "mode": "stdout"},
		"storageDriver": map[string]any{
			"overlaybd": map[string]any{"blockDriver": blockDriver, "autoCompactLayers": 150},
		},
	}
	configBytes, err := json.Marshal(bootConfig)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write strmvold config: %w", err)
	}

	binary, err := exec.LookPath(strmvoldBin)
	if err != nil {
		return nil, fmt.Errorf("strmvold binary not found in builder image: %w", err)
	}
	command := exec.Command(binary, configPath)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start strmvold: %w", err)
	}

	session := &strmvoldSession{root: root, command: command, socket: socket}
	session.shutdown = func() {
		if command.Process != nil {
			_ = command.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() { _, _ = command.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = command.Process.Kill()
			}
		}
		_ = os.Remove(socket)
	}

	if err := waitForSocket(ctx, socket, strmvoldStartupTimeout); err != nil {
		session.shutdown()
		return nil, err
	}
	conn, err := grpc.NewClient("unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		session.shutdown()
		return nil, fmt.Errorf("dial strmvold: %w", err)
	}
	session.conn = conn
	session.client = rpc.NewStreamingVolumeServiceClient(conn)

	pingCtx, cancel := context.WithTimeout(ctx, strmvoldStartupTimeout)
	defer cancel()
	if _, err := session.client.Ping(pingCtx, &rpc.PingRequest{Id: "builder"}); err != nil {
		session.close()
		return nil, fmt.Errorf("ping strmvold: %w", err)
	}
	return session, nil
}

// close tears the session down (conn first, then the daemon).
func (s *strmvoldSession) close() {
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.shutdown()
}

// waitForSocket polls until the socket file exists and accepts a dial.
func waitForSocket(ctx context.Context, socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("strmvold socket %s not ready within %s", socket, timeout)
}

// stagePublishOCIImages packs rootfs and memory as OverlayBD OCI images and
// returns their digest-pinned references. It replaces the legacy
// overlaybd-import-raw packaging for overlaybd builds that declare an
// output registry. The tag names both derived image refs; callers pick it
// from the build identity (pipeline) or an explicit flag (oci-publish).
// skipPush keeps the committed artifacts in the session root (manifest +
// layer blob) without uploading — the local-only mode of oci-publish.
func stagePublishOCIImages(ctx context.Context, spec apiv1alpha2.SandboxTemplateSpec, workdir, rootfs, memory, tag string, skipPush bool) (refs ociImageRefs, err error) {
	if tag == "" {
		tag = buildShortID(workdir)
	}
	rootfsGiB, err := sizeGiB(spec.Output.RootfsSize)
	if err != nil {
		return refs, fmt.Errorf("rootfs size: %w", err)
	}
	memoryGiB, err := sizeGiB(spec.Machine.Memory)
	if err != nil {
		return refs, fmt.Errorf("memory size: %w", err)
	}

	session, err := startStrmvold(ctx, workdir)
	if err != nil {
		return refs, err
	}
	defer session.close()

	auth := os.Getenv(registryAuthEnv)
	registry := &rpc.RegistryConfig{PlainHTTP: os.Getenv(registryPlainHTTPEnv) == "1"}
	rootfsRef, memRef := ociDerivedRefs(spec.Output.Registry, tag)

	for _, artifact := range []struct {
		name     string
		source   string
		target   string
		sizeGiB  int
		refField *string
	}{
		{"memory", memory, memRef, memoryGiB, &refs.Memory},
		{"rootfs", rootfs, rootfsRef, rootfsGiB, &refs.Rootfs},
	} {
		started := time.Now()
		digestRef, publishErr := publishBlockImage(ctx, session, artifact.name, artifact.source, artifact.target, artifact.sizeGiB, auth, registry, skipPush)
		if publishErr != nil {
			return refs, fmt.Errorf("publish %s image: %w", artifact.name, publishErr)
		}
		*artifact.refField = digestRef
		logOCIPublish(artifact.name, digestRef, time.Since(started).Milliseconds())
	}
	return refs, nil
}

// publishBlockImage runs the per-artifact pipeline: attach an empty raw
// volume, write the image byte-exactly onto the device, commit the upper
// layer into an OCI manifest, push it (unless skipPush), and return the
// digest-pinned ref.
func publishBlockImage(ctx context.Context, session *strmvoldSession, name, sourcePath, targetRef string, sizeGiB int, auth string, registry *rpc.RegistryConfig, skipPush bool) (string, error) {
	if sizeGiB <= 0 {
		return "", fmt.Errorf("invalid %s volume size %dG", name, sizeGiB)
	}
	volumeID := fmt.Sprintf("build-%s-%d", name, time.Now().UnixNano())

	attachResp, err := session.client.Attach(ctx, &rpc.AttachVolumeRequest{
		Id:       volumeID + "-attach",
		ImageRef: emptyVolumeRefPrefix,
		Registry: registry,
		Params: map[string]string{
			labels.VolumeType:  "oci.overlaybd",
			labels.VolumeID:    volumeID,
			labels.VirtualSize: strconv.Itoa(sizeGiB),
		},
	})
	if err != nil {
		return "", fmt.Errorf("attach empty volume: %w", err)
	}
	if attachResp.GetStatus() != 0 {
		return "", fmt.Errorf("attach empty volume: %s", attachResp.GetMessage())
	}
	device := attachResp.GetMount().GetSource()
	if device == "" {
		return "", fmt.Errorf("attach empty volume: no device in response")
	}
	detached := false
	defer func() {
		if detached {
			return
		}
		if _, detachErr := session.client.Detach(ctx, &rpc.DetachVolumeRequest{
			Id: volumeID,
			Params: map[string]string{
				labels.VolumeID: volumeID,
			},
		}); detachErr != nil {
			klog.V(2).InfoS("detach volume failed", "volume", volumeID, "err", detachErr)
		}
	}()

	if err := writeImageToDevice(sourcePath, device); err != nil {
		return "", fmt.Errorf("write %s onto device: %w", name, err)
	}

	// Detach before commit: commit seals the upper layer on disk; keeping
	// the device open across commit is unnecessary and would hold the ublk
	// device for the whole push.
	if _, err := session.client.Detach(ctx, &rpc.DetachVolumeRequest{
		Id: volumeID,
		Params: map[string]string{labels.VolumeID: volumeID},
	}); err != nil {
		return "", fmt.Errorf("detach %s volume: %w", name, err)
	}
	detached = true

	commitParams := map[string]string{labels.VolumeID: volumeID}
	if auth != "" {
		commitParams[labels.SecretType] = labels.DockerAuth
		commitParams[labels.SecretData] = auth
	}
	if commitResp, commitErr := session.client.Commit(ctx, &rpc.CommitImageRequest{
		Id:        volumeID + "-commit",
		TargetRef: targetRef,
		Params:    commitParams,
	}); commitErr != nil {
		return "", fmt.Errorf("commit %s image: %w", name, commitErr)
	} else if commitResp.GetStatus() != 0 {
		return "", fmt.Errorf("commit %s image: %s", name, commitResp.GetMessage())
	}

	digest, err := ociManifestDigest(session.root, targetRef)
	if err != nil {
		return "", err
	}

	if !skipPush {
		pushParams := map[string]string{labels.VolumeID: volumeID}
		if auth != "" {
			pushParams[labels.SecretType] = labels.DockerAuth
			pushParams[labels.SecretData] = auth
		}
		if pushResp, pushErr := session.client.Push(ctx, &rpc.PushImageRequest{
			Id:        volumeID + "-push",
			TargetRef: targetRef,
			Registry:  registry,
			Params:    pushParams,
		}); pushErr != nil {
			return "", fmt.Errorf("push %s image: %w", name, pushErr)
		} else if pushResp.GetStatus() != 0 {
			return "", fmt.Errorf("push %s image: %s", name, pushResp.GetMessage())
		}
	}

	return ociDigestPin(targetRef, digest), nil
}
