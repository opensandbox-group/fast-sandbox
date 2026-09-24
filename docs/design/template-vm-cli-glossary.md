# Glossary: template-vm CLI

> 文档类型：术语表
>
> 日期：2026-09-10
>
> 配套 ADR：[adr-template-vm-cli.md](./adr-template-vm-cli.md)

本术语表统一「从远程 template 创建 VM」的 CLI 工具（下称 **template-vm**）涉及的
全部领域名词。

## 输入与标识

| 术语 | 定义 |
|------|------|
| **template.json** | CLI 的唯一输入清单。字段：`sandbox_id`、`template.{repo, template_id, auth}`、`metadata`（透传的用户元数据，当前为空语义，预留）。 |
| **Template（模板）** | 远程 OCI registry 中**一对** overlaybd 镜像的合称，由 `repo + template_id` 唯一标识。不含 K8s SandboxTemplate CRD 语义（本工具不经过任何控制面）。 |
| **rootfs 镜像** | `${repo}:${template_id}_rootfs`。overlaybd 格式，承载 VM 系统盘全部数据，恢复时以 virtio-blk 通入 guest。 |
| **snapfiles 镜像** | `${repo}:${template_id}_snapfiles`。overlaybd 格式，盘内为 ext4 文件系统，存放恢复 VM 必需的描述信息与内存状态（见「盘内布局约定」）。 |
| **sandbox_id** | 用户在 template.json 中指定的本地唯一标识（如 `tianping-test`），是本地状态目录、ublk 设备归属、list/delete 的主键。 |

## 镜像与设备

| 术语 | 定义 |
|------|------|
| **overlaybd** | 按需加载的块设备格式（LSMT 索引 + 数据段），支持从 registry 按 range 远程拉块。项目：<https://github.com/containerd/overlaybd>。 |
| **config.v1.json** | overlaybd 设备配置文件，结构为 `OverlayBDBSConfig{lowers[], upper?, repoBlobUrl}`（定义见 accelerated-container-image `pkg/types/types.go`）。CLI 负责依据镜像 manifest 生成它。无 `upper` = 只读设备；有 `upper` = 可写设备。 |
| **overlaybd-ublkd** | overlaybd 官方集中式 ublk daemon（单进程托管多设备，共享一棵 cache 树）。控制 API 为 **HTTP over root-only unix socket**（默认 `/var/run/overlaybd-ublk/ublkd.sock`）：`POST /v1/add {config}` / `POST /v1/del {dev_id}` / `GET /v1/list` / `POST /v1/shutdown`。 |
| **ublk 设备** | `/dev/ublkbN`，由 overlaybd-ublkd 创建。rootfs 与 snapfiles 各占一个。 |
| **upper（可写层）** | rootfs 设备的可写顶层，使用 LSMT **hybrid** 模式（`RWType::Hybrid`，`FLAG_HYBRID_RW=5`，与 sparse_rw 互斥）。每 sandbox 一份本地 data/index 文件。默认**不 commit、不推回 registry**；sandbox 快照请求触发的 commit 回写属后续迭代，本期不实现。 |
| **lower（只读层）** | overlaybd 镜像在 registry 中的各数据层，以 digest 寻址，写入 config.v1.json 的 `lowers[]`。 |

## 快照恢复

| 术语 | 定义 |
|------|------|
| **vmstate.bin** | VMM 私有序列化的虚拟机状态（vCPU 寄存器、设备状态、VM 配置）。版本敏感：恢复侧 VMM 版本必须与拍摄侧兼容。 |
| **memfile** | guest 物理内存的**裸文件**（非 LSMT 层）。恢复时 VMM 直接 `mmap` 它；按需加载发生在块层：page fault → ext4 → ublk → overlaybd 远端 range read。 |
| **metadata.json** | snapfiles 盘内的 VM 规格**权威**来源。字段含 `vcpu_count / memory_mb / disk_size_mb / firecracker_version / kernel_version / hypervisor_type / network{host_dev_name, mac} / cpu_arch / cpu_model / envd_version / default_user / default_workdir / created_at` 等。CLI flag 可覆盖其中规格类字段。 |
| **hypervisor_type** | metadata.json 字段，决定恢复路径分派。当前仅实现 `firecracker`；`dragonball` 等其他值报 `Unsupported`（预留 VMM 抽象）。 |
| **envd** | guest 内 agent（OpenSandbox 运行时），metadata.json 记录其版本；本期 CLI 不依赖 exec 通道，字段仅作记录与兼容校验参考。 |

## 布局与运行时

| 术语 | 定义 |
|------|------|
| **盘内布局约定** | snapfiles ublk 设备（ext4）挂载后的固定路径：`/vmstate.bin`、`/memfile`、`/metadata.json`。为既有规范，CLI 适配之。 |
| **状态目录（home）** | `/var/lib/template-vm/sandboxes/<sandbox_id>/`：upper data/index、config.v1.json、snapfiles 挂载点、状态记录文件（pid、dev_id、挂载点）。持久，delete 时清理。 |
| **运行目录（runtime）** | `/run/template-vm/sandboxes/<sandbox_id>/`：firecracker API socket、serial 日志。易失，宿主机重启即失效。 |
| **kernel 解析** | metadata.json 的 `kernel_version` → 宿主机本地 vmlinux 路径（约定目录 + flag 覆盖）。kernel 不随镜像分发。 |
| **凭证转写** | template.json 的 `auth.dockerAuth`（docker config json 字符串）→ overlaybd 全局凭证文件（默认 `/opt/overlaybd/cred.json`，`credentialFilePath`）。CLI 负责合入，否则按需拉块返回 401。 |
| **既有网络体系** | metadata.json `network.host_dev_name`（形如 `cnid-*`）所属的网络栈。CLI 通过 NetworkProvider 抽象对接；接口规范待网络侧提供，MVP 支持 `--network=none` 跳过。 |
