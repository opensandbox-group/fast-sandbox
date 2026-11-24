# fsb-vm 命令行使用指南

[English version](fsb-vm-user-guide.en.md)

`fsb-vm` 在单机上直接驱动 OverlayBD ublkd 和 Firecracker，用于：

1. 从普通 OCI/Docker 镜像或 Native OverlayBD 镜像构建本地模板产物；
2. 将 registry 中的一对 rootfs/snapfiles 模板恢复为运行中的 Firecracker VM；
3. 查询和清理 VM 及其本地资源。

当前工具绕过 fast-sandbox 控制面，适合开发、验证和主机侧集成。多数操作需要 root 权限。

本文所有命令都以 fast-sandbox 项目为根目录。开始前设置统一环境变量：

```bash
cd ./fast-sandbox
export FAST_SANDBOX_ROOT="$PWD"
export FSB_VM="$FAST_SANDBOX_ROOT/bin/fsb-vm"
export ACCELERATED_CONTAINER_IMAGE_DIR="$FAST_SANDBOX_ROOT/../accelerated-container-image"
export CONVERTOR="$ACCELERATED_CONTAINER_IMAGE_DIR/bin/convertor"
export FSB_VM_ASSETS="$FAST_SANDBOX_ROOT/.fsb-vm"
export FIRECRACKER="$FSB_VM_ASSETS/firecracker"
export KERNEL="$FSB_VM_ASSETS/vmlinux-6.1.176"
export INIT_SCRIPT="$FSB_VM_ASSETS/init.sh"
export TEMPLATE_FILE="$FSB_VM_ASSETS/template.json"
export DOCKER_CONFIG_FILE="$FSB_VM_ASSETS/docker-config.json"
export BUILD_WORK_DIR="$FSB_VM_ASSETS/builds"
export BUILD_OUTPUT_DIR="$FSB_VM_ASSETS/artifacts"
export STATE_DIR="$FSB_VM_ASSETS/state"
export RUNTIME_DIR="$FSB_VM_ASSETS/runtime"
mkdir -p "$FSB_VM_ASSETS"
```

将 Firecracker、内核、init 脚本、模板文件和可选 Docker config 放到上述变量指定的位置，或根据实际情况修改变量值。

## 1. 前置条件

运行节点需要：

- Linux、KVM 和与宿主架构匹配的 Firecracker；
- 已启动的 `overlaybd-ublkd`，默认 socket 为 `/var/run/overlaybd-ublk/ublkd.sock`；
- OverlayBD 工具：
  - `/opt/overlaybd/bin/overlaybd-create`
  - `/opt/overlaybd/bin/overlaybd-apply`
  - `/opt/overlaybd/bin/overlaybd-commit`
  - `/opt/overlaybd/bin/overlaybd-resize`
- `e2fsck`、`resize2fs`、`dumpe2fs`、`mount` 和 `umount`；
- 构建普通 OCI 镜像时，还需要 accelerated-container-image userspace convertor。

快速预检：

```bash
test -c /dev/kvm
test -S /var/run/overlaybd-ublk/ublkd.sock
test -x /opt/overlaybd/bin/overlaybd-create
test -x /opt/overlaybd/bin/overlaybd-apply
test -x /opt/overlaybd/bin/overlaybd-commit
test -x /opt/overlaybd/bin/overlaybd-resize
curl -fsS --unix-socket /var/run/overlaybd-ublk/ublkd.sock \
  http://localhost/v1/ping
```

### 1.1 安装 OCI convertor

`accelerated-container-image` 是独立前置依赖，不由 fast-sandbox 自动下载。当前验证基线 revision 为 `a00e366f9bfc970ec4aaa00887541c4c1d9f9dab`：

```bash
cd "$ACCELERATED_CONTAINER_IMAGE_DIR"
git checkout a00e366f9bfc970ec4aaa00887541c4c1d9f9dab
make bin/convertor
sudo install -D -m0755 bin/convertor \
  /opt/overlaybd/snapshotter/convertor
cd "$FAST_SANDBOX_ROOT"
```

如果 checkout 位于 fast-sandbox 相邻目录，也可以只编译依赖：

```bash
make fsb-vm-convertor
```

使用 fast-sandbox 根目录下的其他 checkout：

```bash
export ACCELERATED_CONTAINER_IMAGE_DIR="$FAST_SANDBOX_ROOT/third_party/accelerated-container-image"
export CONVERTOR="$ACCELERATED_CONTAINER_IMAGE_DIR/bin/convertor"
make fsb-vm-convertor \
  ACCELERATED_CONTAINER_IMAGE_DIR="$ACCELERATED_CONTAINER_IMAGE_DIR"
```

convertor 仅在普通 OCI/Docker 输入时使用；Native OverlayBD 输入不会依赖它。`fsb-vm` 始终以 `--no-upload` 调用 convertor。

## 2. 编译

在 fast-sandbox 仓库根目录执行：

```bash
go test ./cmd/fsb-vm/
go vet ./cmd/fsb-vm/
make fsb-vm
```

输出文件为：

```text
$FAST_SANDBOX_ROOT/bin/fsb-vm
```

也可以直接编译：

```bash
go build -o "$FSB_VM" ./cmd/fsb-vm/
```

查看帮助：

```bash
"$FSB_VM" --help
"$FSB_VM" build --help
"$FSB_VM" create --help
"$FSB_VM" list --help
"$FSB_VM" delete --help
```

## 3. 基本命令

| 命令 | 用途 |
| --- | --- |
| `build` | 从远程源镜像构建本地 rootfs/snapfiles 模板对 |
| `create -f template.json` | 从 registry 中的模板恢复并运行 VM |
| `list` | 列出本地 sandbox，并根据 Firecracker PID 判断状态 |
| `delete <sandbox_id>` | 停止 VM，卸载 snapfiles，删除 ublk 设备和状态目录 |

常用全局参数：

| 参数 | 默认值 | 用途 |
| --- | --- | --- |
| `--ublkd-socket` | `/var/run/overlaybd-ublk/ublkd.sock` | ublkd 控制 socket |
| `--cred-file` | `/opt/overlaybd/cred.json` | OverlayBD 全局凭证文件 |
| `--reject-credential-conflicts` | `false` | 共享 daemon 时拒绝同仓库的凭证冲突 |
| `--kernel-dir` | `/opt/fsb-vm/kernels` | `create` 默认查找 `vmlinux-<version>` 的目录 |
| `--state-dir` | `/var/lib/fsb-vm/sandboxes` | 持久化 sandbox 状态根目录 |
| `--manifest-cache-dir` | `<state-dir>/.manifest-cache` | 不可变 registry 元数据缓存，可独立于 sandbox 状态共享 |
| `--runtime-dir` | `/run/fsb-vm/sandboxes` | API socket 和串口日志目录 |
| `--firecracker` | `firecracker` | Firecracker 可执行文件 |
| `--overlaybd-convertor` | `/opt/overlaybd/snapshotter/convertor` | OCI userspace convertor |
| `--build-work-dir` | `/var/lib/fsb-vm/builds` | 构建临时目录根路径 |
| `--build-output-dir` | `/var/lib/fsb-vm/artifacts` | 本地 artifact 根路径 |
| `--plain-http` | `false` | 对 registry 使用明文 HTTP |

全局参数放在子命令之前，例如：

```bash
sudo "$FSB_VM" --plain-http --state-dir "$STATE_DIR" list
```

## 4. 构建模板

通过 Kubernetes `SandboxTemplate` 构建时，`spec.output.publish` 只填写仓库地址，
例如 `oci://registry.example.com/team/templates`，不要带 tag 或 digest。
Controller 为每个构建 Pod 分配随机 UUID，Builder 发布 `<uuid>_rootfs` 和
`<uuid>_snapfiles`，并通过 `status.manifestRef` 返回 snapfiles 的不可变 digest 引用。
修改 spec 触发新一轮构建时生成新的 UUID；重复 reconcile 复用已有 Pod 的 UUID。
重复 apply 未变化的 spec 不会触发构建。已有带 tag 的发布地址需要先去掉 tag，
具体见 [OCI 模板重复构建](overlaybd-oci-sandboxes.md#rebuilds-and-generated-tags)。

`SandboxTemplate` 的标准 Builder 镜像包含 OCI 所需的 `fsb-vm`、convertor 和 OverlayBD 工具。输出为 `oci://` 时，Builder 启动 ublkd，读取源镜像的 manifest/config 以固定 linux/amd64 镜像 digest 并准备环境变量和运行时注入文件，然后将远端 digest 引用直接传给 `fsb-vm build`。源镜像层由 `fsb-vm` 的构建流程读取，不预先写入 OCI layout，也不启动本地 registry。`format: native` 的对象存储路径保留 `pullOCILayout → oci2rootfs` 流程；OverlayBD 仅支持 OCI，不支持 S3；`SANDBOX_TEMPLATE_IMAGE_TAR` 仅支持该对象存储路径，OCI 路径设置此变量会报错。OCI 构建需要访问源和目标 registry。

`build/Dockerfile.fastlet` 生成的标准 Fastlet 镜像包含 OCI 运行工具；OCI Firecracker Pool 通过 `FAST_SANDBOX_OCI_ENABLED=true` 启动 ublkd。默认不启动，普通运行模式不需要 ublk。标准 Builder 使用 `auto` 模式，保留非 OCI 构建不依赖 ublk 的行为。OCI 仓库访问支持匿名或用户名密码认证，包括 Docker config 的 `auth` 字段；仅含 `identitytoken` 的凭据会被明确拒绝，同时存在用户名密码时使用用户名密码。原有公共凭据字段及其他运行时行为保持不变。

Checkpoint 默认写回源模板仓库；可用 `FAST_SANDBOX_OCI_SNAPSHOT_REPOSITORY=registry/repository` 指定其他目标。目标凭证需具备 push 权限，独立写账号通过 registry rule 的 `writeSecretRef` 配置。Checkpoint 发布按完整仓库路径分别选择父层读凭证和目标写凭证；跨仓库时需在 Pool 中配置两者。OCI GuestCopy 直接挂载 ublk 块设备，普通 rootfs 镜像文件仍使用 loop 挂载。

最小命令：

```bash
sudo "$FSB_VM" build \
  --source <registry>/<repository>:<tag> \
  --output <name>:<template-id> \
  --kernel "$KERNEL"
```

`--output` 是本地逻辑名称，不是 registry 地址，名称部分不能包含 `/`。例如 `code-interpreter:v1.0.0` 会生成：

- `<registry-repository>:v1.0.0_rootfs` 对应的 rootfs manifest；
- `<registry-repository>:v1.0.0_snapfiles` 对应的 snapfiles manifest。

常用构建参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--vcpu` | `1` | VM vCPU 数 |
| `--memory-mb` | `1024` | VM 内存 MiB |
| `--disk-size-gb` | `20` | rootfs 虚拟磁盘大小 GiB，必须为正整数 |
| `--snapfiles-size-mb` | 自动计算 | snapfiles 虚拟磁盘大小 |
| `--kernel-version` | 从 `vmlinux-<version>` 推导 | 写入模板 metadata 的内核版本 |
| `--manifest-extra-file` | 空 | 可选的 `guestNetwork`、`lineage` 和 `platform` 补充字段；合入生成的 manifest，不能覆盖核心 VM 字段 |
| `--init-script-file` | 内置 Redis init | 注入 guest 并作为 PID 1 运行的脚本 |
| `--ready-pattern` | `Ready to accept connections` | 串口中表示工作负载就绪的文本 |
| `--ready-timeout` | `3m` | 等待就绪文本的超时 |
| `--auth-file` | 空 | Docker config JSON 凭证文件 |
| `--username` | 空 | `username:password` 形式的 registry 凭证 |
| `--keep-work-dir` | `false` | 成功后保留构建临时目录 |
| `--no-compress` | `false` | 提交层时禁用 zfile 压缩 |
| `--guest-control-port` | `0`（禁用） | 冷启动 probe：通过 vsock 校验经 `--inject-dir` 注入的 guest-control helper；只接受保留端口 `1234`。probe 成功后模板 manifest 记录 `guestControl` 能力，probe 失败则构建失败 |

`build` 不会自动执行源镜像 OCI config 中的 Entrypoint、Cmd 或 Env。对于非 Redis 工作负载，应使用 `--init-script-file` 提供 init，并让脚本在服务真正就绪后输出与 `--ready-pattern` 完全匹配的稳定文本。`--init-path` 必须是无空白的 clean 绝对 guest 路径；写入受 `os.Root` 约束，不会跟随镜像中的符号链接逃逸到宿主机。每次 build 使用独立临时目录；同一 `--output` 的并发构建通过非阻塞锁立即拒绝第二个请求。

### 4.1 从 Native OverlayBD 镜像构建

以下公开测试镜像是 Native OverlayBD，构建时会直接拉取其 Native 层并绕过 convertor：

```bash
sudo "$FSB_VM" \
  --firecracker "$FIRECRACKER" \
  --build-work-dir "$BUILD_WORK_DIR" \
  --build-output-dir "$BUILD_OUTPUT_DIR" \
  build \
  --source dadi-test-registry.cn-hangzhou.cr.aliyuncs.com/sample-v2/code-interpreter:v1.0.0_containerd_accelerated \
  --output code-interpreter:v1.0.0 \
  --kernel "$KERNEL" \
  --init-script-file "$INIT_SCRIPT" \
  --ready-pattern fsb-vm-code-interpreter-ready \
  --ready-timeout 10m
```

输入必须是整个 manifest 均为 Native OverlayBD 层。Turbo OCI、tar-wrapped OverlayBD、Native/tar 混合层、空 manifest 和未知 layer media type 均会被拒绝。

### 4.2 从普通 OCI/Docker 镜像构建

将 `--source` 指向普通 OCI/Docker 镜像：

```bash
sudo "$FSB_VM" \
  --overlaybd-convertor "$CONVERTOR" \
  --build-work-dir "$BUILD_WORK_DIR" \
  --build-output-dir "$BUILD_OUTPUT_DIR" \
  build \
  --source registry.example.com/team/application:latest \
  --output application:v1 \
  --kernel "$KERNEL" \
  --init-script-file "$INIT_SCRIPT" \
  --ready-pattern application-ready
```

处理流程为：

1. 解析 tag 或 digest；
2. 如果输入是多架构 index，只选择 `linux/<当前 Go 架构>` manifest；
3. 使用选中 manifest 的固定 digest 调用 convertor；
4. 强制 `--no-upload --dump-manifest --reserve`，转换为本地 Native OverlayBD；
5. 校验并导入 converted manifest、config 和各层；
6. 使用转换后的 Native 层构建 rootfs/snapfiles。

原始 OCI tar 层不会进入最终 artifact。如果 convertor 失败，构建会终止，不会回退到直接解包 OCI 层。

### 4.3 Registry 认证

匿名 registry 不需要认证参数。

直接传入凭证：

```bash
sudo "$FSB_VM" build \
  --source registry.example.com/team/application:latest \
  --output application:v1 \
  --kernel "$KERNEL" \
  --username 'user:password'
```

密码可以包含 `:`；用户名不能为空。也可以使用 Docker config：

```bash
sudo "$FSB_VM" build \
  --source registry.example.com/team/application:latest \
  --output application:v1 \
  --kernel "$KERNEL" \
  --auth-file "$DOCKER_CONFIG_FILE"
```

`--username` 与 `--auth-file` 互斥。普通 OCI 输入会把同一凭证用于初始 manifest 解析和 convertor 拉层。外部 convertor 只支持通过 argv 接收凭证，因此运行期间同用户或 root 可能从进程信息中看到凭证；`fsb-vm` 自身不会打印完整命令，并会对错误输出中的凭证脱敏。

明文 HTTP registry 需要在子命令前增加：

```bash
sudo "$FSB_VM" --plain-http build ...
```

### 不可变 manifest 缓存

`create` 将以 `@sha256:...` 引用的 rootfs 和 snapfiles 镜像 manifest 缓存在
`<state-dir>/.manifest-cache/`。后续创建先校验本地文件的 SHA256 和 manifest 描述符，
命中后直接使用，不再向 registry 请求这两份 manifest 或获取对应的认证 token。
缓存按 registry、repository、HTTP/HTTPS 和当前凭证隔离，不保存凭证。
使用 `template.reference` 时，同目录还缓存由平台入口解析出的不可变 pair 和元数据，
并将镜像配对和 checkpoint 归属记录到 sandbox 的 `state.json.template_metadata`；平台信息从挂载的 `manifest.json` 读取。
删除单个 sandbox 不会删除这份共享缓存。通过 `--manifest-cache-dir` 可让同一节点上不同 sandbox 状态目录共用缓存。Fastlet 使用 `<StateRoot>/oci/manifest-cache`，Pod 重建后无需重新获取已缓存的 registry manifest。缓存超过 128 MiB 或 4,096 个文件时按最近访问时间淘汰；凭证隔离与可变 tag 回源规则不变。

缓存缺失或损坏时重新从 registry 获取并原子写入；缓存写入失败不会阻止使用已验证的
远端 manifest。可变 tag 始终回源解析，指向多架构 index 的 digest 保持现有平台选择流程。
删除 `.manifest-cache/` 可清除本地 manifest 缓存。它与 OverlayBD 数据块缓存独立：
每个 sandbox 仍创建独立的可写层和设备，未缓存的数据块仍由 OverlayBD 按需读取。

Fastlet 从挂载的 snapfiles 文件系统中的 `manifest.json` 读取平台信息。FastPath 使用 create 请求中的绑定，不再访问 OCI registry 读取策略；opaque `policy.json` 和 `vault.json` 由节点侧导入。

`fsb-vm build` 无需平台输入也会生成唯一的 `snapfiles/manifest.json`，不再生成 `metadata.json`。`--manifest-extra-file` 只补充 `guestNetwork`、`lineage`、`platform`，禁止覆盖核心 VM 配置。OCI 和 S3 共用 [schemaVersion 1 模型](artifact-manifest-reference.md)；已有产物需重建，不提供旧格式兼容读取。

### 4.4 磁盘大小

rootfs 默认是 20 GiB，可指定其他正整数 GiB：

```bash
sudo "$FSB_VM" build ... --disk-size-gb 30
```

当源 ext4 文件系统大于目标容量时，工具先缩小文件系统，再 detach ublk 设备、修改 OverlayBD upper vsize、重新 attach，并校验块设备和 ext4 几何。目标容量必须能容纳实际文件系统数据。

### 4.5 本地 artifact

构建完成后，输出目录类似：

```text
$BUILD_OUTPUT_DIR/application_v1/
  manifest/
    source-*.json
    application_v1_rootfs.json
    application_v1_snapfiles.json
    index.json
  blobs/sha256/
    <sha256 blobs>
```

普通 OCI 输入的 `source-*.json` 是转换后的 Native OverlayBD manifest；`index.json` 中的 `source_ref` 仍保留用户传入的原始引用。产物经过校验后原子发布；同名最终目录已存在时构建会失败，不会覆盖。

`build` 当前只创建本地产物，不会上传 registry。

## 5. 发布模板到 registry

`create` 要求 registry 中存在同一 repository 下的两个 tag：

```text
<template-id>_rootfs
<template-id>_snapfiles
```

例如 `template_id` 为 `v1` 时，应发布：

```text
registry.example.com/team/application:v1_rootfs
registry.example.com/team/application:v1_snapfiles
```

发布工具必须先上传这两个 manifest 引用的全部 `blobs/sha256/*`，再按对应 tag 上传 rootfs 和 snapfiles manifest。`manifest/index.json` 记录了 manifest 文件名和 digest，可供发布脚本读取。

对明文 HTTP registry，使用 `fsb-vm --plain-http push ...`；该参数同时用于目标仓库和增量产物的远端父层读取，默认仍使用 HTTPS。

`fsb-vm push` 接收 `--artifact-dir`、`--destination registry/repository:tag`、`--metadata-file` 和 `--auth-file`，只发布 `<tag>_rootfs` 和 `<tag>_snapfiles` 两份镜像；rootfs 先发布，snapfiles 最后发布。fsb-vm 始终生成核心 VM manifest，平台信息通过可选 `build --manifest-extra-file <manifest-extra.json>` 补充并在封装前写入同一份 `manifest.json`；OCI manifest 仅保留配对和 checkpoint 归属 annotations。`push --metadata-file` 可省略，仅提取 checkpoint 归属，不能向已封装的 snapfiles 添加文件。返回的 `manifestRef` 与 `artifactDigest` 指向 snapfiles manifest。两份镜像按相同 ID 配对，ID 必须不可覆盖；发布器拒绝用不同内容覆盖现有 tag。采用历史元数据结构的 OCI 和 S3 模板均需重建，不提供旧格式兼容读取。`SandboxTemplate` 的 OCI 构建流程自动执行此步骤，将不带 tag 的 `output.publish` 与系统分配的 UUID 组合为上传目标。手动执行 CLI 时仍按 `--destination repository:tag` 显式指定 tag。产物布局与上传顺序参见 [发布契约](../design/adr-fsb-vm-cli.md#adr-012-build-本地产物布局与发布契约build-迭代补充)。

## 6. 准备 template.json

`create` 的唯一输入 manifest 示例：

```json
{
  "sandbox_id": "application-demo",
  "template": {
    "repo": "registry.example.com/team/application",
    "template_id": "v1",
    "auth": {
      "dockerAuth": ""
    }
  },
  "metadata": {}
}
```

字段含义：

| 字段 | 说明 |
| --- | --- |
| `sandbox_id` | 本机唯一 sandbox ID，同时用于状态和 runtime 子目录 |
| `template.repo` | registry repository，不带 tag |
| `template.template_id` | 模板 ID；工具会拉取 `<id>_rootfs` 和 `<id>_snapfiles` |
| `template.reference` | 平台返回的 `oci://repository@sha256:...` 或逻辑 tag；与 `repo/template_id`、显式 `rootfs/snapfiles` 二选一 |
| `template.auth.dockerAuth` | Docker config JSON 的字符串形式；匿名 registry 使用空字符串 |
| `metadata` | 预留透传字段，可省略 |

Fastlet 集成使用 `template.reference`，直接执行 `create --prepare-only`。
CLI 负责解析两产物引用及 annotations、复用缓存和准备设备；Fastlet 不再先通过节点 agent 查询 OCI 元数据。
使用这种输入时，`metadata` 由解析后的镜像配对和归属信息替换，原有 repo/template_id 输入仍可使用。

私有 registry 的 `dockerAuth` 是 JSON **字符串**，不是嵌套对象。例如原始 Docker config：

```json
{"auths":{"registry.example.com":{"auth":"BASE64_USER_COLON_PASSWORD"}}}
```

在 `template.json` 中需要 JSON 转义：

```json
{
  "sandbox_id": "application-demo",
  "template": {
    "repo": "registry.example.com/team/application",
    "template_id": "v1",
    "auth": {
      "dockerAuth": "{\"auths\":{\"registry.example.com\":{\"auth\":\"BASE64_USER_COLON_PASSWORD\"}}}"
    }
  }
}
```

## 7. 运行 VM

```bash
sudo "$FSB_VM" \
  --firecracker "$FIRECRACKER" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create \
  -f "$TEMPLATE_FILE" \
  --network none
```

`--network` 当前只支持 `none`。`create` 将：

1. 严格校验并原子占用 `sandbox_id`，写入 `creating` 状态；
2. 拉取 manifests，创建 rootfs/snapfiles ublk 设备并渐进记录所有权；
3. 在 runtime 目录建立 `rootfs.dev` 链接，以该目录为 cwd 加载 Firecracker snapshot；
4. 恢复 VM，将 `state.json` 更新为 `running`。

默认根据模板 metadata 中的 `kernel.version` 检查 `/opt/fsb-vm/kernels/vmlinux-<version>`。标准 Builder 使用 `vmlinux.bin` 时，`kernel.version` 为 `vmlinux.bin`，对应检查同目录下的 `vmlinux.bin`，不会添加 `vmlinux-` 前缀。可显式覆盖：

```bash
sudo "$FSB_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create \
  -f "$TEMPLATE_FILE" \
  --kernel "$KERNEL"
```

对于明文 HTTP registry：

```bash
sudo "$FSB_VM" \
  --plain-http \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create -f "$TEMPLATE_FILE"
```

不要重复使用仍存在的 `sandbox_id`。

## 8. 查询 VM

```bash
sudo "$FSB_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  list
```

示例输出：

```text
SANDBOX_ID       STATUS   PID    ROOTFS_DEV   SNAPFILES_DEV   API_SOCKET
application-demo running  12345  /dev/ublkb0  /dev/ublkb1     $RUNTIME_DIR/application-demo/api.sock
```

状态由已记录的 Firecracker `(PID, starttime, boot_id)` 身份是否仍匹配决定，而非只检查 PID 数字。无法读取 `state.json` 的目录显示为 `corrupt`。使用自定义目录时，`list`、`create` 和 `delete` 必须传入相同的 `--state-dir` 与 `--runtime-dir`。

## 9. 清理 VM

```bash
sudo "$FSB_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  delete application-demo
```

清理顺序为：

1. 向 Firecracker 发送 SIGTERM，必要时升级为 SIGKILL；
2. 卸载 snapfiles；
3. 删除 rootfs 和 snapfiles ublk 设备；
4. 删除 sandbox state 和 runtime 目录。

Firecracker 停止失败时不会继续释放仍被 VMM 占用的设备；其他安全步骤会尽量执行。只有资源全部释放后才删除目录。任何失败都会保留 `deleting` 状态和 writable upper，可重新运行同一 delete 重试。设备按 ublkd 当前列出的绝对 config 路径确认归属，不会仅凭可复用的 dev_id 删除；进程信号通过 pidfd 发送，并校验 starttime 与 boot_id。

清理后可核对：

```bash
sudo "$FSB_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  list
sudo curl -fsS \
  --unix-socket /var/run/overlaybd-ublk/ublkd.sock \
  http://localhost/v1/list
```

## 10. 已知限制

- 仅支持 Linux 宿主平台选择；多架构 source 只构建当前宿主架构。
- 网络 provider 当前仅支持 `none`。
- 不支持 Turbo OCI、tar-wrapped OverlayBD、混合 layer 格式和未知 media type。
- `build` 只生成本地产物，不负责推送 registry。
- `build` 不继承 OCI Entrypoint、Cmd 或 Env，工作负载启动必须由 init 脚本定义。
- create/delete 状态是本机状态，不是分布式控制面资源。

构建、创建步骤和产物所有权说明见 [fsb-vm 设计](../design/adr-fsb-vm-cli.md)。

OCI Fastlet 使用宿主机 `/dev`，以访问运行期间新增的 ublk 设备。runtime-agent DaemonSet 中的节点 ublkd 容器为同节点的 Fastlet 提供服务；fsb-vm 直接连接共享 Unix socket，runtime-agent 安装工具、检查健康，不转发设备请求。OCI jailer 也会在启动前挂载配置的 snapshot spill 目录。

快照打包中断后，重试或删除 staging 前会先按 config 路径恢复并释放 pack 设备；清理失败时保留 backing files。独立 fsb-vm 按实际 rootfs/snapfiles 仓库同步凭据：匿名访问会覆盖该仓库的旧凭据，不会删除其他仓库的凭据。Fastlet 为节点 daemon 启用 `--reject-credential-conflicts`，拒绝同仓库的不同账号或匿名覆盖。此同步不承诺立即撤销已经运行的 VM 的访问。

构建 Fastlet 镜像使用 `make images COMPONENT=fastlet FASTLET_IMAGE=opensandbox/fsb-fastlet:dev`，然后自行推送并在 Pool 中使用该镜像。此 tag 是构建示例，不表示已发布版本。Fastlet 使用 Ubuntu 22.04 运行阶段，不包含 Go 工具链和 OverlayBD 构建阶段。runtime-agent 镜像统一安装 fsb-vm 和 OverlayBD 工具，Fastlet 只读挂载这些工具，保留执行所需的系统库和基础工具。节点 agent/daemon 和 OCI Pool 均通过 `FAST_SANDBOX_OCI_ENABLED=true` 启用；路径、缓存容量、凭证和升级要求见 [OCI 部署指南](overlaybd-oci-sandboxes.md)。无 extent seek 支持时，upper 复制会扫描零块并保留稀疏空间，但扫描耗时可能高于 reflink/extent copy。
