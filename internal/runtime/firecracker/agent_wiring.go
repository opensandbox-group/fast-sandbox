package firecracker

// agent_wiring.go connects the driver to the node-level firecracker
// runtime-agent: PullImage proxies to PinImage, ProbeCapabilities gates on
// the agent Health, and DeleteSandbox releases the lease and unpins the
// image. When no socket is configured the driver stays in local mode; when
// the agent is unreachable the driver falls back to the local cache (the
// agent being absent must not break warm images).

import (
	"context"
	"errors"
	"time"

	"k8s.io/klog/v2"
)

// SetFastletPodUID records the pod identity attached to runtime-agent
// requests (identity headers).
func (d *Driver) SetFastletPodUID(podUID string) {
	d.mu.Lock()
	d.podUID = podUID
	d.mu.Unlock()
}

// SetAgentSocket configures the runtime-agent UDS socket. An empty path
// switches the driver back to local mode (no remote pulls, no lease RPCs).
func (d *Driver) SetAgentSocket(socketPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.agentSocket = socketPath
	if socketPath == "" {
		d.newAgentClient = nil
		d.agentClient = nil
		return
	}
	d.newAgentClient = func(path string) (AgentClient, error) {
		d.mu.RLock()
		namespace := d.namespace
		podUID := d.podUID
		d.mu.RUnlock()
		return NewAgentClient(path, namespace, podUID)
	}
}

// agentClientOrNil returns the lazily built agent client, or nil in local
// mode. The build runs once; the cached instance is reused.
func (d *Driver) agentClientOrNil() (AgentClient, error) {
	d.mu.RLock()
	client := d.agentClient
	newClient := d.newAgentClient
	socketPath := d.agentSocket
	d.mu.RUnlock()
	if client != nil {
		return client, nil
	}
	if newClient == nil {
		return nil, nil
	}
	built, err := newClient(socketPath)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.agentClient == nil {
		d.agentClient = built
	}
	client = d.agentClient
	d.mu.Unlock()
	return client, nil
}

// warmPullRequestID is the idempotency key of the warm pull of an image,
// scoped to the fastlet Pod: retries of the same Pod replay the first pin
// instead of double-counting, while a second fastlet Pod warming the same
// image on the node (pool scale-out) gets its own key — the agent journal
// rejects a request id committed by a different pod UID.
func (d *Driver) warmPullRequestID(image string) string {
	if d.podUID == "" {
		return "warm-pull-" + imageKey(image)
	}
	return "warm-pull-" + d.podUID + "-" + imageKey(image)
}

// rememberLease records the runtime-agent lease of a Sandbox (populated
// when a later phase wires LeaseDevices into EnsureSandbox).
func (d *Driver) rememberLease(sandboxID, leaseID string) {
	d.mu.Lock()
	if d.sandboxLeases == nil {
		d.sandboxLeases = make(map[string]string)
	}
	d.sandboxLeases[sandboxID] = leaseID
	d.mu.Unlock()
}

// leaseForSandbox returns the recorded lease of a Sandbox.
func (d *Driver) leaseForSandbox(sandboxID string) (string, bool) {
	d.mu.RLock()
	leaseID, ok := d.sandboxLeases[sandboxID]
	d.mu.RUnlock()
	return leaseID, ok
}

// forgetLease drops the recorded lease of a Sandbox.
func (d *Driver) forgetLease(sandboxID string) {
	d.mu.Lock()
	delete(d.sandboxLeases, sandboxID)
	d.mu.Unlock()
}

// releaseAgentSandbox unpins the Sandbox image and releases its lease (if
// any) on the runtime-agent. Failures never fail the delete; an unreachable
// agent leaves the lease to the agent's journaled recovery.
func (d *Driver) releaseAgentSandbox(ctx context.Context, sandboxID, image string) {
	client, err := d.agentClientOrNil()
	if err != nil || client == nil {
		return
	}
	if leaseID, ok := d.leaseForSandbox(sandboxID); ok {
		if err := client.ReleaseDevices(ctx, "release-"+leaseID, leaseID); err != nil && !errorsIsAgentUnreachable(err) {
			klog.V(2).InfoS("firecracker runtime-agent ReleaseDevices failed", "sandboxID", sandboxID, "err", err)
		}
		d.forgetLease(sandboxID)
	}
	if image != "" {
		if err := client.UnpinImage(ctx, "unpin-"+sandboxID, image); err != nil && !errorsIsAgentUnreachable(err) {
			klog.V(2).InfoS("firecracker runtime-agent UnpinImage failed", "sandboxID", sandboxID, "err", err)
		}
	}
}

// agentHealthTimeout bounds the ProbeCapabilities health call: a stuck
// agent must not hang the capability probe.
const agentHealthTimeout = 10 * time.Second

func errorsIsAgentUnreachable(err error) bool {
	return err != nil && errors.Is(err, errAgentUnreachable)
}
