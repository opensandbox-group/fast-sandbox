# manifest.json 字段参考（中文）

`manifest.json` 是发布到 artifact store 的制品集的元数据文档。SandboxTemplate
构建、SandboxSnapshot、暂停 checkpoint 三类制品集共用同一 schema；整个文档
的 SHA-256 就是该制品集的 `artifactDigest`，变更任何字节都会形成新的一套制品。

## 完整示例

```json
{
  "schemaVersion": 1,
  "runtime": "firecracker",
  "sourceImage": "registry.example.com/sandbox:v1.0.21",
  "sourceImageDigest": "sha256:4d05…",
  "execd": "ghcr.io/opensandbox/execd@sha256:9f1c…",
  "kernel": {
    "name": "vmlinux-6.1.177",
    "digest": "3e8d…"
  },
  "machine": { "vcpu": "2", "memory": "2Gi" },
  "compatibility": {
    "firecrackerVersion": "1.16.1",
    "hostKernel": "5.10.134-18.al8.x86_64",
    "cpuModel": "Intel(R) Xeon(R) Platinum 8163 CPU @ 2.50GHz"
  },
  "guestNetwork": {
    "iface": "eth0",
    "mac": "02:00:00:00:00:01",
    "ip": "172.30.0.3",
    "gateway": "172.30.0.1",
    "netmask": "255.255.255.0",
    "mtu": 1500
  },
  "entrypoint": ["/opt/gem/run.sh"],
  "init": "/usr/local/sbin/sandbox-init",
  "envs": [{ "name": "FOO", "value": "bar" }],
  "rootfsSize": "30G",
  "format": "overlaybd",
  "files": {
    "rootfs.ext4":  { "sha256": "a1b2…", "sizeBytes": 32212254720 },
    "vmstate.snap": { "sha256": "c3d4…", "sizeBytes": 1835008 },
    "memory.snap":  { "sha256": "e5f6…", "sizeBytes": 2147483648 },
    "overlaybd/rootfs/layer.lsmt":  { "sha256": "…", "sizeBytes": … },
    "overlaybd/memory/layer.lsmt":  { "sha256": "…", "sizeBytes": … }
  },
  "validation": { "booted": true, "restored": true }
}
```

## 字段释义

| 字段 | 含义与作用 |
| --- | --- |
| `schemaVersion` | manifest 模式版本，当前恒为 `1`。前向兼容标记，消费端目前不据此分支 |
| `runtime` | 产出该制品集的运行时，当前恒为 `"firecracker"`——声明这套 vmstate/memory 只能被 Firecracker 驱动恢复 |
| `sourceImage` | 来源镜像引用。模板构建写入模板的 `spec.image`；快照/checkpoint 覆盖为拍摄该 Sandbox 当时的 `image` 引用（若它是从模板名创建的，就是那个模板名） |
| `sourceImageDigest` | 初次构建时源 OCI 镜像的 digest。快照/checkpoint 不覆盖此字段，因此跨代快照保留的是最初构建的 digest，是整个谱系的锚点 |
| `execd` | 构建时注入 guest rootfs 的 OpenSandbox execd 镜像引用。注入发生在构建期，此字段是谱系记录（说明 rootfs 里带着哪个 execd），运行时不重新解析 |
| `kernel` | guest 内核：`name` 为构建镜像内置的内核文件名，`digest` 为该文件的 SHA-256。内核是构建期资产（不是节点运行时资产），快照逐字继承此字段；恢复时使用的内核由节点 runtime 环境提供，digest 用于核对 |
| `machine` | 快照的机器规格：`vcpu`、`memory` 为 resource quantity（如 `"2"`、`"2Gi"`）。这是恢复的权威配置——Firecracker 拒绝以不同于 vmstate 创建时的内存恢复，因此创建请求的 cpu/mem 只做校验：请求内存低于快照内存会被显式拒绝。快照逐字继承源镜像的 `machine` |
| `compatibility` | 拍摄环境三元组：`firecrackerVersion`（Firecracker 二进制版本）、`hostKernel`（宿主机内核 `uname -r`）、`cpuModel`（宿主机 CPU 型号）。用于排障，以及设计上"快照能在哪些节点恢复"的匹配依据；当前恢复路径不强制校验，属信息性字段。每次拍摄重新写入，不继承 |
| `guestNetwork` | 烘焙进快照的客户机静态网络（克隆网络模型）：`iface`/`mac`/`ip`/`gateway`/`netmask`/`mtu`。恢复时客户机侧不变（都在内存镜像里），节点侧只替换 host tap；消费端读取 `ip`/`gateway`/`netmask`/`mtu`——`mtu` 为 0 表示老格式清单，回退内核默认。同样逐字继承 |
| `entrypoint` | 模板声明的业务命令 argv。构建期已写进 rootfs 启动链，这里逐字发布作谱系/审计 |
| `init` | 注入的 guest PID 1 路径（默认 `/usr/local/sbin/sandbox-init`；空表示不注入，由镜像自己的 init 负责）。构建期事实的记录 |
| `envs` | 模板 envs 逐字发布（不做 OCI `Config.Env` 合并，不支持 `valueFrom`），构建期写入 guest 的 `/etc/sandbox-init.env`。注意不要放机密——任何能读到 manifest 的人都能看到这些值，凭据应走 `publishSecretRef` |
| `rootfsSize` | 实际 rootfs 容量：声明的最小值向上取整到 SI GiB，形如 `"30G"`，与 `files["rootfs.ext4"].sizeBytes` 一致，供消费端对齐声明容量与真实制品 |
| `format` | `native` 或 `overlaybd`，两种格式都包含完整快照集，区别是有无额外的 LSMT 层（用于按需加载）。快照/checkpoint 恒为 `native` |
| `files` | 制品逐文件清单：键为制品相对名，值为 `{sha256, sizeBytes}`——sparse-aware 摘要与逻辑大小。拉取侧据此逐文件下载并校验摘要（已校验的文件跳过，损坏的重拉）。固定成员：`rootfs.ext4`（本地缓存中改名 `rootfs.img`）、`vmstate.snap`、`memory.snap`；模板构建且 `format=overlaybd` 时追加 `overlaybd/rootfs/layer.lsmt`、`overlaybd/memory/layer.lsmt` |
| `validation` | 拍摄期验证标记：`booted` 表示该镜像/快照成功启动过；`restored` 表示额外做过恢复验证（模板构建为 true，live dump 恒为 false——vmstate 本身就是运行态证据）。信息性字段 |
| `actionBindings` | 仅快照/checkpoint 且非空时写入：拍摄时源 Sandbox 的动作绑定（`handler` + `input`），最典型是 egress 网络策略。制品集因此自包含——删掉 SandboxSnapshot CR 不影响制品，策略跟着制品走。`CreateSandbox(image=<templateName>)` 时 fastpath 会读取并合并进初始绑定：显式传入的同 handler 覆盖、目标 Pool 未声明的 handler 丢弃、store 不可达则跳过。构建产物（golden image）没有此字段 |

相关文档：[SandboxTemplate golden images](sandboxtemplate-golden-images.md)、
[Sandbox Snapshots](sandbox-snapshots.md)、
[Pause, resume, and snapshot integration](pause-resume-snapshot-integration.md)。
