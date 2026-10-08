# template-vm 命令行使用指南

[English version](template-vm-user-guide.en.md)

`template-vm` 在单机上直接驱动 OverlayBD ublkd 和 Firecracker，用于：

1. 从普通 OCI/Docker 镜像或 Native OverlayBD 镜像构建本地模板产物；
2. 将 registry 中的一对 rootfs/snapfiles 模板恢复为运行中的 Firecracker VM；
3. 查询和清理 VM 及其本地资源。

当前工具绕过 fast-sandbox 控制面，适合开发、验证和主机侧集成。多数操作需要 root 权限。

本文所有命令都以 fast-sandbox 项目为根目录。开始前设置统一环境变量：

```bash
cd ./fast-sandbox
export FAST_SANDBOX_ROOT="$PWD"
export TEMPLATE_VM="$FAST_SANDBOX_ROOT/bin/template-vm"
export ACCELERATED_CONTAINER_IMAGE_DIR="$FAST_SANDBOX_ROOT/../accelerated-container-image"
export CONVERTOR="$ACCELERATED_CONTAINER_IMAGE_DIR/bin/convertor"
export TEMPLATE_VM_ASSETS="$FAST_SANDBOX_ROOT/.template-vm"
export FIRECRACKER="$TEMPLATE_VM_ASSETS/firecracker"
export KERNEL="$TEMPLATE_VM_ASSETS/vmlinux-6.1.176"
export INIT_SCRIPT="$TEMPLATE_VM_ASSETS/init.sh"
export TEMPLATE_FILE="$TEMPLATE_VM_ASSETS/template.json"
export DOCKER_CONFIG_FILE="$TEMPLATE_VM_ASSETS/docker-config.json"
export BUILD_WORK_DIR="$TEMPLATE_VM_ASSETS/builds"
export BUILD_OUTPUT_DIR="$TEMPLATE_VM_ASSETS/artifacts"
export STATE_DIR="$TEMPLATE_VM_ASSETS/state"
export RUNTIME_DIR="$TEMPLATE_VM_ASSETS/runtime"
mkdir -p "$TEMPLATE_VM_ASSETS"
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
make template-vm-convertor
```

使用 fast-sandbox 根目录下的其他 checkout：

```bash
export ACCELERATED_CONTAINER_IMAGE_DIR="$FAST_SANDBOX_ROOT/third_party/accelerated-container-image"
export CONVERTOR="$ACCELERATED_CONTAINER_IMAGE_DIR/bin/convertor"
make template-vm-convertor \
  ACCELERATED_CONTAINER_IMAGE_DIR="$ACCELERATED_CONTAINER_IMAGE_DIR"
```

convertor 仅在普通 OCI/Docker 输入时使用；Native OverlayBD 输入不会依赖它。`template-vm` 始终以 `--no-upload` 调用 convertor。

## 2. 编译

在 fast-sandbox 仓库根目录执行：

```bash
go test ./cmd/template-vm/
go vet ./cmd/template-vm/
make template-vm
```

输出文件为：

```text
$FAST_SANDBOX_ROOT/bin/template-vm
```

也可以直接编译：

```bash
go build -o "$TEMPLATE_VM" ./cmd/template-vm/
```

查看帮助：

```bash
"$TEMPLATE_VM" --help
"$TEMPLATE_VM" build --help
"$TEMPLATE_VM" create --help
"$TEMPLATE_VM" list --help
"$TEMPLATE_VM" delete --help
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
| `--kernel-dir` | `/opt/template-vm/kernels` | `create` 默认查找 `vmlinux-<version>` 的目录 |
| `--state-dir` | `/var/lib/template-vm/sandboxes` | 持久化 sandbox 状态根目录 |
| `--runtime-dir` | `/run/template-vm/sandboxes` | API socket 和串口日志目录 |
| `--firecracker` | `firecracker` | Firecracker 可执行文件 |
| `--overlaybd-convertor` | `/opt/overlaybd/snapshotter/convertor` | OCI userspace convertor |
| `--build-work-dir` | `/var/lib/template-vm/builds` | 构建临时目录根路径 |
| `--build-output-dir` | `/var/lib/template-vm/artifacts` | 本地 artifact 根路径 |
| `--plain-http` | `false` | 对 registry 使用明文 HTTP |

全局参数放在子命令之前，例如：

```bash
sudo "$TEMPLATE_VM" --plain-http --state-dir "$STATE_DIR" list
```

## 4. 构建模板

最小命令：

```bash
sudo "$TEMPLATE_VM" build \
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
| `--init-script-file` | 内置 Redis init | 注入 guest 并作为 PID 1 运行的脚本 |
| `--ready-pattern` | `Ready to accept connections` | 串口中表示工作负载就绪的文本 |
| `--ready-timeout` | `3m` | 等待就绪文本的超时 |
| `--auth-file` | 空 | Docker config JSON 凭证文件 |
| `--username` | 空 | `username:password` 形式的 registry 凭证 |
| `--keep-work-dir` | `false` | 成功后保留构建临时目录 |
| `--no-compress` | `false` | 提交层时禁用 zfile 压缩 |

`build` 不会自动执行源镜像 OCI config 中的 Entrypoint、Cmd 或 Env。对于非 Redis 工作负载，应使用 `--init-script-file` 提供 init，并让脚本在服务真正就绪后输出与 `--ready-pattern` 完全匹配的稳定文本。

### 4.1 从 Native OverlayBD 镜像构建

以下公开测试镜像是 Native OverlayBD，构建时会直接拉取其 Native 层并绕过 convertor：

```bash
sudo "$TEMPLATE_VM" \
  --firecracker "$FIRECRACKER" \
  --build-work-dir "$BUILD_WORK_DIR" \
  --build-output-dir "$BUILD_OUTPUT_DIR" \
  build \
  --source dadi-test-registry.cn-hangzhou.cr.aliyuncs.com/sample-v2/code-interpreter:v1.0.0_containerd_accelerated \
  --output code-interpreter:v1.0.0 \
  --kernel "$KERNEL" \
  --init-script-file "$INIT_SCRIPT" \
  --ready-pattern template-vm-code-interpreter-ready \
  --ready-timeout 10m
```

输入必须是整个 manifest 均为 Native OverlayBD 层。Turbo OCI、tar-wrapped OverlayBD、Native/tar 混合层、空 manifest 和未知 layer media type 均会被拒绝。

### 4.2 从普通 OCI/Docker 镜像构建

将 `--source` 指向普通 OCI/Docker 镜像：

```bash
sudo "$TEMPLATE_VM" \
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
sudo "$TEMPLATE_VM" build \
  --source registry.example.com/team/application:latest \
  --output application:v1 \
  --kernel "$KERNEL" \
  --username 'user:password'
```

密码可以包含 `:`；用户名不能为空。也可以使用 Docker config：

```bash
sudo "$TEMPLATE_VM" build \
  --source registry.example.com/team/application:latest \
  --output application:v1 \
  --kernel "$KERNEL" \
  --auth-file "$DOCKER_CONFIG_FILE"
```

`--username` 与 `--auth-file` 互斥。普通 OCI 输入会把同一凭证用于初始 manifest 解析和 convertor 拉层。外部 convertor 只支持通过 argv 接收凭证，因此运行期间同用户或 root 可能从进程信息中看到凭证；`template-vm` 自身不会打印完整命令，并会对错误输出中的凭证脱敏。

明文 HTTP registry 需要在子命令前增加：

```bash
sudo "$TEMPLATE_VM" --plain-http build ...
```

### 4.4 磁盘大小

rootfs 默认是 20 GiB，可指定其他正整数 GiB：

```bash
sudo "$TEMPLATE_VM" build ... --disk-size-gb 30
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

当前 CLI 尚未提供 `push` 子命令。完整的本地 registry 上传命令参见 [template-vm 手工 E2E 验证](template-vm-manual-e2e.md#7-将本地产物上传到临时-registry)。生产环境应使用能够保留 OCI manifest 和 OverlayBD layer media type 的发布流程。

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
| `template.auth.dockerAuth` | Docker config JSON 的字符串形式；匿名 registry 使用空字符串 |
| `metadata` | 预留透传字段，可省略 |

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
sudo "$TEMPLATE_VM" \
  --firecracker "$FIRECRACKER" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create \
  -f "$TEMPLATE_FILE" \
  --network none
```

`--network` 当前只支持 `none`。`create` 将：

1. 拉取 rootfs/snapfiles manifests；
2. 创建并挂载 ublk 设备；
3. 从 snapfiles 加载 Firecracker snapshot；
4. 将恢复后的 rootfs drive 重定向到新设备；
5. 恢复 VM 并写入 `state.json`。

默认根据模板 metadata 中的 `kernel_version` 检查 `/opt/template-vm/kernels/vmlinux-<version>`。可显式覆盖：

```bash
sudo "$TEMPLATE_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create \
  -f "$TEMPLATE_FILE" \
  --kernel "$KERNEL"
```

对于明文 HTTP registry：

```bash
sudo "$TEMPLATE_VM" \
  --plain-http \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  create -f "$TEMPLATE_FILE"
```

不要重复使用仍存在的 `sandbox_id`。

## 8. 查询 VM

```bash
sudo "$TEMPLATE_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  list
```

示例输出：

```text
SANDBOX_ID       STATUS   PID    ROOTFS_DEV   SNAPFILES_DEV   API_SOCKET
application-demo running  12345  /dev/ublkb0  /dev/ublkb1     $RUNTIME_DIR/application-demo/api.sock
```

状态由已记录的 Firecracker PID 是否存活决定。无法读取 `state.json` 的目录显示为 `corrupt`。使用自定义目录时，`list`、`create` 和 `delete` 必须传入相同的 `--state-dir` 与 `--runtime-dir`。

## 9. 清理 VM

```bash
sudo "$TEMPLATE_VM" \
  --state-dir "$STATE_DIR" \
  --runtime-dir "$RUNTIME_DIR" \
  delete application-demo
```

清理顺序为：

1. 向 Firecracker 发送 SIGTERM，必要时升级为 SIGKILL；
2. 卸载 snapfiles；
3. 删除 rootfs 和 snapfiles ublk 设备；
4. 删除 sandbox state 和 runtime 目录。

各步骤采用 best-effort：某一步失败时仍继续执行后续清理，最后返回非零错误并在 stderr 输出警告。不要直接删除 state 目录；`state.json` 保存了 PID、mount point 和 ublk dev_id，丢失它可能造成资源泄漏。

清理后可核对：

```bash
sudo "$TEMPLATE_VM" \
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

需要完整的 privileged E2E、artifact digest 校验、临时 registry 发布和残留资源排查时，参见 [template-vm 手工 E2E 验证](template-vm-manual-e2e.md)。
