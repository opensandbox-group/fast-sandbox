---
title: Firecracker memory reclamation
description: Default free-page reporting with existing Fastlet memory observation and admission.
---

# Firecracker memory reclamation

Free-page reporting (FPR) returns unused guest pages to the host while the sandbox keeps running. It does not change configured RAM, static resource commitments, or checkpoint pause/resume. New object-store golden snapshots enable FPR by default without a SandboxTemplate field. Existing snapshots retain their saved device state; rebuild a template to acquire reporting. OCI templates currently use the separate fsb-vm build path, which does not configure FPR.

The object-store builder installs a zero-sized balloon with FPR before boot. Balloon statistics polling is disabled; Fastlet continues to observe host cgroup memory usage. The Firecracker binary must support `free_page_reporting`; an unsupported VMM fails the build. The guest needs built-in VIRTIO_BALLOON and PAGE_REPORTING. A custom kernel without either interface can still boot: its console reports `SANDBOX_FREE_PAGE_REPORTING_UNAVAILABLE`, and the builder logs a warning.

The builder logs the detected guest kernel support and records it in `snapshot-phases.json` and the manifest's optional `freePageReporting` field. New object-store builds record `true` when the required guest kernel interfaces are detected and `false` otherwise; an absent field means the result was not recorded. This is build provenance, not a runtime toggle or a reclamation metric. Snapshot and checkpoint manifests inherit it, while Firecracker preserves the balloon device and guest state. Reporting cannot be disabled on a live VM; rollback selects an older template.

## Feedback to Fastlet and FastPath

FPR releases backing pages from the Firecracker process. For ResourceAware direct Firecracker, the resulting change is visible in the Pod's shared `fast-sandbox` cgroup. Fastlet's memory observer reads cgroup usage, PSI, OOM events, and host available memory. It uses watermarks and recovery hysteresis to update resource admission state. Fastlet reports that state and its revision to FastPath through heartbeats and Create responses.

This feature adds no separate pressure policy, admission gate, or Fastlet-to-FastPath protocol. Configure ResourceAware Pools and the existing memory observer as described in [Resource-aware overcommit tuning](resource-aware-overcommit-tuning.md). LegacySlot Pools retain their existing admission behavior. Guest free-memory statistics are not admission headroom and do not change the static commitment ledger.

Compare old snapshots and reporting snapshots at identical density. Measure cgroup usage, PSI, OOMs, aggregate VMM PSS, CPU cost, and tail latency after idle. Verify process and file state through snapshot and cross-host resume. Do not raise overcommit merely because a guest reports free pages or a single reclaim sample succeeds.

## FPR and deployment density

FPR is enabled by default for supported new object-store snapshots, but it does not automatically increase deployment density. It reduces physical memory use without changing VM memory limits, Kubernetes requests, static commitments, or available slots. To place more VMs per node while retaining larger per-VM limits, explicitly tune the Pool's memory overcommit ratio and slot count, then verify the derived runtime budget. A fully committed Pool cannot admit another VM merely because FPR returned pages to the host.

Whether to enable memory overcommit depends on the workload's concurrent memory demand. FPR can help workloads that release memory between independent bursts, but it cannot reclaim live working sets when many VMs grow at the same time. Follow the [workload selection and OOM precautions](resource-aware-overcommit-tuning.md#choose-memory-overcommit-by-workload) before increasing density; default FPR does not make a particular overcommit ratio safe.

## Compatibility and limits

Upgrade the object-store builder and rebuild templates; no new CRD field is required. Existing Firecracker runtimes that support reporting restore the saved balloon device state. Verify the deployed Firecracker version and storage backend before rollout. Existing Fastlet memory admission and FastPath feedback remain responsible for placement and admission decisions.

Only pages already freed by the guest are reported. FPR does not actively evict cold file cache, reclaim live anonymous working sets, configure DAMON, or automatically pause sandboxes. A resident kernel, browser, or JVM can retain its working set during inactivity. Shared pmem/DAX layers are a separate storage optimization.

Native pause removes the VMM but can leave checkpoint file cache charged to the Fastlet cgroup. FPR does not evict host checkpoint cache, so a fall in VMM PSS alone does not establish a fall in total cgroup memory. Idle detection and automatic pause scheduling remain the caller's responsibility.
