# Firecracker in Kubernetes pods — operational pitfalls

> Scope: the native Firecracker runtime, where Fastlet launches the VMM
> directly inside a Pod. The Firecracker documentation assumes a bare-metal or
> plain-VM host; when the VMM runs inside a Kubernetes Pod — the natural
> deployment shape for a sandbox fleet — four operational pitfalls appear that
> neither Firecracker nor typical Kubernetes documentation covers. Each one was
> hit and measured while productionizing a Firecracker-based sandbox runtime in
> Kubernetes.
>
> Node and kernel readiness for the single-host chain is covered in the
> [Firecracker integration environment guide](firecracker-integration-env.md#61-host--kernel-prerequisites)
> and `scripts/firecracker-host-check.sh`. This page covers the Pod-level layer
> those checks do not reach: memory limits, seccomp, cgroup placement, and
> huge-page strategy.

## Measurement environment

The numbers below are dated engineering observations from one fleet shape, not
release-grade benchmarks. Re-measure on your own nodes before sizing capacity.

| Dimension | Value |
| --- | --- |
| Nodes | bare-metal, AMD EPYC 128-core, 503 GiB RAM, local NVMe |
| OS / kernel | Ubuntu 24.04, Linux 6.8, cgroup v2 |
| Firecracker | v1.17.0 |
| Guest shape | 1 vCPU / 512 MiB, ext4 rootfs |
| Samples | 40 per arm (boot and restore); 30 VMs (KSM batch); 6 VMs (KSM restore eligibility probe) |
| Date | 2026-10-27 |

## Pitfall 1: zero hugetlb limits on the Pod cgroup

**Symptom.** When a Pod does not request huge-page resources, kubelet can leave
zero hugetlb limits on its cgroup hierarchy; on the reference nodes those
Pods carried `hugetlb.2MB.max=0`. Every `hugetlbfs` allocation from a process inside that
Pod then fails, and Firecracker's own errors point in an unrelated direction —
they read like a broken image or a host memory problem, not a cgroup limit:

```text
Kernel Loader: Unable to read kernel image     # fresh boot with huge_pages=2M
UFFDIO_COPY: Cannot allocate memory            # uffd restore into huge-page memory
```

This failure sends debugging to the wrong layer; check the cgroup limit
before touching Firecracker or the host configuration.

**Diagnose.** Read the limit from the cgroup that contains the VMM, and compare
it with the host pool:

```bash
# cgroup v2: resolve the VMM's cgroup and read its 2 MiB limit
CGROUP=$(cut -d: -f3 "/proc/<vmm-pid>/cgroup")
cat "/sys/fs/cgroup${CGROUP}/hugetlb.2MB.max"   # 0 = no huge-page allocation allowed

# host pool, for comparison
grep HugePages_Total /proc/meminfo
```

If the host has free huge pages but the Pod cgroup limit is zero, the
allocation failure is a cgroup limit, not a host misconfiguration.

**Fix paths.** Either request huge pages in the Pod spec, so kubelet configures
the hugetlb cgroup limit:

```yaml
resources:
  limits:
    hugepages-2Mi: 512Mi
  requests:
    hugepages-2Mi: 512Mi
```

or place the VMM outside the Pod cgroup hierarchy and set its limits there (see
pitfall 3). Both paths were verified with `huge_pages=2M` and with uffd-backed
restore.

## Pitfall 2: Firecracker's seccomp filter rejects `prctl`

**Symptom.** Firecracker installs a seccomp filter at startup. The allowlist
lets the VMM and API threads call `madvise` but does not include `prctl`, so
per-process KSM opt-in with `prctl(PR_SET_MEMORY_MERGE, 1)` (Linux 6.4 and
later) is rejected by the filter and the process dies on `SIGSYS`. Enabling
KSM from the host side therefore cannot use the normal opt-in path: setting
`/sys/kernel/mm/ksm/run=1` alone does not make the guest-memory mappings
mergeable, because KSM only scans VMAs explicitly marked with
`MADV_MERGEABLE` (or process-wide via `PR_SET_MEMORY_MERGE`).

**Workaround.** The marking can be driven through a ptrace-based injection
channel into the VMM process: attach to the main thread, then inject
`madvise(MADV_MERGEABLE)` over each private anonymous read-write VMA. On the
reference fleet this marked 516-517 MiB per VM across 30 VMs with zero
failures. It is fragile — it depends on the VMM's thread and VMA layout and
must be revalidated for every Firecracker release.

**Scope of the workaround.** KSM only merges private anonymous pages. The
snapshot-restore path maps guest memory from the memfile as private
file-backed pages, so a restore fleet has almost nothing eligible: a probe of
6 restored VMs over 3 minutes of KSM scanning left `pages_shared` unchanged.
Fresh-boot fleets are the case where the workaround pays off: 30 same-template
VMs converged from 65.8 MiB to 9.9 MiB PSS per VM (-85%) in about a minute, at
0.02-0.03 cores of `ksmd`.

**Alternatives and their costs.** Changing the filter contents is upstream
Firecracker's call; this page documents the workaround, not a code change.
Disabling Firecracker's seccomp filter trades away a security layer for every
VM. Leaving KSM off costs the fresh-boot density win but is the only zero-risk
option for restore-based fleets.

## Pitfall 3: place the VMM outside the `kubepods` cgroup subtree

**Symptom.** With the VMM left in the Pod's default cgroup placement, shared
page deduplication and memory accounting stop being attributable. KSM counters
(`/sys/kernel/mm/ksm/pages_shared`, `pages_sharing`) are host-global, and the
Pod's memory metrics conflate the VMM with the rest of the Pod, so
capacity and placement decisions built on those numbers go wrong. Pod-level
limits also apply to the VMM when it remains in the `kubepods` subtree — this
is how pitfall 1 blocks huge-page allocation in the first place.

**Requirement.** Place every VMM in a dedicated cgroup outside the `kubepods`
subtree — for example one cgroup per VMM or per pool under
`/sys/fs/cgroup/<runtime>/...` — and set the intended memory and hugetlb limits
there. Treat this as a hard deployment requirement, not a tuning option: on the
reference fleet, moving the VMM into a dedicated non-`kubepods` cgroup with its
own hugetlb limit unblocked both the fresh-boot huge-page path and the uffd
restore path.

**Trade-off to plan for.** Pod-level policies that operators assume cover the
sandbox do not apply to a VMM that lives outside the Pod cgroup. The runtime
owns that cgroup's limits, OOM policy, and monitoring; scrape accounting from
the cgroup the VMM actually lives in.

## Pitfall 4: transparent huge pages vs explicit 2M pages

**Boot path.** Boot-to-agent-ready latency, P50, 40 samples per arm:

| Guest memory backend | P50 | vs 4K |
| --- | ---: | ---: |
| 4K anonymous (file backend) | 405.9 ms | — |
| Transparent huge pages (`madvise`) | 318.6 ms | -21% |
| Explicit 2M pages (hugetlbfs) | 333.7 ms | -18% |

**Restore path.** Restore latency, P50, 40 samples per arm:

| Restore path | P50 | Note |
| --- | ---: | --- |
| Snapshot + file backend | 10.52 ms | current default |
| Snapshot + uffd backend, `huge_pages=2M` | 15.61 ms | +5.1 ms (+48%), the uffd handler hop |
| Hugetlbfs snapshot + file backend | rejected | Firecracker requires uffd for hugetlbfs snapshots |

The restore-side parameter decides the mapping; it does not have to match the
`huge_pages` value baked into the snapshot.

**Reservation rigidity.** An explicit 2M pool is a rigid reservation. In the
reference run, a reservation of 8 GiB (4096 x 2 MiB) peaked at 2560 pages of
occupancy, so at least 37% of the reservation stayed idle for the entire run. The reservation
cannot be floated to non-huge-page workloads, and shrinking `nr_hugepages` on
a co-located host often fails. Idle density is not a differentiator: both
backends lazily claim roughly 66-70 MiB per idle VM.

**Accounting.** PSS and RSS are effectively blind to huge-page guest memory
(about 0.8 MiB per VM observed). A capacity ledger for a 2M fleet must read
`free_hugepages`, not the usual per-process memory metrics.

**Net guidance.** Transparent huge pages in `madvise` mode are the default:
most of the boot win, zero pool management, and no rigid reservation. Explicit
2M pages buy a slightly better boot number in exchange for a forced uffd
page source (+48% on the restore path), a non-reclaimable reservation, and an
accounting blind spot. Adopt them only for workloads proven to be TLB or
page-table sensitive; at fleet density the reservation is usually the deciding
constraint.

## Prerequisite sampling checklist

Sample every candidate node before committing capacity to Firecracker Pods.
The runtime-agent readiness loop and `scripts/firecracker-host-check.sh` cover
the KVM/TUN/kernel/storage half; the interfaces below cover the Pod-level
memory, storage, and network paths that the existing checks do not reach.

| Interface | Probe | Why it matters |
| --- | --- | --- |
| KVM | `/dev/kvm` opens read-write | The VMM cannot start without it |
| Nested virtualization | `vmx`/`svm` flag in `/proc/cpuinfo` | Informational; a VM without the flag cannot host KVM guests (nested pools) |
| userfaultfd | `/dev/userfaultfd` exists and is openable; `/proc/sys/vm/unprivileged_userfaultfd` policy | uffd-backed restore and page sources |
| io_uring | `/proc/sys/kernel/io_uring_disabled` (0 = allowed) | Asynchronous I/O in the OverlayBD/ublkd storage stack |
| ublk | `/dev/ublk-control` exists | Userspace block devices for OCI/OverlayBD snapshots |
| eBPF TC attach | A `clsact` qdisc can be attached; the loader has `CAP_BPF`/`CAP_NET_ADMIN` | Host-side egress and network enforcement |

A compact probe (run per candidate node; reports only, changes nothing):

```bash
#!/usr/bin/env bash
set -u
fail=0
say() { printf '%-10s %s\n' "$1" "$2"; }

if [[ -c /dev/kvm && -r /dev/kvm && -w /dev/kvm ]]; then
  say PASS "/dev/kvm present and read-write"
else
  say FAIL "/dev/kvm missing or not accessible"; fail=1
fi
grep -Eq ' (vmx|svm) ' /proc/cpuinfo \
  && say INFO "nested-virtualization flag present" \
  || say WARN "no vmx/svm flag: this node cannot host KVM guests"

if [[ -c /dev/userfaultfd ]]; then
  say PASS "/dev/userfaultfd present"
else
  say WARN "/dev/userfaultfd absent; only the userfaultfd(2) syscall path remains"
fi
say INFO "vm.unprivileged_userfaultfd=$(cat /proc/sys/vm/unprivileged_userfaultfd 2>/dev/null || echo n/a)"
say INFO "kernel.io_uring_disabled=$(cat /proc/sys/kernel/io_uring_disabled 2>/dev/null || echo n/a)"

if [[ -c /dev/ublk-control ]]; then
  say PASS "/dev/ublk-control present"
else
  say WARN "/dev/ublk-control missing (required only for the OCI/OverlayBD chain)"
fi

dev="$(ip -o route show default 2>/dev/null | awk '{print $5; exit}')"
if [[ -n "$dev" ]] && tc qdisc show dev "$dev" 2>/dev/null | grep -q clsact; then
  say PASS "clsact qdisc on $dev (TC eBPF attach)"
else
  say WARN "no clsact qdisc on ${dev:-default device}; TC eBPF attach needs one"
fi

exit "$fail"
```

KVM is the only hard failure; the remaining rows are warnings scoped to the
features that consume them. Sampling across three worker nodes spanning the
production pool shapes produced zero misses; the point of the checklist is to
make that a repeatable pre-flight gate rather than a one-time observation.

## KubeVirt VM pools need independent calibration

If the Firecracker Pods run inside KubeVirt VM pools (nested virtualization),
bare-metal numbers do not carry over. Calibrate the pool shape separately:
boot and restore latency, per-VM memory cost, and density all shift when the
KVM provider runs inside a guest. Treat bare-metal and KubeVirt pools as
distinct performance envelopes in capacity planning, and never extrapolate
measurements from one to the other. The checklist above still applies, but run
it inside the pool VM: a pool VM can expose the interfaces and still lack
nested KVM.

## Related documentation

- [Firecracker integration environment](firecracker-integration-env.md) —
  host/kernel prerequisites and the single-host chain.
- [Performance](performance.md) — the measurement contract this page follows
  (dated engineering baselines are not release claims).
- [OverlayBD OCI sandboxes](overlaybd-oci-sandboxes.md) — ublk requirements and
  the storage layout.
- [Secure runtimes](secure-runtimes.md) — runtime profiles and the Firecracker
  storage contract.
