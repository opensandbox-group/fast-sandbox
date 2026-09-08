# SandboxTemplate 产物分发 OCI 化（format=overlaybd）

> 文档类型：设计提案
>
> 日期：2026-09-08
>
> 范围：仅改变 `spec.output.format=overlaybd` 构建的**分发通道**；`native` 构建完全不受影响。快照生成语义、节点缓存布局、restore 路径均不变。

- [Summary](#summary)
- [核心问题](#核心问题)
- [方案](#方案)
- [API 变更](#api-变更)
- [上下游影响](#上下游影响)
- [原子性与回滚](#原子性与回滚)
- [演进：内存按需加载](#演进内存按需加载)
- [风险](#风险)
- [迁移](#迁移)
<!-- /toc -->

## Summary

`format=overlaybd` 的构建产物中，`rootfs.ext4` 与 `memory.snap` 两个大文件改为以**单层 OverlayBD LSMT 的 OCI 镜像**发布（经 streamingvolume），小文件 `vmstate.snap` / `manifest.json` 留在 S3。`format=native` 维持现有全量 S3 布局不变，节点按 `manifest.json` 的 `format` 字段选择拉取通道。

## 核心问题

当前全部产物走自建 S3 分发链（index 指针 → manifest → digest 校验下载）：

1. memory（≥2Gi）/ rootfs 实数据冷拉慢：无压缩、无 P2P、无按需取段；
2. 自建链路维护成本：index、SHA256SUMS、aws-cli、S3 大文件下载分支；
3. 现有 `overlaybd-import-raw` 转换产物没有对齐 OverlayBD 生态（OCI 层标注、streamingvolume 消费端），后续内存按需加载无从演进。

## 方案

### 通道划分（按数据大小与访问模式）

| 产物 | 特征 | format=overlaybd | format=native |
|------|------|------------------|---------------|
| rootfs.ext4 | 逻辑 30Gi 稀疏，随机读写 | **OCI 镜像** `<t>-rootfs`（单层 LSMT） | S3（现状） |
| memory.snap | ≥2Gi，随机读（COW） | **OCI 镜像** `<t>-mem`（单层 LSMT） | S3（现状） |
| vmstate.snap | MB 级，只读一次性读 | S3（现状） | S3（现状） |
| manifest.json | KB 级 | S3（现状，schema 扩展） | S3（现状） |

vmstate 不进 OCI 的原因：小、只读、restore 时一次性读入，S3 直拉已是最优；且它是逐 build 产物（vCPU 数、root drive 容量、NIC MAC、pause 时刻状态都随构建变化），无去重收益。

### 镜像格式

streamingvolume 标准 commit 产物：

```
<t>-rootfs:<tag> / <t>-mem:<tag>
└── manifest (artifactType: application/vnd.alibaba.overlaybd.layer.zfile)
    ├── config: {}（空 JSON）
    └── layer[0]: LSMT blob（mediaType + OverlayBDBlobDigest/Size/Version 三标注）
```

两条硬约束：

- **裸块字节级一致**：写入方式为 Attach 空裸卷后 `dd`，不是挂 fs 拷文件。重建 ext4 会导致 inode/UUID 漂移，破坏快照恢复契约。
- **一镜像一设备**：rootfs 与 memory 是两个独立块设备对象（root drive 与 memfile），不能合并成镜像或层；vmstate 非块设备，不进 OCI。

### builder 流程（仅 format=overlaybd 的发布阶段替换）

```
快照阶段不变 →
  子进程拉起 strmvold（root=workdir/strmvol，unix socket，随 Pod 销毁）
  mem:    Attach 裸卷(memSize) → dd memory.snap → Commit+Push <t>-mem:<tag>
  rootfs: Attach 裸卷(rootfsSize) → dd rootfs.ext4 → Commit+Push <t>-rootfs:<tag>
  S3:     vmstate.snap → manifest.json（最后上传，保持提交点语义）
  → Pod annotations 自报 → controller 更新 status
```

builder 经 gRPC（`api/strmvold`，与 strmvolctl 同一契约）驱动子进程 daemon，而非进程内嵌入 `pkg/service`——后者会引入无权限的私有模块（dadi-snapshotter、overlaybd-convert）。

`overlaybd-import-raw` 转换步骤与 S3 大文件上传删除（LSMT 封装由 commit 完成）。

### 节点拉取链（按 format 分支）

```
manifest.json（S3，现有 index/digest 校验）
├─ format=native     → 现有 S3 全量下载（不变）
└─ format=overlaybd  → Fetch+Attach(-mem, ro)    → cp 设备 → <cache>/memory.snap
                       Fetch+Attach(-rootfs, ro) → cp 设备 → <cache>/rootfs.img
                       vmstate.snap / manifest.json 走 S3（现有逻辑）
```

缓存布局与后续 reflink / jailer hardlink / snapshot load **零改动**。

## API 变更

### SandboxTemplateSpec（`spec.output`）

```go
type SandboxTemplateOutput struct {
    // 现有字段不变
    RootfsSize       string
    Format           string // "overlaybd" 走新通道，"native" 走原通道
    Publish          string // S3 基址（s3://bucket/prefix）
    PublishSecretRef string

    // 新增（format=overlaybd 时必填）
    Registry          string // 镜像仓库基址，如 registry.example.com/fs-templates/<name>
    RegistrySecretRef string // docker config JSON（streamingvolume secret.type=dockerAuth）
}
```

镜像 ref 派生规则：`<Registry>-rootfs:<tag>` / `<Registry>-mem:<tag>`，`<tag>` 默认取 build generation（或 manifest digest 短缀）。

### SandboxTemplateStatus（新增字段）

```go
RootfsImageRef string // digest-pin：...-rootfs:<tag>@sha256:...
MemoryImageRef string // digest-pin：...-mem:<tag>@sha256:...
```

builder 经 Pod annotations `sandbox.fast.io/rootfs-image-ref` / `sandbox.fast.io/memory-image-ref` 自报。

### manifest.json（schema 扩展）

```json
{
  "format": "overlaybd",
  "artifacts": {
    "rootfs":  {"ref": "...@sha256:..."},
    "memory":  {"ref": "...@sha256:..."},
    "vmstate": {"key": "<digest>/vmstate.snap", "sha256": "...", "sizeBytes": 0}
  }
}
```

`SHA256SUMS` 收缩为仅覆盖 S3 侧文件。index/`<sha256(image)>.json` 不变。

## 上下游影响

| 组件 | 影响 |
|------|------|
| controller | 新增 status 字段回读与校验；build Pod 模板增加 /dev hostPath（ublk 设备节点在宿主 devtmpfs 创建；内核<5.19 走 tcmu 无需 ublk）与 registry SecretKeyRef |
| builder 镜像 | 补齐 OverlayBD 完整工具链（overlaybd-create/commit 等）；go.mod 引入 streamingvolume（内部代理） |
| runtime-agent | pull 链增加 format 分支与 registry 通道（进程内嵌 streamingvolume service）；S3 大文件下载分支仅 native 使用 |
| driver / restore | 无 |
| SandboxPool warmImages | 无（经 index→manifest 自动感知通道） |
| e2e 环境 | integration-env 增加 registry 容器 |
| S3 存储 | overlaybd 构建的发布缩减为小文件；native 不变 |

## 原子性与回滚

- 上传顺序固定：两镜像 → vmstate → manifest.json（最后）→ annotations → status；status 更新即提交点，任何失败停留在旧一代。
- 镜像 digest 由 registry 内容寻址保证；跨产物一致性由 `artifactDigest`（manifest.json digest）承担，节点拉齐后校验。
- digest-pin 引用，tag 被覆盖不影响已发布代。

## 演进：内存按需加载

本方案是 Mode 2 的直接前置：Phase 1（本方案）Attach 后 cp 出文件；Mode 2 将「cp 出来」替换为「Attach 可写卷直连 memfile」，缺页经 overlaybd 按需取段、写落本地 upper，发布格式不变。前置验证（另行立项）：Firecracker `mem_file_path` 指向块设备、jailer 内设备节点暴露、缺页 prefetch。

## 风险

| 风险 | 缓解 |
|------|------|
| registry 大 blob 上限/推送慢 | LSMT zstd 压缩；streamingvolume 并发推层+断点重试；上线前确认 blob 上限 |
| 宿主机无 ublk（内核<5.19） | blockDriver 回退 tcmu（内核 ≥4.x，需 target_core_user 模块 + configfs + overlaybd-tcmu handler，公开 overlaybd 源码可编）；builder 经 `SANDBOX_TEMPLATE_BLOCKDRIVER` 切换 |
| streamingvolume 版本行为变化 | 锁模块版本；集成测试覆盖 Attach/Commit/Push |
| 双存储运维 | S3 侧收缩为小文件；GC 各自独立，registry 侧按 tag 生命周期配置 |
| 拷出模式 memory 仍全量读 | 灰度期可回退旧链路；Mode 2 落地后消除 |

## 迁移

1. **双写**：overlaybd 构建同时发布旧 S3 全量布局与新布局；节点 feature gate 灰度切新链路（native 始终旧链路）。
2. **切换**：全量后停止 S3 大文件发布；S3 GC 清理存量。
3. **收敛**：删 builder 旧发布代码与 agent 大文件下载分支（native 路径保留）。

任一阶段可回滚到上一阶段。
