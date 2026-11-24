# Component file logging and rotation

First-party Kubernetes services write application logs through `k8s.io/klog/v2`, the shared `internal/componentlog` configuration loader, and `gopkg.in/natefinch/lumberjack.v2` v2.2.1. One YAML schema controls file output and rotation for every covered component. This is component logging, not a policy for Sandbox workloads, VM logs, or guest serial output.

## Covered processes and filenames

Each process writes `<component>.log` under the configured `directory`. Filenames are fixed; there is no filename override. The controller binary selects its filename from its role.

| Process | Active filename |
|---|---|
| `controller --role=controller` | `controller.log` |
| `controller --role=fastpath` | `fastpath.log` |
| `controller --role=all` | `all.log` |
| `fastlet` | `fastlet.log` |
| `fastlet-proxy` | `fastlet-proxy.log` |
| `sandbox-proxy` | `sandbox-proxy.log` |
| `firecracker-runtime` (runtime agent) | `firecracker-runtime.log` |
| `janitor` | `janitor.log` |
| `sandboxtemplate-builder` main pipeline | `sandboxtemplate-builder.log` |

Excluded are the `fastctl` and `fsb-vm` CLIs, `sandbox-init`, experimental `fsb-guest-control`, test fixtures, and the builder's snapshot-stage helper (`sandbox-snapshot-stage` or the `snapshot-stage` subcommand). External OverlayBD, containerd, Execd, and Dart processes retain their own logging behavior. The runtime agent's first-party Dart supervisor messages are included in `firecracker-runtime.log`, but the Dart child's stdout/stderr are not. Per-VM Firecracker log files and guest serial files are also outside this policy; rotating `firecracker-runtime.log` does not rotate them.

## Configure the shared policy

The default file is `/etc/fast-sandbox/logging/logging.yaml`. Save the following complete configuration as a local `logging.yaml`; these values are also the built-in defaults:

```yaml
directory: /var/log/fast-sandbox
maxSizeMB: 100
maxBackups: 10
maxAgeDays: 7
compress: true
alsoStderr: false
```

`maxSizeMB` is measured in MiB and must be positive. `maxBackups` and `maxAgeDays` must be nonnegative integers: `maxBackups: 0` removes the backup-count cap, and `maxAgeDays: 0` removes the backup-age cap. A day means 24 hours, not a calendar-day boundary. Setting both caps to zero gives unbounded historical retention. See the [shared configuration reference](../reference/configuration.md#shared-component-logging) for the field contract.

The sole configuration-path override is `FAST_SANDBOX_LOG_CONFIG`. There are no per-component logging environment-variable sets and no logging fields in `agent.yaml` or Fastlet settings. An absent default file uses the same built-in **file logging** defaults; it does not fall back to stderr. An explicitly selected missing file, invalid configuration, or unwritable log output fails process startup with a stderr diagnostic. An unreadable configuration file is an error, not an absent-file fallback.

The klog verbosity flags `-v` and `-vmodule` retain their usual behavior across all covered binaries. Legacy klog output flags are no longer exposed: `-log_file`, `-log_dir`, `-log_file_max_size`, `-logtostderr`, `-alsologtostderr`, `-stderrthreshold`, `-one_output`, and `-skip_log_headers`. Remove those flags from custom launch commands when adopting this policy; use the shared YAML instead of a second output configuration mechanism.

## Deploy in each namespace

The optional ConfigMap is named `fast-sandbox-logging`, with the key `logging.yaml`. The `component-log-config` volume mounts it read-only at `/etc/fast-sandbox/logging`. The samples are `config/logging/configmap.yaml` and `config/logging/kustomization.yaml`; `config/default` includes them, and the `config/dev` and `config/all-in-one` overlays inherit them.

ConfigMaps are namespaced. Use the same name and schema in every namespace containing covered Pods; there is no automatic copying or inheritance from `fast-sandbox-system`. The controller injects logging mounts into dynamically created Fastlet, Fastlet Proxy, and SandboxTemplate builder Pods, but operators must create the ConfigMap in each tenant namespace to apply a configured policy there. Without it, those processes use the built-in defaults.

For an existing namespace, create or update the ConfigMap from the standalone YAML above, not from a ConfigMap manifest:

```bash
kubectl -n fast-sandbox-system create configmap fast-sandbox-logging \
  --from-file=logging.yaml=/path/to/logging.yaml \
  --dry-run=client -o yaml | kubectl apply -f -

# Repeat for every tenant namespace that runs Fastlet or builder Pods.
kubectl -n tenant-a create configmap fast-sandbox-logging \
  --from-file=logging.yaml=/path/to/logging.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
```

The supplied Pod layout mounts a disk-backed `emptyDir` named `component-logs` at `/var/log/fast-sandbox`. Fastlet and Fastlet Proxy share that directory but write distinct files, as do the runtime agent and its janitor sidecar. Each Pod has its own volume, so replicas do not share files.

Changing `directory` requires a matching writable volume mount accessible to the process identity. The built-in mounts stay at `/var/log/fast-sandbox`; changing the ConfigMap does not change a Pod's mounts. Coordinate nondefault paths with operator-managed deployment wiring, including controller-generated Pods, rather than putting logging settings in a Pool's Fastlet settings or the runtime agent configuration.

## Apply changes safely

Each process reads the logging file only at startup. ConfigMap projection updates do not hot-reload logging and do not automatically roll Pods. Restart affected processes using the normal Deployment, DaemonSet, or Pool lifecycle after the desired file is available. Newly started builder pipelines read the current configuration; an existing pipeline keeps its startup configuration.

For example, after planning availability and collecting any logs that must be retained, restart a controller Deployment:

```bash
kubectl -n fast-sandbox-system rollout restart deployment/fast-sandbox-controller
kubectl -n fast-sandbox-system rollout status deployment/fast-sandbox-controller
```

**Do not casually delete live Fastlet Pods to apply logging changes.** Fastlet Pod loss destroys its live Sandboxes. Use the normal Pool drain/replacement lifecycle and plan workload disruption; logging configuration does not provide live migration or instance survival. Pod replacement also discards the default log volume, so collect needed logs first. To revert a policy, restore the previous YAML in each affected namespace and repeat the same controlled lifecycle.

## Inspect files and stderr

Use `kubectl exec` against a running container to inspect component files. `kubectl logs` reads container stdout/stderr, not these files: by default it does not show the component application log stream. Set `alsoStderr: true` and restart to mirror that stream to stderr while keeping file logging enabled. Logging initialization and runtime write failures produce stderr diagnostics even when mirroring is disabled; unrelated subprocess output can also appear in container logs.

The following checks use the supplied deployment/container names. Replace `tenant-a`, set `FASTLET_POD` and `BUILDER_POD` to actual Pod names, and adjust the Fastlet container name if your template does not name it `fastlet`. Run only the topology you deploy; the `all` role replaces the separate controller and Fast-Path roles. Deployment and DaemonSet targets select one Pod, so repeat with explicit Pod names to inspect every replica or node. The commands require `sh` and `ls` in the selected running container.

```bash
# Split control plane.
kubectl -n fast-sandbox-system exec deployment/fast-sandbox-controller -c manager -- sh -c 'ls -lh /var/log/fast-sandbox/controller.log'
kubectl -n fast-sandbox-system exec deployment/fast-sandbox-fastpath -c manager -- sh -c 'ls -lh /var/log/fast-sandbox/fastpath.log'

# Alternative: all-in-one control plane.
kubectl -n fast-sandbox-system exec deployment/fast-sandbox-controller -c manager -- sh -c 'ls -lh /var/log/fast-sandbox/all.log'

# Tenant Fastlet and proxy, and the shared Sandbox Proxy.
kubectl -n tenant-a exec "$FASTLET_POD" -c fastlet -- sh -c 'ls -lh /var/log/fast-sandbox/fastlet.log'
kubectl -n tenant-a exec "$FASTLET_POD" -c fastlet-proxy -- sh -c 'ls -lh /var/log/fast-sandbox/fastlet-proxy.log'
kubectl -n fast-sandbox-system exec deployment/fast-sandbox-proxy -c proxy -- sh -c 'ls -lh /var/log/fast-sandbox/sandbox-proxy.log'

# Firecracker runtime agent and colocated janitor.
kubectl -n fast-sandbox-system exec daemonset/firecracker-runtime -c firecracker-agent -- sh -c 'ls -lh /var/log/fast-sandbox/firecracker-runtime.log'
kubectl -n fast-sandbox-system exec daemonset/firecracker-runtime -c janitor -- sh -c 'ls -lh /var/log/fast-sandbox/janitor.log'

# Alternative janitor deployment for containerd-based runtimes.
kubectl -n fast-sandbox-system exec daemonset/fast-sandbox-janitor -c janitor -- sh -c 'ls -lh /var/log/fast-sandbox/janitor.log'

# Inspect the builder while its main pipeline is still running.
kubectl -n tenant-a exec "$BUILDER_POD" -c build -- sh -c 'ls -lh /var/log/fast-sandbox/sandboxtemplate-builder.log'
```

For recent entries, backups, or logging diagnostics:

```bash
kubectl -n tenant-a exec "$FASTLET_POD" -c fastlet -- sh -c 'tail -n 100 /var/log/fast-sandbox/fastlet.log'
kubectl -n tenant-a exec "$FASTLET_POD" -c fastlet -- sh -c 'ls -lh /var/log/fast-sandbox/'
kubectl -n tenant-a logs "$FASTLET_POD" -c fastlet
# Previous container stderr, when a previous instance exists after a restart.
kubectl -n tenant-a logs "$FASTLET_POD" -c fastlet --previous
```

A completed builder container cannot be inspected with `kubectl exec`; arrange log collection while it runs, before its Pod is cleaned up. Missing files should prompt checks of the selected role, Pod/container, configured directory, mounted file, and stderr diagnostics, not an assumption that logs moved to `kubectl logs`.

## Rotation, retention, and disk budget

When a write would exceed `maxSizeMB`, rotation renames the active file with a UTC timestamp and recreates `<component>.log` for subsequent writes. Gzip compression and backup cleanup are asynchronous. For example, a Fastlet backup uses the form `fastlet-<UTC timestamp>.log`, with `.gz` added when compression completes.

Backup count and age cleanup is triggered when opening or rotating the log, not by a guaranteed daily wall-clock sweep. Age retention applies to timestamped backups, not the active file. Quiet processes can therefore keep an old active file or expired backups until the next relevant open/rotation. Do not use `maxAgeDays` as a precise deletion deadline.

With a nonzero backup-count cap, budget approximately `(maxBackups + 1) * maxSizeMB` MiB per component for uncompressed files, plus transient compression/cleanup space. The defaults are about 1,100 MiB per component before transient overhead; a Fastlet/proxy pair or runtime-agent/janitor pair can each need roughly twice that per Pod. Age cleanup and compression may reduce steady-state usage, but neither makes this estimate a hard instantaneous quota. Multiply by replicas and include other ephemeral storage consumers. With `maxBackups: 0`, there is no count-based size bound; with both retention caps zero, history is unbounded.

Only one process may own and rotate a file. Do not point multiple processes or replicas at the same component file, and do not run external `logrotate` on these files. Collectors should follow rename/recreate rotation without independently rotating or truncating the source.

## Persistence and failure behavior

The default `emptyDir` survives a container restart within the same Pod, but not Pod deletion, replacement, or node loss. It is not persistent log storage. For durable history, deploy a collector that can read this Pod-local volume and ship logs off-node, or provide operator-managed PVC or host storage with an appropriate retention and recovery plan. A collector that only reads container stdout/stderr will not collect file-only logs. Preserve isolated directories per Pod/process when changing storage; a shared PVC or host directory must not make replicas rotate the same filename. Host storage alone does not guarantee survival of node loss.

Writes are synchronous and unbuffered at the application logging layer, but there is no per-write `fsync` guarantee. Runtime write failures produce stderr diagnostics rather than silent success; monitor stderr and disk availability even when application logs are file-only. Normal exit closes the writer. At fatal termination or `os.Exit`, completed file writes have already been handed to the operating system, but this does not guarantee crash-durable storage. Asynchronous compression may remain incomplete after abrupt exit.

For message structure, lifecycle identity, traces, and metrics, see [Observability](observability.md).
