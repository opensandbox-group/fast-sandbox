# Kata Firecracker 运行时调查与解除门禁方案

**状态**：根因已定位；`kata-fc` 继续 fail closed
**调查日期**：2026-07-21
**目标环境**：远端 `fast`，kind 集群 `fsb-e2e-kata`，Kata Containers `3.27.0`，Firecracker `1.12.1`

## 1. 结论

当前 `kata-fc` 失败不是 Fast Sandbox Controller、Fastlet 调度或 Firecracker jailer 的随机故障，而是测试节点的 Kata Firecracker 运行时基线不完整，包含两个相互独立的问题：

1. Kata `3.27.0` 静态包中 `configuration-fc.toml` 指向默认 `vmlinux.container`，但对应 kernel config 没有 `CONFIG_VIRTIO_MMIO=y`。Firecracker 通过 virtio-mmio 暴露 rootfs，guest 因此看不到 `/dev/vda1`，内核 panic 并重启；外层最终只看到 hybrid-vsock 连接超时。
2. 修正 kernel 后 VM 和 `kata-agent.service` 可以正常启动，但 kind 的 containerd 使用 overlayfs snapshotter。Firecracker 不提供 virtio-fs/host filesystem sharing，Kata 无法把 OCI workload rootfs 注入 guest，最终报 `failed to mount ... rootfs ... ENOENT`。Firecracker 路径需要块设备型 snapshotter，官方操作路径是 devmapper。

因此，不能通过增加超时、重试、关闭 jailer或仅替换 kernel 来宣称支持。`KataFirecrackerNotValidated` 能力门禁目前是正确行为。

## 2. 证据链

### 2.1 Fast Sandbox 之外也能复现

在专用 kind 集群直接创建 `runtimeClassName: kata-fc` 的最小 Pod，不经过 Sandbox CRD、FastPath 或 Fastlet，仍然失败：

```text
Failed to Check if grpc server is working:
timed out connecting to hybrid vsocket
hvsock:/run/vc/firecracker/<sandbox>/root/kata.hvsock
```

这把故障边界收缩到了 Kubernetes CRI → Kata shim → Firecracker/guest 资产。

### 2.2 Host/KVM 能力正常

目标节点具有：

- `/dev/kvm`、`/dev/vhost-vsock`、`/dev/vhost-net`、`/dev/net/tun`；
- nested KVM 可用；
- `kata-runtime kata-check --verbose` 成功；
- Firecracker 能创建 VMM 并启动 guest kernel。

禁用 jailer 后错误完全相同，因此 jailer/chroot/cgroup 不是本次根因。

### 2.3 直接启动 Firecracker 暴露真实内核错误

使用 Kata 生成配置中的 kernel、image、boot args 和 vsock 直接运行 Firecracker，并临时启用串口后，guest 明确输出：

```text
/dev/root: Can't open blockdev
VFS: Cannot open root device "/dev/vda1" or unknown-block(0,0)
Kernel panic - not syncing: VFS: Unable to mount root fs
```

同时静态包中的配置为：

```text
config-6.18.12-181:
  CONFIG_VIRTIO_BLK=y
  # CONFIG_VIRTIO_MMIO is absent
```

boot args 虽然包含 Firecracker 自动添加的 `virtio_mmio.device=...`，kernel 却没有注册对应 device bus，所以没有生成 `vda`。

### 2.4 MMIO kernel 验证

同一静态包里的 `vmlinux-dragonball-experimental.container` 对应 config 含有 `CONFIG_VIRTIO_MMIO=y`。只替换 kernel 后：

```text
virtio-mmio: Registering device ...
virtio_blk virtio0: [vda] ...
vda: vda1
VFS: Mounted root (ext4 filesystem) readonly
Started kata-agent.service - Kata Containers Agent.
```

这证明原 hybrid-vsock 超时的首要原因是 guest kernel 在 agent 启动前已经 panic，而不是 vsock 网络本身。

`dragonball-experimental` kernel 只用于定位，不能作为生产修复：它的命名、版本和构建目标都不是稳定的 Firecracker 供应链契约。

### 2.5 CRI workload 暴露第二个前置条件

使用 MMIO kernel 再次创建 Kubernetes Pod 后，VM/agent 已经启动，但 workload rootfs 创建失败：

```text
failed to mount /run/kata-containers/shared/containers/<id>/rootfs
to /run/kata-containers/<id>/rootfs: ENOENT
```

Kata 官方 Firecracker 指南明确要求使用 block device backing store，并以 containerd devmapper snapshotter 为标准配置。Kata 的虚拟化说明也明确列出 Firecracker 不支持 filesystem sharing。

官方参考：

- [Kata Containers with Firecracker](https://github.com/kata-containers/kata-containers/blob/main/docs/how-to/how-to-use-kata-containers-with-firecracker.md)
- [Kata virtualization design](https://github.com/kata-containers/kata-containers/blob/main/docs/design/virtualization.md)
- [Kata releases](https://github.com/kata-containers/kata-containers/releases)；后续发布记录包含 unified kernel 加入 MMIO fragment 的修复。

## 3. 正确修复方案

### 3.1 Runtime artifact

- 将 E2E/生产基线从 Kata `3.27.0` 升级到已将 Firecracker MMIO fragment 纳入统一 kernel build 的稳定版本；
- pin 完整组合，而不是单独 pin shim：Kata runtime、Firecracker、guest kernel、guest image、kata-agent 必须来自同一验证矩阵；
- 不使用 `vmlinux-dragonball-experimental.container` 作为长期替代；
- 安装阶段验证 FC 配置引用的实际 kernel，并要求对应 config 有 `CONFIG_VIRTIO_MMIO=y`。

### 3.2 Containerd storage

- 为 Firecracker 节点配置持久化 devmapper thin pool；
- 确认 `io.containerd.snapshotter.v1.devmapper` 状态为 `ok`；
- `kata-fc` CRI runtime 显式使用 devmapper snapshotter，不复用默认 overlayfs；
- 节点重启时先恢复 thin pool，再启动 containerd/kubelet；
- 为 thin-pool 容量、水位、metadata、discard 和 GC 建立节点级观测与保护。

不应让 Pool 或 Fastlet 动态创建 devmapper thin pool。这是节点 runtime/bootstrap 的职责，失败时节点不能进入 `kata-fc` 可调度集合。

### 3.3 Capability gate

解除 Fast Sandbox 的 `KataFirecrackerNotValidated` 前，节点预检至少需要证明：

```text
/dev/kvm and /dev/vhost-vsock available
kata/firecracker/kernel/image versions match pinned matrix
configured kernel has CONFIG_VIRTIO_MMIO=y
configured rootfs is image, not initrd
devmapper snapshotter plugin is ok
kata-fc runtime selects devmapper
minimal CRI RuntimeClass Pod reaches Ready
```

仅 `kata-check` 成功不够：本次环境正是 `kata-check` 成功但 guest rootfs 无法启动。

### 3.4 Fast Sandbox 验收

节点基线修好后按以下顺序解除门禁：

1. 最小 `RuntimeClass=kata-fc` Pod Ready、删除和重复创建通过；
2. `kata-fc` Fastlet Pod Ready，heartbeat 报告 RuntimeReady；
3. Sandbox 创建、私有网络、Infra delivery 和透明代理通过；
4. workload 自行退出、声明式删除、Fastlet 重启和 node reboot 通过；
5. devmapper image cache/GC 不破坏 Pool warm image 和运行中 snapshot；
6. 重复执行 E2E，无 hybrid-vsock timeout、orphan shim、thin-pool leak。

在这些条件全部满足前：

- 内置 `kata-fc` profile 保持 `CapabilityDegraded`；
- Controller 不创建 `kata-fc` Fastlet Pod；
- 文档只声明“已定位并有明确节点修复路径”，不声明生产支持。

## 4. 组件责任

| 责任 | 组件 |
|---|---|
| Pin Kata/Firecracker/kernel/image 组合 | 节点镜像/部署系统 |
| 创建并恢复 devmapper thin pool | Node bootstrap |
| 配置 containerd `kata-fc` runtime/snapshotter | Node bootstrap |
| 检查节点能力并决定是否可调度 | 调度层/节点能力上报 |
| Fastlet runtime 真实探活与 heartbeat | Fastlet Pod |
| Pool fail-closed 与状态展示 | Sandbox Controller |
| 清理 orphan shim/VM/storage 残留 | NodeJanitor，执行调度层下发的节点级清理计划 |

调度层拥有全局视角，应基于节点能力和 runtime inventory 选择可用节点；NodeJanitor 不负责做调度决策，只负责在目标节点执行经过 fence 的本地清理。

## 5. 本轮代码结论

本轮不解除 `kata-fc` profile 门禁，也不把 E2E 中的 kind 节点临时改造成生产 devmapper 节点。需要先把上述节点基线作为独立基础设施工作完成，再将 capability probe 和真实 `kata-fc` E2E 纳入默认 Gate。
