# 施工方案：per-clone netns 网络改造（jailer 载体）

> 文档类型：施工任务书（转交实现）
>
> 日期：2026-08-29
>
> 前置：设计定案见
> [firecracker-clone-network.md](firecracker-clone-network.md)（含真机验证
> 结论：jailer `--netns` 载体、chroot 目录结构、API 路径转换、决策记录 6 项）。
> 关联：opensandbox-group/fast-sandbox#26。
>
> 实现人按本任务书独立完成，有分歧以本任务书 + 设计文档为准。

---

## 1. 背景与目标

golden restore 成为唯一启动路径后（PR #25），guest 网络身份 baked 在
快照里（共享 IP/MAC），与 fastlet slot 的 `slot.IP+1` DNAT 模型漂移：
多实例 ingress 不可达、共享桥 ARP 冲突、OSEP-0022 的 source-IP dispatch
被破坏。本任务实现 **per-clone netns 数据面**（上游 clone 模型）：
firecracker 进程经 **jailer `--netns`** 进 slot netns，tap/DNAT/MASQUERADE
全部移入 netns 内，guest 共享 baked IP/MAC 在 netns 内安全共存。

**目标验收（issue #26 闭环）**：

- 多实例从同一快照集并发 restore 互不冲突；
- 每实例 ingress（dial slot.IP）与 egress（host forward 处 src=slot.IP）
  正确；
- E2E `TestFirecrackerDriverE2EConcurrent` **恢复 per-instance
  reachability 断言**；
- container/gvisor/kata/boxlite 零影响（`LinuxNetNSDriver` 不动）。

**范围边界（本次不做）**：

- 不动 `LinuxNetNSDriver`（container/kata 基座）；
- 不动 builder / 快照产物 / agent（guest IP/MAC 保持 baked）；
- 不改 fastlet API / slot store 字段（`GuestTap` 语义变化见 §4）；
- 不动 OSEP-0022（egress 联调为跨仓库后续项，见 §8 待决策）。

## 2. 代码位置与改动点

| 文件 | 改动 |
|------|------|
| `internal/fastlet/network/guest_vm_linux_driver.go` | Prepare/Destroy/Validate：tap 移入 netns（固定名 `vmtap0`），netns 内 DNAT/MASQUERADE/FORWARD/ip_forward 规则（§3） |
| `internal/runtime/firecracker/launcher.go` | jailer 化启动 + jail root 准备（§4） |
| `internal/runtime/firecracker/driver.go` | `configureRestoreVM` chroot 路径 + `vmtap0`；`prepareInstanceRootfs` 目标 → jail root；`state.APIAddress` 宿主路径转换；删除清理 jail 目录 |
| `internal/runtime/firecracker/driver_test.go` | fake launcher/jailer 断言 |
| `internal/runtime/firecracker/e2e_test.go` | 拓扑更新 + Concurrent per-instance reachability 恢复 |
| `scripts/firecracker-e2e.sh` / `firecracker-chain-e2e.sh` | 清理逻辑（jail 目录）、断言 |
| `docs/guides/firecracker-runtime-e2e.md` | 网络拓扑章节重写 |

## 3. GuestVMNetNSDriver 改造（netns 内数据面）

### Prepare（在 `LinuxNetNSDriver.Prepare` 之后）

```text
# tap 进 netns（固定名 vmtap0，guestIP/32，不再挂 host 桥）
ip netns exec <ns> ip tuntap add dev vmtap0 mode tap
ip -n <ns> addr add <guestIP>/32 dev vmtap0
ip -n <ns> link set vmtap0 up
# netns 内转发
ip -n <ns> sysctl -w net.ipv4.ip_forward=1
# netns 内 DNAT：slot.IP → guestIP（ingress）
ip netns exec <ns> iptables -t nat -A PREROUTING -d <slot.IP>/32 -j DNAT --to-destination <guestIP>
# netns 内 FORWARD：出站（tap→eth0）放行；回包（conntrack）放行；兄弟隔离在前
ip netns exec <ns> iptables -A FORWARD -d <privateCIDR> -j REJECT            # 兄弟隔离（先插）
ip netns exec <ns> iptables -A FORWARD -i vmtap0 -o eth0 -j ACCEPT          # guest 出站
ip netns exec <ns> iptables -A FORWARD -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT  # 回包
# netns 内出站源地址：guest（共享 baked）→ slot.IP（唯一，OSEP-0022 前置）
ip netns exec <ns> iptables -t nat -A POSTROUTING -s <guestIP>/32 -j SNAT --to-source <slot.IP>
```

- `guestIP` 与 `slot.IP` 均来自 slot（`GuestVMIP(slot)` 现状推导
  `slot.IP+1`，保持与 baked guest IP 的约定一致）；
- 规则幂等：先 `-C` 检查再 `-A`（对齐现状 GuestVMNetNSDriver 的
  check-then-add 模式）；
- 命令执行统一走 `d.runner`（fake 可注入，测试断言序列）。

### Destroy（逆序清理）

```text
删除 netns 内规则（DNAT/SNAT/FORWARD×3，-D）
ip netns exec <ns> ip link del vmtap0（或随 netns 删除自动消失——先删规则，
  再走 LinuxNetNSDriver.Destroy 删 netns；tap 随 netns 消失无需显式删）
```

- 顺序保证：**规则 → netns**（netns 删除前规则先清，避免半残留）；
- 幂等：`-D` 失败忽略缺失资源（对齐 `isMissingNetworkResource`）。

### Validate

- 新增断言：netns 内 `vmtap0` 存在（`ip -n <ns> link show vmtap0`）；
- 保留既有 tap 校验（改为 netns 内路径）。

## 4. launcher jailer 化

### launchConfig 扩展

```go
type launchConfig struct {
    BinaryPath string
    JailerPath string            // 激活 FirecrackerConfig.JailerPath
    ChrootBase string            // <StateRoot>/jails
    NetNSPath  string            // slot.NetNSPath（/run/netns/fsb<hex>）
    SandboxID  string            // 截断 32 位（NodeJanitor 约定）
    APIAddress string            // chroot 内 /api.sock
    LogPath    string            // chroot 内 /fc.log
}
```

### argv 构造（真机验证结论）

```text
<jailer> --id <sandboxID[:32]> --netns <NetNSPath> --uid 0 --gid 0 \
         --exec-file <firecracker> --chroot-base-dir <ChrootBase> -- \
         --api-sock /api.sock --log-path /fc.log ...
```

- **`--id` 只传一次**（jailer 自动传递给 firecracker，`--` 后重复会被拒）；
- **API 宿主路径**：`<ChrootBase>/<exec-basename>/<id>/root/api.sock`
  （`exec-basename` = firecracker 二进制的 basename，实测为 `firecracker`）；
- 非 daemonize（jailer exec firecracker，PID 不变——进程管理/恢复语义
  与现状一致）。

### jail root 准备（launcher 内，启动前）

```text
jailRoot := <ChrootBase>/<exec-basename>/<sandboxID[:32]>/root
mkdir -p <jailRoot>/snapshots
rootfs.img   ← 实例 reflink 副本（原 prepareInstanceRootfs 的目标改为这里）
snapshots/vmstate.snap ← 硬链接（缓存文件；失败回退复制）
snapshots/memory.snap  ← 硬链接（COW 读安全，写不落文件；失败回退复制）
```

- `prepareInstanceRootfs` 的目标目录从 `stateDir` 改为 `jailRoot`；
- 硬链接失败回退复制（跨文件系统），日志告警。

## 5. driver 改造

### configureRestoreVM（restore 参数）

```go
client.LoadSnapshot(ctx, SnapshotLoadRequest{
    SnapshotPath: "/snapshots/vmstate.snap",          // chroot 内路径
    MemBackend:   {BackendType: "File", BackendPath: "/snapshots/memory.snap"},
    ResumeVM:     false,
    NetworkOverrides: []SnapshotNetworkOverride{{IfaceID: "eth0", HostDevName: "vmtap0"}},
})
```

- **snapshot/mem 用 chroot 内相对路径**（jailer chroot 后 firecracker
  只能见 jail root 内文件）；
- `network_overrides` 的 tap 名 = `vmtap0`（netns 内固定名）；
- `state.APIAddress` 宿主路径：`jailerSocketPath(chrootBase, execBase, id)`
  （等待 socket 用）；driver 内部 curl 用该宿主路径。

### 清理（DeleteSandbox / 失败路径）

```text
kill（jailer PID，现状 killAndForget）
移除 jail 目录 <ChrootBase>/<exec-basename>/<id>/（含 rootfs.img/snapshots/api.sock）
ReleaseDevices + UnpinImage（不变）
netns 内 tap/规则随 slot Destroy 清理（§3）
```

- 顺序：先 kill（VM 停止）→ 再删 jail 目录 → 再走现有 slot 释放；
- 崩溃残留：NodeJanitor 按 jailer `--id` 匹配兜底（现有机制）。

## 6. 测试计划

### 单元

- **guest_vm_linux_driver**：fake runner 断言 Prepare 命令序列（tuntap
  in netns、addr、DNAT/SNAT/FORWARD 的 check-then-add、ip_forward）、
  Destroy 逆序、Validate（netns 内 tap）；
- **launcher**：jailer argv 断言（`--netns`/`--chroot-base-dir`/`--id`
  唯一性、`--` 后无重复 id）、jail root 准备（rootfs 副本 + snapshots
  硬链接存在）、`jailerSocketPath` 转换；
- **driver**：fake client 断言 `snapshot/load` 载荷（chroot 路径 +
  `vmtap0`）、`state.APIAddress` 转换、删除清理（jail 目录移除）。

### 单机集成（KVM，参考机）

- 单实例 restore → host ping slot.IP（netns DNAT ingress）；
- guest 出网：host forward 点 `iptables -L -v` 计数或 `nft` trace 观察
  src=slot.IP（netns SNAT 生效）；
- 删除后：jail 目录消失、netns 删除无 tap 残留。

### E2E（firecracker build tag）

- **`TestFirecrackerDriverE2EConcurrent` 恢复 per-instance
  `assertGuestReachable`**（issue #26 核心验收：5 VM 并发 restore，每
  实例 slot.IP 可达）；
- 既有 E2E（单实例/NoInfra/ImageGC）全绿（拓扑更新后）。

### 回归

- container/gvisor/kata 网络相关测试全绿（`LinuxNetNSDriver` 零改动）；
- 全链路 E2E（`firecracker-chain-e2e.sh`）保持绿色（网络拓扑更新后）。

## 7. 验收标准

1. `go build ./...`、`go test ./internal/fastlet/network/...
   ./internal/runtime/firecracker/... -count=1 -race`、`go vet ./...`
   全绿；
2. E2E Concurrent per-instance reachability 断言恢复且通过；
3. 单机集成：ingress（ping slot.IP）+ egress（host forward src=slot.IP）
   验证通过；
4. container/kata 回归全绿（零改动证明）；
5. 不引入新三方依赖；
6. 设计文档决策记录项全部落实（§4-5 与决策记录一致）。

## 8. 参考材料与待决策项

- 设计：`docs/design/firecracker-clone-network.md`（真机验证结论、流量
  路径、决策记录）；
- 上游：firecracker `docs/snapshotting/network-for-clones.md`（v1.16.1）；
- 现状代码：`internal/fastlet/network/{linux_driver,guest_vm_linux_driver,
  types}.go`、`internal/runtime/firecracker/{launcher,driver}.go`；
- OSEP-0022（egress 联调：集成测试补 firecracker 用例为跨仓库待决策项）。

**待决策项（实现不阻塞）**：

1. egress 集成测试补 firecracker 用例的归属（egress 侧 vs 本方案）；
2. jailer `--uid/--gid` 生产加固（专用 uid vs root 起步）。

## 9. 交付物

- 上述代码改动 + 单测 + E2E 更新；
- PR 描述：netns 规则表、jailer argv、路径转换、E2E 结果
  （Concurrent per-instance reachability 实测数据）、与设计文档的
  一致性说明。
