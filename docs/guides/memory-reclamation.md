---
title: Firecracker memory reclamation
description: Default free-page reporting with existing Fastlet memory observation and admission.
---

# Firecracker memory reclamation

Free-page reporting (FPR) returns unused guest pages to the host while the sandbox keeps running. It does not change configured RAM, static resource commitments, or checkpoint pause/resume. New golden snapshots (both object-store and OCI/OverlayBD builds via `fsb-vm build`) enable FPR by default without a SandboxTemplate field. Existing snapshots retain their saved device state; rebuild a template to acquire reporting.

Both the object-store builder and `fsb-vm build` install a zero-sized balloon with FPR before boot. Balloon statistics polling is disabled; Fastlet continues to observe host cgroup memory usage. The Firecracker binary must support `free_page_reporting`; an unsupported VMM fails the build. The guest needs built-in VIRTIO_BALLOON and PAGE_REPORTING. A custom kernel without either interface can still boot: its console reports `SANDBOX_FREE_PAGE_REPORTING_UNAVAILABLE`, and the builder logs a warning. Standalone `fsb-vm build` invocations that supply a custom `--init-script-file` must emit `SANDBOX_FREE_PAGE_REPORTING_READY` on the serial console to record guest support as `true`.

The builder logs the detected guest kernel support and records it in the manifest's optional `freePageReporting` field (object-store builds also record it in `snapshot-phases.json`). New builds record `true` when the required guest kernel interfaces are detected and `false` otherwise; an absent field means the result was not recorded. This is build provenance, not a runtime toggle or a reclamation metric. Snapshot and checkpoint manifests inherit it, `RootfsOnly` cold boots attach the FPR balloon when `freePageReporting` is `true`, and Firecracker preserves the saved balloon device and guest state on Full restores. Reporting cannot be disabled on a live VM; rollback selects an older template.

## Feedback to Fastlet and FastPath

FPR releases backing pages from the Firecracker process. For ResourceAware direct Firecracker, the resulting change is visible in the Pod's shared `fast-sandbox` cgroup. Fastlet's memory observer reads cgroup usage, PSI, OOM events, and host available memory. Cgroup usage excludes clean, unmapped page cache that the kernel can drop without writeback, so cache left by checkpoint and restore I/O does not block admission. It uses watermarks and recovery hysteresis to update resource admission state. Fastlet reports that state and its revision to FastPath through heartbeats and Create responses.

This feature adds no separate pressure policy, admission gate, or Fastlet-to-FastPath protocol. Configure ResourceAware Pools and the existing memory observer as described in [Resource-aware overcommit tuning](resource-aware-overcommit-tuning.md). LegacySlot Pools retain their existing admission behavior. Guest free-memory statistics are not admission headroom and do not change the static commitment ledger.

Compare old snapshots and reporting snapshots at identical density. Measure cgroup usage, PSI, OOMs, aggregate VMM PSS, CPU cost, and tail latency after idle. Verify process and file state through snapshot and cross-host resume. Do not raise overcommit merely because a guest reports free pages or a single reclaim sample succeeds.

## FPR and deployment density

FPR is enabled by default for supported new golden snapshots, but it does not automatically increase deployment density. It reduces physical memory use without changing VM memory limits, Kubernetes requests, static commitments, or available slots. To place more VMs per node while retaining larger per-VM limits, explicitly tune the Pool's memory overcommit ratio and slot count, then verify the derived runtime budget. A fully committed Pool cannot admit another VM merely because FPR returned pages to the host.

Whether to enable memory overcommit depends on the workload's concurrent memory demand. FPR can help workloads that release memory between independent bursts, but it cannot reclaim live working sets when many VMs grow at the same time. Follow the [workload selection and OOM precautions](resource-aware-overcommit-tuning.md#choose-memory-overcommit-by-workload) before increasing density; default FPR does not make a particular overcommit ratio safe.

## Compatibility and limits

Upgrade the builder (including `fsb-vm`) and rebuild templates; no new CRD field is required. Existing Firecracker runtimes that support reporting restore the saved balloon device state. Verify the deployed Firecracker version and storage backend before rollout. Existing Fastlet memory admission and FastPath feedback remain responsible for placement and admission decisions.

Only pages already freed by the guest are reported. FPR does not actively evict cold file cache, reclaim live anonymous working sets, configure DAMON, or automatically pause sandboxes. A resident kernel, browser, or JVM can retain its working set during inactivity. Shared pmem/DAX layers are a separate storage optimization.

Native pause removes the VMM but can leave checkpoint file cache charged to the Fastlet cgroup. FPR does not evict host checkpoint cache, so a fall in VMM PSS alone does not establish a fall in total cgroup memory. Idle detection and automatic pause scheduling remain the caller's responsibility.
