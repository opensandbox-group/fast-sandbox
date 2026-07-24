# BoxLite integration

BoxLite is a strategic RuntimeDriver integration, but the built-in `boxlite` profile remains fail closed. The existing sidecar path proves the control boundary; it does not establish production support.

## Architecture

```text
Fast Sandbox control plane
  -> RuntimeProfile(runtime=boxlite)
  -> Fastlet BoxLiteDriver
  -> versioned Pod-local Unix socket
  -> boxlite-runtime sidecar
  -> BoxLite native SDK and runtime
  -> one Box per Sandbox UID
```

Fastlet stays pure Go. Native libraries, CGO, KVM, gvproxy, and persistent BoxLite state remain isolated in the sidecar. BoxLite is not represented as a containerd runtime handler.

## Implemented boundary

The repository contains:

- a pure-Go BoxLiteDriver client;
- a versioned sidecar wire protocol;
- Sandbox and Fastlet identity fencing;
- artifact-volume Infra delivery;
- a credential-protected guest tunnel and LocalForward descriptor;
- List, Inspect, Delete, recovery, and image-cache interfaces;
- a NodeJanitor backend;
- capability negotiation and fail-closed Pool readiness.

## Resource enforcement gap

The SandboxPool resource profile requires CPU, memory, and PID limits that a guest-root workload cannot weaken.

The integrated BoxLite API can express vCPU and memory values but does not expose a complete host-enforced contract equivalent to Fast Sandbox's requirements, especially PID control and effective-limit inspection.

Guest-only cgroup configuration is insufficient because a privileged workload may modify or leave that cgroup. Removing user capabilities to compensate would silently change OCI image semantics.

Production support requires a versioned create-time resource API that:

- expresses fractional CPU, memory, and PIDs;
- applies before the user process starts;
- is enforced by the host/runtime boundary;
- returns effective values through Inspect or Stats;
- survives sidecar and Fastlet recovery;
- passes guest-root escape tests.

## Network tunnel gap

The compatibility path allocates one Pod-local port per Box, maps it to `sandbox-tunnel` in the guest, and uses a random credential in the tunnel preamble.

It preserves the external "any private target port" contract, but it adds:

- a Pod-local port lease per Box;
- a wider gvproxy listener guarded by an application credential;
- coupled recovery of port, credential, and relay state;
- an extra capacity and conntrack dimension.

The preferred upstream contract is a per-Box native stream:

```text
Dial(boxID, network, targetHost, targetPort) -> bidirectional stream
```

or a versioned Unix-socket/FD tunnel. It must define cancellation, half-close, backpressure, long-lived streaming, identity fencing, and sidecar restart behavior.

## Kubernetes validation gap

Production support also requires evidence for:

- observed source addresses and NAT;
- CNI NetworkPolicy behavior;
- DNS and conntrack behavior;
- Fastlet process, sidecar, Pod, and node restart boundaries;
- state-root and lock recovery;
- Janitor concurrency;
- multiple Boxes using the same guest port;
- namespace and Pool isolation.

These properties cannot be inferred only from gvproxy source code.

## Upstream collaboration priorities

The highest-value BoxLite discussions are:

1. host-enforced ResourceLimits;
2. a per-Box native stream/tunnel;
3. stable List, Inspect, GetOrCreate, Reconnect, and ForceRemove semantics;
4. image/template prewarming and observable cache hit/miss;
5. Kubernetes network and restart support;
6. versioned capability negotiation;
7. signed, compatible native and guest artifacts;
8. Infra Component delivery before the user entrypoint.

Fast Sandbox can contribute Kubernetes/KVM test environments, resource escape tests, stream stress tests, sidecar recovery, fencing, and OpenSandbox Execd integration cases.

## Production completion criteria

The profile can become Ready only when:

- guest root cannot bypass resource limits;
- arbitrary target ports use an identity-bound native stream;
- Kubernetes network behavior has real evidence;
- Fastlet, sidecar, Pod, and node failures have defined recovery;
- Infra Components start and become ready before route publication;
- cache/prewarm inventory integrates with heartbeat and Top-K;
- every claimed capability has positive and negative remote E2E evidence.

Until then:

```bash
make e2e SUITE=runtime RUNTIME=boxlite
```

proves the fail-closed capability boundary rather than BoxLite availability.
