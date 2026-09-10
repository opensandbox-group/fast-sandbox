# ADR: template-vm CLI —— 从远程 template 创建 VM 的独立 CLI

> 文档类型：架构决策记录（ADR）
>
> 日期：2026-09-10
>
> 状态：已确认（三轮设计访谈收敛）
>
> 配套术语表：[template-vm-cli-glossary.md](./template-vm-cli-glossary.md)

## 背景

需要一个独立 CLI 工具：输入一份 `template.json`（registry repo + template_id +
dockerAuth），把存放在远程 OCI registry 中的模板（一对 overlaybd 镜像：
`${repo}:${template_id}_rootfs` 与 `${repo}:${template_id}_snapfiles`）在本机
恢复为一台运行中的 VM。创盘走 overlaybd-ublkd, VMM 恢复路径参考 AgentENV 或其他
sandbox项目的实现。

本工具**不经过任何控制面**（不碰 fast-sandbox 的 fastpath/CRD/reconciler），
是单机数据面工具。

## 决策清单

### ADR-001 代码落点：fast-sandbox 仓库内独立二进制

- **决策**：代码放 `fast-sandbox/cmd/template-vm/`（Go，cobra），与 fastctl 平级；
  不挂进 fastctl 子命令树，不复用 fastpath gRPC。
- **理由**：用户明确「独立 CLI + 放 fast-sandbox 仓库」。与 fast-sandbox 控制面
  零交互，独立二进制避免 `internal/architecture/dependencies_test.go` 的分层
  规则把数据面依赖（oras、ublkd client）洩入控制面。
- **取舍**：无法复用 fastctl 的 endpoint/config 基建——本工具也不需要（无服务端）。

### ADR-002 创盘通道：overlaybd-ublkd（HTTP over unix socket）

- **决策**：调用 overlaybd-ublkd 的控制 API：`POST /v1/add {"config": <path>} /
  POST /v1/del {"dev_id": N} / GET /v1/list`，socket 默认
  `/var/run/overlaybd-ublk/ublkd.sock`（flag 可覆盖）。
- **理由**：用户指定，参考 overlaybd 官方
  [standalone-usage.md](https://github.com/containerd/overlaybd/blob/main/docs/standalone-usage.md#overlaybd-ublkd-many-devices-in-one-process)。
- **排除项**（调研结论）：
  - AgentENV `uvm-ublk-daemon`：unix socket + JSON RPC（`DaemonRequest::CreateOverlaybd`），
    非 HTTP，且与 AgentENV 的 image.json/config 模型耦合；
  - accelerated-container-image `overlaybd-attacher`：tcmu 路径，非 ublk。
- **部署假设**：ublkd 由 systemd 预部署（官方 unit 模板
  `/opt/overlaybd/overlaybd-ublkd.service`）；CLI 启动时仅做 socket 可达性检查，
  不负责拉起 daemon。
- **注意**：`resize` 需要内核 6.11+（UBLK_F_UPDATE_SIZE），本期不用；daemon 持有
  的设备必须经其 API 删除；writable 镜像二次挂载会被 daemon 拒绝（upper 排他保护）。

### ADR-003 模板解析：oras 拉 manifest，CLI 生成 config.v1.json

- **决策**：CLI 用 oras/distribution client + template.json 的 dockerAuth 拉取两个
  镜像的 manifest，按 accelerated-container-image 的方式生成
  `config.v1.json`（`OverlayBDBSConfig{lowers[], upper?, repoBlobUrl}`，
  见 `pkg/types/types.go`）。
- **细节**：rootfs 的 config 含 `upper`（本地 hybrid 可写层，先创建空 data/index
  再 add）；snapfiles 的 config 无 `upper`（只读）。`repoBlobUrl` 指向
  `https://<registry>/v2/<repo>/blobs`，overlaybd 运行时按 range 拉块。
- **凭证转写**：dockerAuth（docker config json）合入 overlaybd 全局凭证文件
  （默认 `/opt/overlaybd/cred.json`，overlaybd 按 remote_path 惰性 reload），
  否则按需拉块 401。合入需原子写 + 备份，避免并发 CLI 实例互相覆盖。

### ADR-004 kernel：宿主机本地，metadata 校验 + flag 覆盖

- **决策**：kernel 不随镜像分发。`metadata.json.kernel_version` 映射到宿主机约定
  目录下的 vmlinux（默认约定 `/opt/template-vm/kernels/vmlinux-<kernel_version>`，
  可用 `--kernel` 显式覆盖）；文件缺失则报错。

### ADR-005 memfile：裸内存文件 mmap（Firecracker Backend File）

- **决策**：snapfiles 盘只读挂载后，firecracker `PUT /snapshot/load` 使用
  `mem_backend = {backend_type: "File", backend_path: <mnt>/memfile}`。
- **理由**：memfile 位于 ublk 块设备上的 ext4 内，page fault
  沿 ext4 → ublk → overlaybd 远端 range read 链路惰性拉取；guest 首写在
  firecracker 进程地址空间内 COW 为匿名页，不污染只读层。

### ADR-006 盘内布局约定：ext4 + 固定路径

- **决策**：snapfiles 设备为 ext4，挂载后固定路径 `/vmstate.bin`、`/memfile`、
  `/metadata.json`。**既有规范，CLI 适配**；构建侧改动需版本化公告。
- **风险**：规范目前仅有口头约定，联调时需用真实样例镜像验证路径与 FS 类型。

### ADR-007 规格来源：metadata.json 权威 + flag 覆盖

- **决策**：`vcpu_count / memory_mb / disk_size_mb` 等以 metadata.json 为准；
  CLI 提供 `--vcpu / --memory-mb / --kernel` 覆盖。启动前校验：
  `hypervisor_type` 当前仅支持 `firecracker`（其他值报 Unsupported，预留 VMM
  抽象接口）；`cpu_arch` 与宿主一致；本机 firecracker 二进制版本与
  `firecracker_version` 兼容（不兼容默认报错，`--force` 可降级为警告）。

### ADR-008 rootfs upper：hybrid 可写层，默认不 commit

- **决策**：每 sandbox 一份 LSMT hybrid 模式 upper（data/index 存于
  状态目录），VM 销毁即丢弃。sandbox 快照触发的 `overlaybd-commit` + 推回
  registry 流程**本期不实现**，接口位置预留。

### ADR-009 命令面与生命周期：create / list / delete

- **决策**：参考 aenv 语义（start = 从模板直接拉起，不区分 start/resume），
  MVP 三命令：
  - `template-vm create -f template.json [--kernel ...] [--vcpu ...] [--network=none]`
  - `template-vm list`：枚举状态目录 + 校验 pid 存活
  - `template-vm delete <sandbox_id>`：杀 firecracker → umount snapfiles →
    ublkd `del` 两个设备 → 清理状态目录（顺序严格逆序，任何一步失败都报告并继续
    尽最大努力清理）
- **布局**：状态目录 `/var/lib/template-vm/sandboxes/<id>/`（upper、
  config.v1.json、挂载点、state.json）；运行目录 `/run/template-vm/sandboxes/<id>/`
  （api socket、serial 日志）。

### ADR-010 网络：NetworkProvider 抽象，MVP 支持 none

- **决策**：metadata.json 的 `network{host_dev_name, mac}` 属既有网络体系
  （`cnid-*`）。CLI 定义 NetworkProvider 接口（Acquire(metadata) → 设备 +
  清理回调），具体对接规范**待网络侧提供**；MVP 默认 `--network=none` 跳过，
  不阻塞快照恢复主链路。
- **未决输入**：网络体系的设备分配接口（待用户提供后另起 ADR 补充）。

## create 主流程（编排顺序，严格分步、失败逆序回滚）

1. 解析 template.json；校验 sandbox_id 未占用（状态目录不存在）。
2. oras 拉取 rootfs / snapfiles 两个 manifest → 层 digest 列表。
3. dockerAuth 合入 `/opt/overlaybd/cred.json`（原子写）。
4. rootfs：创建 hybrid upper（data/index）→ 生成含 upper 的 config.v1.json →
   ublkd `/v1/add` → 记录 dev_id。
5. snapfiles：生成只读 config.v1.json → `/v1/add` → mount -o ro →
   读 metadata.json / 校验（ADR-007）。
6. 解析 kernel 路径（ADR-004）；启动匹配版本的 firecracker（api socket 于
   运行目录）。
7. 恢复：`PUT /drives/rootfs`（path=/dev/ublkbN，drive_id 与 vmstate 一致；
   不一致则先 PATCH）→ `PUT /snapshot/load`（vmstate.bin + memfile File backend）
   → 网络（NetworkProvider 或 none）→ `PATCH /vm` resume。
8. 落状态文件，输出 sandbox 信息（id、pid、ublk 设备、socket 路径）。

## 风险与缓解

| 风险 | 缓解 |
|------|------|
| hypervisor_type=dragonball 的存量模板无法恢复 | 启动前硬校验 + 明确报错；VMM 抽象预留 |
| vmstate 版本与宿主 firecracker 不兼容 | `firecracker_version` 校验；`--force` 逃生门 |
| 盘内布局仅口头约定 | 联调用真实镜像验证；路径集中为一个常量区块 |
| cred.json 并发覆盖 | 原子写（tmp+rename）+ 文件锁 |
| ublkd 设备泄漏（CLI 崩溃后） | state.json 记录 dev_id；delete 幂等；后续可加 janitor |
| 内核要求 | ublk 基础能力即可（overlaybd 官方在 5.10 backport 内核验证过；6.11+ 仅 resize 需要） |
