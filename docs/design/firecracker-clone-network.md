# Firecracker clone 网络：per-clone netns 数据面改造

> 文档类型：技术方案（设计）
>
> 日期：2026-08-28
>
> 状态：草案（提交评审）
>
> 关联：opensandbox-group/fast-sandbox#26（restore 网络模型与 slot DNAT 漂移）；
> [OSEP-0022](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0022-multi-sandbox-egress-control-plane.md)
> （多沙箱 egress 控制面，source-IP dispatch 前置假设）。
>
> 配套：[firecracker-on-demand-loading.md](firecracker-on-demand-loading.md)
> 的启动路径/数据面设计（本方案是其网络数据面的改造）。

---

- [问题](#问题)
- [社区调研结论](#社区调研结论)
- [方案评估](#方案评估)
- [总体设计：per-clone netns（方案 2）](#总体设计per-clone-netns方案-2)
- [jailer 适配](#jailer-适配)
- [数据面流量路径](#数据面流量路径)
- [数据面语义：不变 / 新增 / slot 字段](#数据面语义不变--新增--slot-字段)
- [每实例与共享](#每实例与共享)
- [与其他 runtime 的关系](#与其他-runtime-的关系)
- [restore 启动衔接](#restore-启动衔接)
- [与 OSEP-0022（多沙箱 egress 控制面）的兼容性](#与-osep-0022多沙箱-egress-控制面的兼容性)
- [改动面清单](#改动面清单)
- [测试计划](#测试计划)
- [风险与已知问题](#风险与已知问题)
- [验证项](#验证项)
- [决策记录与待决策项](#决策记录与待决策项)

## 问题

golden snapshot restore 成为唯一启动路径后（PR #25），guest 网络身份
（IP/MAC）**baked 在快照里**（v1.16 restore 从 vmstate 恢复 NIC 与 guest
网络栈），而 fastlet slot 数据面按 `slot.IP + 1` 推导 guest 地址并安装
host 侧 DNAT。两个模型只在第一个 slot 巧合对齐：

- **多实例 / 非首 slot**：host DNAT 目标 `slot.IP+1` 与 baked guest IP
  错位 → ingress 不可达；
- **共享 baked MAC/IP 挂同一桥**：并发 restore 多个 clone 时 ARP 冲突
  （E2E `TestFirecrackerDriverE2EConcurrent` 被迫放弃 per-instance
  reachability 断言）；
- **fastlet-proxy ingress**（`AccessKindDirectIP` dial slot.IP 依赖 DNAT）
  对每个非首实例失效；
- **OSEP-0022 前置破坏**：firecracker egress 流量 src=共享 baked IP，
  host forward 点不可区分（详见兼容性章节）。

## 社区调研结论

Firecracker 官方 clone 网络模型（`docs/snapshotting/network-for-clones.md`，
v1.16.1）是**唯一被验证的路径**：

1. 每个 clone 的 **firecracker 进程 + tap 运行在独立 netns**——tap 同名
   无冲突（netns 隔离），**同 guest MAC/IP 无 ARP 冲突**（ARP 域按 netns
   隔离）；
2. netns 内 veth 接 host，**NAT 解决"同 guest IP"**（netns 内 DNAT/
   MASQUERADE）；
3. 官方明确：**共享桥不隔离的方案不可行**（ARP 冲突）；v1.16 restore
   不热插拔 NIC，guest 侧重配（udev 注入）不触发。

## 方案评估

| 方向 | 结论 | 理由 |
|------|------|------|
| 1. 对齐 DNAT 到 baked guest IP（数据面最小改） | **否决** | 只解决首 slot 巧合；共享 IP/MAC 仍挂同一桥，ARP 冲突无解；egress source-IP dispatch 仍不可区分 |
| 2. **per-clone netns**（上游模型） | **采用** | 官方唯一验证路径；fastlet slot netns 即现成的 per-clone 隔离域（见下） |
| 3. restore 后 guest 侧重配网络 | **否决** | v1.16 restore 不重触发 NIC 热插拔，无 guest 侧机制 |

## 总体设计：per-clone netns（方案 2）

**关键洞察**：fastlet 的 slot netns（`LinuxNetNSDriver` 每 sandbox 一个
`fsb<hex>`）就是现成的 per-clone 隔离域——container runtime 本来就是
"进程进 slot netns"。firecracker 从"host 侧桥接 tap + host DNAT"改为
"进程经 **jailer `--netns`** 进 slot netns + netns 内数据面"，与 container
同构。

> **载体确认（真机验证，v1.16.1）**：`firecracker --help` 完整输出确认
> firecracker 二进制**没有** `--netns` flag——上游文档的 `--netns` 是
> **jailer** 参数（"using the `--netns` jailer parameter"）。进程进 netns
> 的唯一官方路径是 jailer，同时激活设计里预留的 `JailerPath` 字段
> （chroot 隔离的生产安全收益一并拿到）。

```text
改造前（现状）                       改造后（per-clone netns）
┌─ host netns ───────────────┐    ┌─ slot netns（已存在）────────────┐
│ bridge fsb0                │    │ eth0 = slot.IP（172.30.0.2/24）   │
│   ├─ hostVeth ── netns     │    │   ├─ 默认路由 via 桥网关（不变）  │
│   ├─ tap fc<hex> ── VM     │    │   ├─ OUTPUT 隔离规则（不变）      │
│ DNAT slot.IP→slot+1 (host) │    │   ├─ vmtap0（netns 内创建）── VM  │
│ firecracker 进程在 host     │    │   ├─ PREROUTING DNAT slot.IP→guest│
│ (guest MAC/IP 挂桥共享)     │    │   ├─ POSTROUTING MASQ guest→slot.IP│
└────────────────────────────┘    │   └─ FORWARD 隔离（兄弟禁止）     │
                                  │ jailer --netns <slotNetNSPath>    │
                                  └───────────────────────────────────┘
```

## jailer 适配

### 启动形态（真机验证）

```text
jailer --id <sandboxID[:32]> --netns <slotNetNSPath> --uid 0 --gid 0 \
       --exec-file <firecracker> --chroot-base-dir <StateRoot>/jails -- \
       --api-sock /api.sock ...
```

- `--chroot-base-dir`（v1.16 参数名；位置参数形态被拒绝）；
- **`--id` 由 jailer 自动传递给 firecracker，`--` 后不得重复传入**
  （重复报 `DuplicateArgument`）；
- 非 daemonize 时 jailer **exec** firecracker（PID 不变）——进程管理/
  probe 语义与现状一致（`killAndForget` 按 PID 工作）。

### chroot 目录结构与路径转换（真机验证）

```text
<chroot-base-dir>/<exec-file basename>/<id>/root/     # jail root
├── api.sock        # firecracker 在 chroot 内创建 → 宿主侧此路径
└── rootfs.img      # 实例 rootfs（见下）
```

- 中间目录是 **exec-file 的 basename**（`firecracker/`），不是固定值——
  driver 的路径转换按 `jailerRoot(chrootBase, execBase, id)` 推导；
- driver 的 `state.APIAddress`（等待 socket 用宿主路径）=
  `<chroot-base>/<exec-basename>/<id>/root/api.sock`；
- firecracker 进程日志走 `--log-path`（chroot 内路径）→ 宿主侧同样转换；
- **sandbox 状态目录迁移**：driver 现有的 `<StateRoot>/sandboxes/<id>/`
  （meta.json）保留，但**实例 rootfs 从 stateDir 移到 jail root**（见下）。

### cwd 与实例 rootfs（cwd 依赖的解法）

jailer chroot 后进程 cwd 在 chroot 内（`/`）——restore 依赖的"相对
rootfs.img 经 cwd 解析"不再可靠。**解法（定案）**：

- 快照 bake 的 root 驱动路径**保持相对 `rootfs.img`**（builder/E2E prep
  现状不变）；
- driver 把实例 rootfs 的 reflink 副本直接放进 **jail root**：
  `<chroot-base>/<exec-basename>/<id>/root/rootfs.img`——firecracker 的
  cwd 是 chroot 内 `/`，相对 `rootfs.img` 恰好解析到该副本；
- `prepareInstanceRootfs` 的目标从 `stateDir/rootfs.img` 改为
  `jailRoot/rootfs.img`（jail root 在 launch 前由 launcher 创建）；
- vmstate.snap / memory.snap：restore 的 snapshot_path/mem_backend 用
  **宿主绝对路径**（jailer chroot 后 firecracker 按传入路径打开——传入
  绝对路径在 chroot 外不可见？）——**注意**：jailer chroot 后，firecracker
  能访问的只有 jail root 内的文件。因此 **vmstate/memory 必须复制或
  bind 进 jail root**：
  - 方案 A（定案）：driver 在 jail root 内建 `snapshots/` 目录，把
    vmstate.snap/memory.snap **reflink 或硬链接**进去（文件不大/可共享：
    memory.snap 是 COW 读，硬链接共享安全），restore 参数用 chroot 内
    相对路径（`/snapshots/vmstate.snap` 等）；
  - 方案 B（备选）：jailer 前先在 jail root 内 bind-mount 缓存目录
    （fastlet 特权容器可 bind），restore 用 chroot 内路径——省复制但
    引入 bind 生命周期管理；
  - 采用 A（简单、无 bind 管理；memory.snap 硬链接 + COW 读取不污染
    缓存 base）。

### uid/gid 与清理

- `--uid 0 --gid 0`（Fastlet 特权容器）起步；专用 uid 作为生产加固后续；
- **清理**：sandbox 删除 → kill（jailer PID）→ 移除 jail 目录
  （`<chroot-base>/<exec-basename>/<id>/`，含 api.sock/rootfs.img/
  snapshots）→ netns 内 tap/规则随 netns 删除自动消失；
- **NodeJanitor**：residual-process 匹配（`--id <sandboxID[:32]>`）对
  jailer 进程本身同样成立（参数可见）。

## 数据面流量路径

### Ingress（host → guest，每实例唯一地址 = slot.IP）

```text
fastlet-proxy / 外部 → slot.IP（172.30.0.2，唯一）
  → host 路由（桥网段直连）→ hostVeth → slot netns eth0
  → netns PREROUTING DNAT：172.30.0.2 → 172.30.0.3（baked）
  → netns 路由（172.30.0.3/32 → vmtap0）→ tap → guest eth0
```

### Egress（guest → 外部，每实例 source IP = slot.IP）

```text
guest（src 172.30.0.3，共享）→ tap → netns 路由 → eth0
  → netns POSTROUTING MASQUERADE：172.30.0.3 → 172.30.0.2（slot.IP，唯一）
  → hostVeth → 桥 → host 路由 → host POSTROUTING MASQUERADE（出网）
  → 外部看到 src = 宿主出口 IP；host forward 点观察 src = slot.IP（唯一）
```

### 兄弟隔离

guest 转发流量走 netns **FORWARD** 链（不经 OUTPUT，现状 OUTPUT 隔离对
firecracker 空转）——netns 内新增 `FORWARD -d privateCIDR REJECT`（放行
到 eth0 的出站），补上现状 firecracker 在 host 桥上的隔离缺口。

## 数据面语义：不变 / 新增 / slot 字段

### 保持不变

- `AccessDescriptor` 仍是 slot.IP（ingress dial slot.IP，fastlet-proxy
  语义不变）；
- guest 仍持有 baked 地址（172.30.0.3，来自快照）；
- `network_overrides` 仍只换 tap 名（改为 netns 内固定名 `vmtap0`）；
- 出网仍走桥 + host POSTROUTING MASQUERADE；
- builder / 快照产物 / agent 零改动；
- `LinuxNetNSDriver`（container/gvisor/kata 基座）零改动。

### 新增/移动

| 项 | 位置（旧 → 新） |
|----|------------------|
| tap | host 桥（fc<hex>）→ **netns 内** `vmtap0`（固定名，`guestIP/32`） |
| DNAT（slot.IP→guest） | host PREROUTING → **netns 内 PREROUTING** |
| 出站源地址 | guest 直出（共享 IP）→ **netns 内 MASQUERADE → slot.IP**（必做，egress 兼容） |
| 兄弟隔离 | 无（现状缺口）→ **netns 内 FORWARD REJECT** |
| firecracker 进程 | host netns → **jailer `--netns` 进 slot netns**（+ chroot） |

### slot 字段语义变化

- `Slot.GuestTap`：语义从"host 桥接 tap 名"变为"**netns 内固定 tap 名
  `vmtap0`**"——字段保留（driver 读取作 `network_overrides` 值），
  不再由 IPAM 生成唯一名；
- 其余字段（IP/NetNSPath/HostVeth/Bridge/Gateway/PrivateCIDR/DNSPath/
  Access）语义不变。

## 每实例与共享

| 项 | 归属 |
|----|------|
| netns（fsb<hex>）、eth0=slot.IP、vmtap0、DNAT/MASQ/FORWARD 规则、jail 目录 | per-instance（现有 slot 生命周期 + sandbox 删除清理） |
| guest IP（172.30.0.3）、guest MAC | 共享（baked，netns 内安全） |
| 桥 fsb0、host MASQUERADE、hostVeth、缓存快照文件 | 共享（现有） |

## 与其他 runtime 的关系

| Runtime | 数据面 | 影响 |
|---------|--------|------|
| container / gvisor | 进程进 slot netns，eth0=slot.IP | **零影响**——`LinuxNetNSDriver` 不动；firecracker 新增的 tap/DNAT/MASQ/FORWARD 只在 firecracker 模式的 netns 内存在 |
| kata（GuestNetNS） | slot netns 挂给 guest NIC | **零影响**——netns 结构不变 |
| boxlite | gvproxy 隧道 | 零影响 |
| firecracker | host 桥接 → netns 内数据面 | 与 container 同构 |

**统一数据面边界**：每个 slot 只服务一个 sandbox、一个 runtime；数据面
按 runtime 类型选择（fastlet main 按 `NetworkMode` 实例化 driver）。方案
改动全部叠加在 fc 专属扩展（`GuestVMNetNSDriver` + `runtime/firecracker`），
不触碰共享基座——container/kata 的 netns 结构逐字节不变。落地后
firecracker 的**对外语义**（ingress dial slot.IP、egress src=slot.IP、
proxy/egress 视角）与 container 完全对齐。

## restore 启动衔接

```text
EnsureSandbox（restore 唯一路径，改造后）:
  1. agent.PinImage / LeaseDevices（不变，返回缓存路径）
  2. 网络 slot Acquire（不变；GuestVMNetNSDriver 现在准备 netns 内数据面）
  3. jail root 准备：mkdir <chroot-base>/<exec-basename>/<id>/root/
     ├─ rootfs.img     ← 实例 reflink 副本（原 prepareInstanceRootfs，
     │                    目标从 stateDir 改为 jail root）
     └─ snapshots/     ← vmstate.snap / memory.snap 硬链接（共享安全：
                        memory.snap COW 读，写不落文件）
  4. launcher：jailer --id <truncated> --netns <slotNetNSPath> --uid 0
     --gid 0 --exec-file <fc> --chroot-base-dir <base> --
     --api-sock /api.sock
  5. 等 API socket（宿主路径 <jailRoot>/api.sock）
  6. PUT /snapshot/load {
       snapshot_path: <chroot 内 /snapshots/vmstate.snap>,
       mem_backend: {File, <chroot 内 /snapshots/memory.snap>},
       network_overrides: [{iface: eth0, host_dev_name: vmtap0}],
       resume_vm: false }
  7. PATCH /vm Resumed + 轮询 Running（不变）
  8. 删除/失败 → kill（jailer PID）→ 清理 jail 目录 + ReleaseDevices +
     UnpinImage
```

## 与 OSEP-0022（多沙箱 egress 控制面）的兼容性

OSEP-0022 的核心前置假设：**egress 按 source IP 区分 subject**
（`ip saddr` dispatch；`SubjectKey = NetNSPath + SourceIP`；enforcement 在
Pod netns `hook forward`，"MASQUERADE happens at POSTROUTING, source IP
intact"）。container 模式天然成立（src=slot.IP）；**现状 firecracker 破坏
它**（guest 出网 src=共享 baked IP，host forward 处不可区分，与 bwrap
"无自有 IP"问题同构）。

**方案 2 + netns 内 MASQUERADE 恰好维持该不变式**：

| OSEP-0022 依赖 | 方案 2 下 | 结论 |
|---|---|---|
| dispatch key = source IP | netns MASQUERADE 后 = slot.IP，每实例唯一 | ✅ 本方案必做项 |
| slot store 字段（ip/netns/hostVeth/gateway/privateCidr/dnsPath） | 全部不变 | ✅ |
| hostVeth `iifname` 防 spoofing | 入口接口仍为 hostVeth→桥路径 | ✅ |
| Pod netns `hook forward` 主 enforcement | firecracker 流量同样过 forward（src=slot.IP） | ✅ |
| per-sandbox netns OUTPUT（防御） | 对 firecracker 空转（guest 走 FORWARD）——覆盖由 netns FORWARD 补上 | ⚠️ 设计差异，已对齐 |
| DNS proxy 绑 `<gateway>:53` + resolv.conf 重写 | guest DNS 流量 MASQUERADE 后 src=slot.IP → REDIRECT 正常 | ✅ |
| Kata（TAP 同 forward surface） | 不受影响 | ✅ |

egress 集成测试应包含 firecracker runtime 用例（N sandbox 不同 policy
在一 Pod 内，其中含 firecracker 模式），验证 source-IP dispatch 对
clone 同样工作。

## 改动面清单

| 文件 | 改动 |
|------|------|
| `internal/fastlet/network/guest_vm_linux_driver.go` | Prepare/Destroy/Validate：tap 移入 netns（固定名 `vmtap0`，`guestIP/32`，不再挂桥）；netns 内 PREROUTING DNAT、POSTROUTING MASQUERADE、FORWARD 隔离、ip_forward 的安装与清理 |
| `internal/runtime/firecracker/launcher.go` | 改为 **jailer** 启动（`--netns <slotNetNSPath>` + `--chroot-base-dir` + `--uid/--gid`），激活 `JailerPath`；`launchConfig` 增加 netns/chroot 字段；jail root 准备（rootfs 副本 + snapshots 硬链接） |
| `internal/runtime/firecracker/driver.go` | `configureRestoreVM`：snapshot/mem 用 chroot 内路径、`network_overrides` = `vmtap0`；`state.APIAddress` 宿主路径转换（`<base>/<exec-basename>/<id>/root/api.sock`）；`prepareInstanceRootfs` 目标 → jail root；删除清理 jail 目录 |
| `internal/runtime/firecracker/driver_test.go` | fake launcher/jailer 断言（netns/chroot 参数、路径转换） |
| `internal/runtime/firecracker/e2e_test.go` | 拓扑更新（tap 不再挂桥）；**Concurrent 用例恢复 per-instance reachability 断言**（issue 核心验收） |
| `scripts/firecracker-e2e.sh` / `firecracker-chain-e2e.sh` | 清理逻辑（jail 目录、确认 netns 删除后无 tap 残留）、断言更新 |
| `docs/guides/firecracker-runtime-e2e.md` | 网络拓扑章节重写（netns 内数据面 + jailer） |

## 测试计划

- **单元（guest_vm_linux_driver）**：Prepare 命令序列（netns 内 tuntap/
  addr、DNAT/MASQ/FORWARD/ip_forward 存在性）、Destroy 清理顺序、Validate
  （netns 内 tap 存在性）；
- **单元（launcher）**：jailer argv（`--netns`/`--chroot-base-dir`/
  `--id` 唯一性）、jail root 准备（rootfs 副本 + snapshots 硬链接）、
  API 路径转换；
- **单机集成**：单实例 restore → host ping slot.IP 可达（netns DNAT）；
  guest 出网（host forward 观察 src=slot.IP）；删除后 jail 目录与 netns
  清理干净；
- **E2E Concurrent（核心）**：5 VM 从同一快照集并发 restore →
  **每实例 per-slot reachability 断言恢复**（ARP 无冲突、DNAT 各自正确）；
- **egress 兼容（OSEP-0022 对齐）**：host forward 点观察 firecracker
  流量 src=slot.IP（`nft trace` 或 iptables 计数）；netns FORWARD 兄弟
  隔离生效；
- **回归**：container/gvisor/kata 既有网络测试全绿（`LinuxNetNSDriver`
  零改动保证）。

## 风险与已知问题

| 风险 | 影响 | 缓解 |
|------|------|------|
| jailer chroot 后文件可见性（vmstate/memory 需在 jail 内） | restore 失败 | 定案：snapshots 硬链接进 jail root（memory.snap COW 读共享安全） |
| netns 内 DNAT/conntrack 语义差异 | ingress 行为 | 单机集成先行验证；与现有 host DNAT 行为对比 |
| netns 删除时 tap/规则残留 | 资源泄漏 | Destroy 顺序（先删 tap/规则再删 netns）+ E2E 清理断言 |
| jail 目录残留（崩溃后） | 磁盘泄漏 | sandbox 删除清理 + NodeJanitor 兜底（jailer 进程匹配） |
| 硬链接跨文件系统失败 | snapshots 准备失败 | 回退复制（memory.snap 复制成本可接受：COW 读场景一次性） |
| egress 集成未含 firecracker 用例 | OSEP-0022 覆盖缺口 | egress 测试计划补充（跨仓库协作项） |
| 共享 guest MAC 的 DNS/应用层身份共享 | 产品语义 | 接受（clone 模型固有），文档记录 |

## 验证项

### 已完成（真机，参考机）

1. ~~firecracker `--netns` 可用性~~ **已验证（v1.16.1）：不支持**——
   载体定为 jailer `--netns`；
2. **jailer `--netns` 完整行为** **已验证**：
   - 进程进入目标 netns（ns/net inode 与 `/var/run/netns/<ns>` 一致，
     4026536959）；
   - chroot 目录结构 `<base>/<exec-basename>/<id>/root/`；
   - API socket 可用（`/version` 200）；
   - 非 daemonize 时 jailer exec firecracker（PID 不变）；
   - 参数细节：`--chroot-base-dir`（位置参数被拒）、`--id` 自动传递
     （重复传入被拒）。

### 施工期验证

- netns 内 tap 经 `network_overrides` 被 restore 打开（E2E）；
- jail root 内 snapshots 硬链接 + chroot 内路径 restore 成功；
- Concurrent per-instance reachability。

## 决策记录与待决策项

### 决策记录

| 决策 | 结论 | 备注 |
|------|------|------|
| 方案选择 | **per-clone netns**（上游模型） | 方案 1（DNAT 对齐）与方案 3（guest 重配）否决，理由见方案评估 |
| 进程进 netns 载体 | **jailer `--netns`** | firecracker 二进制无 `--netns`（真机验证）；顺带激活 JailerPath/chroot 隔离 |
| netns 内 MASQUERADE | **必做**（guest baked IP → slot.IP） | OSEP-0022 source-IP dispatch 前置 |
| tap 命名 | **固定 `vmtap0`**（netns 内无冲突） | `Slot.GuestTap` 字段保留，语义变为固定名 |
| MASQUERADE 粒度 | **仅 firecracker 模式**（`GuestVMNetNSDriver` 内） | 不触碰 container/kata 路径 |
| 实例 rootfs / snapshots 位置 | **jail root 内**（rootfs.img 副本 + snapshots 硬链接） | chroot 文件可见性约束的解法 |
| 兄弟隔离 | **netns 内 FORWARD REJECT** | 补现状 firecracker 在 host 桥上的隔离缺口 |

### 待决策项

1. **与 OSEP-0022 的联调归属**：egress 集成测试由 egress 侧补
   firecracker 用例（推荐）vs 本方案同步补（需跨仓库协作）；
2. **jailer uid/gid 加固**：`--uid 0 --gid 0` 起步 vs 生产直接上专用
   uid（涉及 jail root 目录属主与 Fastlet 容器权限）。
