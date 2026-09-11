# 阶段 1 施工方案：golden snapshot restore（driver 唯一启动路径）

> 文档类型：施工任务书（转交实现）
>
> 日期：2026-08-27
>
> 前置依赖：#22（publish 补全）、#23（pull 层）、#24（UDS 接线）均已合并；
> 设计定案：**restore 为唯一启动路径**（cold boot 已删除，见 #21 决策记录
> "启动路径"行）。
>
> 实现人按本任务书独立完成，有分歧以本任务书 + #21 设计文档（§3.3）为准。

---

## 1. 背景与目标

driver 的 `EnsureSandbox` 目前仍是**冷启动**：`ConfigureBootSource(kernel)` +
`AttachDrive` + `InstanceStart`（`internal/runtime/firecracker/driver.go`
的 `configureVM`）。缓存里已具备完整 golden 产物（pull 层落盘）：

```text
<StateRoot>/images/<sha256(image)>/
├── rootfs.img      # 只读共享 base（per-sandbox reflink 副本后作为根盘）
├── vmstate.snap    # 只读，restore 输入
├── memory.snap     # mem_backend.File 输入（COW，写不落文件，已验证）
└── manifest.json   # 含 machine{vcpu,memory} 与兼容性元组
```

本任务把 `EnsureSandbox` 改为 **golden snapshot restore**：`machine-config` →
`drive(rootfs 实例副本)` → `nic` → `PUT /snapshot/load` → `InstanceStart`，
使 `Image` 引用从发布产物直接恢复启动（无 kernel 引导）。

**范围边界**：

- 只做 golden restore（builder 快照）；运行态快照恢复（`SnapshotRef`，
  阶段 4）不做；
- overlaybd/ublk 设备（阶段 3）不做——rootfs 仍用 reflink 实例副本 +
  缓存文件路径（agent `LeaseDevices` 的 native 语义，已实现）；
- DART/P2P 不做（阶段 2）；
- guest resume hook 只做最小集（网络自动恢复依赖验证，见 §4）。

## 2. 代码位置与改动点

| 文件 | 改动 |
|------|------|
| `internal/runtime/firecracker/client.go` | 新增 `LoadSnapshot(ctx, SnapshotLoadRequest)`：`PUT /snapshot/load` |
| `internal/runtime/firecracker/driver.go` | `configureVM` 改为 restore 配置序列；`EnsureSandbox` 主流程替换冷启动段（保留幂等/状态/网络/清理骨架） |
| `internal/runtime/firecracker/driver_test.go` | 更新 fake 断言（boot-source 不再调用，snapshot/load 被调用） |
| `internal/runtime/firecracker/e2e_test.go` | 新增/改写 restore 用例（见 §6） |

`client.go` 新增的消息类型（v1.16，字段已由
`scripts/firecracker-mem-backend-check.sh` 实测验证）：

```go
// SnapshotLoadRequest mirrors PUT /snapshot/load (v1.16).
type SnapshotLoadRequest struct {
    SnapshotPath  string            `json:"snapshot_path"`
    MemBackend    SnapshotMemBackend `json:"mem_backend"`
    ResumeVM      bool              `json:"resume_vm"`
    // EnableDiffSnapshots/TrackDirtyPages/NetworkOverrides: 后续阶段
}
type SnapshotMemBackend struct {
    BackendType string `json:"backend_type"` // "File"
    BackendPath string `json:"backend_path"`
}
```

## 3. restore 配置序列（EnsureSandbox 替换段）

```text
EnsureSandbox(image, cpu, mem, ...):
  1. agent.PinImage(image)                    # 已实现（#24）
  2. 实例 rootfs：resolveRootfsImage → prepareInstanceRootfs
     （reflink 副本，现状逻辑保留——VM 写根盘不能污染缓存 base）
  3. vmstate/memory 路径：<images>/<key>/{vmstate.snap, memory.snap}
     （缓存文件，只读使用；memory.snap 由 COW mmap 承载 guest 写）
  4. 网络 slot（不变）
  5. firecracker 进程启动（不变）
  6. machine config：见 §4.1（restore 约束）
  7. AttachDrive(rootfs 实例副本) + AttachNetworkInterface(eth0)（不变）
  8. PUT /snapshot/load {
       snapshot_path: vmstate.snap,
       mem_backend: { backend_type: "File", backend_path: memory.snap },
       resume_vm: false }
  9. PUT /actions InstanceStart
  10. 轮询 Running（不变）
  11. sandbox 删除/失败 → agent.ReleaseDevices + agent.UnpinImage（不变）
```

- **不调用 `/boot-source`**（快照内已含 guest 状态）；
- `resume_vm: false` + 显式 InstanceStart，与现有 `bootVM` 的启动/轮询
  骨架复用（`client.Start` + `VMState` 轮询不需要改）；
- 失败清理路径不变（killAndForget + releaseSlot + RemoveInstance）。

## 4. 关键技术点与验证项（实现时实测确认）

### 4.1 machine-config 的 restore 约束（首要验证项）

firecracker 对 snapshot restore 的 machine-config 有限制：`mem_size_mib`
必须与快照创建时**一致**（vmstate 记录），`vcpu_count` 的可调性需实测。

- **方案（默认）**：restore 的 machine-config 使用 **manifest.machine**
  的值（builder 记录的 `{vcpu, memory}`，pull 层已缓存 manifest）——
  保证与快照一致；请求的 cpu/mem 仅做校验（请求 mem >= manifest mem，
  否则报错提示"请求内存小于模板快照内存"）；
- **验证项**：实测 v1.16 在 restore 前设置不同 vcpu/mem 的行为
  （`PUT /machine-config` 后 `PUT /snapshot/load` 是否报错）；若 vcpu
  可调且稳定，再把请求 cpu 映射进 machine-config（个性化恢复）；
- 结论记录在 PR 描述，设计文档 §3.3 的"mem 下限 = 快照 size"按实测
  校准。

### 4.2 网络恢复（guest 内 eth0）

builder 拍快照时**不配置外部连通性**（sandboxtemplate 设计非目标），
restore 后 attach 的新 virtio-net 需要 guest 内自动生效：

- **验证项**：restore 启动后 guest 内 `eth0` 是否被 udev/网络配置自动
  拉起（bionic rootfs），以及 slot 的静态 ip= 语义是否仍适用（快照
  restore 不走 kernel ip= 引导——**静态网络配置必须来自快照内已有
  配置或 guest 侧网络管理，而不是 boot args**）；
- 若 guest 内网络不自动恢复：评估 E2E 验收里"guest reachability"的
  实现路径（guest 内配置注入 vs resume 后网络重配），结论记入 PR。

### 4.3 rootfs 实例副本（保留现状）

restore 后 guest 继续写根盘 → 必须 per-sandbox 副本（reflink），不能
直接 attach 缓存 base。`prepareInstanceRootfs` 现状逻辑直接复用。

### 4.4 memory.snap 共享安全

多个 sandbox 从同一 image restore 时共用同一 memory.snap 文件：
已实测 **MAP_PRIVATE（COW）**——guest 写不落文件，共享安全、page cache
共享。无需复制 memory.snap（v1.16.1 验证结论，`scripts/firecracker-mem-backend-check.sh`）。

### 4.5 vmstate/memory 的完整性

restore 前校验：`vmstate.snap`/`memory.snap` 存在且与 manifest.files
digest 匹配（pull 层已校验落盘；此处仅 stat 存在性 + 可选快校验，
复用 `agent.ImageReady` 的语义——driver 侧 `resolveRootfsImage` 已隐含
缓存完整，保持现状即可）。

## 5. 测试计划

### 单元测试（driver_test.go）

- `configureVM`/restore 序列：fake client 断言调用顺序与载荷——
  machine-config（manifest 值）→ drive → nic → **snapshot/load**（路径
  正确）→ Start；**boot-source 不再被调用**；
- machine-config 载荷：manifest.machine 解析（fake manifest 注入）、
  请求 mem < manifest mem → 校验错误；
- 失败路径：snapshot/load 返回错误 → killAndForget + slot 释放（现有
  清理断言复用）；
- `client.go` 的 `LoadSnapshot`：URL/方法/JSON 字段（httptest 断言）。

### 集成测试（e2e_test.go，`firecracker` build tag）

**现有 E2E 基线是冷启动的，必须整体迁移到 restore**（脚本 + 测试代码 +
环境文档三处）：

| 文件 | 现状 | 改造 |
|------|------|------|
| `internal/runtime/firecracker/e2e_test.go` | `TestFirecrackerDriverE2E`/`NoInfra`/`Concurrent`/`ImageGC` 全部冷启动（boot-source） | 全部改 restore 启动；新增**快照制备 helper**（方式 B 自举）：冷启动一次 → `PATCH /vm` Pause → `PUT /snapshot/create` → 产出 `{rootfs 副本, vmstate.snap, memory.snap}` 组装进缓存布局 → 后续用例从该快照 restore |
| `scripts/firecracker-e2e.sh` | 下载 firecracker + **vmlinux.bin** + bionic rootfs，作为运行资产 | vmlinux.bin 降级为**快照制备资产**（仅第一次拍快照用，restore 不再引导内核）；rootfs 同理为制备输入；脚本增加"快照集产出/复用"步骤（`WORK` 下缓存制备产物，二次运行跳过制备） |
| `docs/guides/firecracker-runtime-e2e.md` | "VM baseline"（vmlinux.bin 运行依赖）、性能基线（冷启动 317ms/113ms 阶段拆分） | 更新：运行资产 = 快照集（kernel 仅构建/制备期）；性能基线改为 restore 时序（快照制备一次性成本 + restore 启动时序）；"guest 静态 ip= 内核参数"表述改为"restore 后网络恢复"（见 §4.2 验证结论） |

**新增/改造的断言**：

- restore 启动成功（VM Running）+ guest reachability（现状 ping 断言复用）；
- 多个 sandbox 从同一快照集 restore：互不干扰（memory.snap 共享 COW）；
- 制备快照与 restore 的 digest 一致性（manifest 校验语义）；
- 保留本地模式（无 agent socket）用例：行为与现状一致。

### 集成测试（方式 A 备选：消费 builder 产物）

若方式 B（自举）受限，可改用 `cmd/sandboxtemplate-builder` 的 snapshot
阶段产物（`scripts/sandboxtemplate-e2e.sh` 已验证可产出 vmstate+memory）
作为 E2E 快照集来源；两种方式的产出布局必须与 pull 层缓存布局一致
（`<images>/<sha256(image)>/{rootfs.img, vmstate.snap, memory.snap}`）。

## 6. 验收标准

1. `go build ./...`、`go test ./internal/runtime/firecracker/... -count=1
   -race`、`go vet ./...` 全绿；
2. `client.LoadSnapshot` 字段与 v1.16 实测一致（§4.1 验证项结论记录）；
3. **E2E 全量迁移到 restore**：方式 B 自举快照集 → restore 启动成功 +
   guest reachability；`TestFirecrackerDriverE2E`/`NoInfra`/`Concurrent`/
   `ImageGC` 全部绿；
4. `scripts/firecracker-e2e.sh` 与 `firecracker-runtime-e2e.md` 同步更新
   （快照制备资产语义 + restore 性能基线 + 网络恢复表述）；
5. 不引入新三方依赖；
6. 设计文档 §3.3 的 machine-config 约束与网络恢复结论按实测更新。

## 7. 参考材料

- 已验证的 v1.16 snapshot API：`scripts/firecracker-mem-backend-check.sh`
  （`PATCH /vm` 暂停、`PUT /snapshot/create`、`PUT /snapshot/load`
  `{snapshot_path, mem_backend{File, path}, resume_vm}`——字段均已实测）；
- driver 现状：`internal/runtime/firecracker/{driver.go,client.go}`
  （`configureVM`/`bootVM`/`ensureSandboxDir`/`prepareInstanceRootfs`）；
- 设计：#21 §3.3（restore 流程）、决策记录"启动路径"行；builder
  manifest（`cmd/sandboxtemplate-builder/manifest.go` 的 machine 字段）；
- E2E 环境：`docs/guides/firecracker-runtime-e2e.md`、
  `scripts/firecracker-e2e.sh`。

## 8. 交付物

- driver/client 改动 + 单测 + E2E restore 用例；
- PR 描述：machine-config restore 约束实测结论（vcpu/mem 可调性）、
  guest 网络恢复结论（eth0 是否自动拉起）、memory.snap 共享验证；
- 若实测推翻 §3.3 的假设，同步更新设计文档并说明。
