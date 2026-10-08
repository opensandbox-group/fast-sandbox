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
- **注意**：daemon 持有的设备必须经其 API 删除；writable 镜像二次挂载会被 daemon
  拒绝（upper 排他保护）。build 缩盘不使用要求内核 6.11+ 的在线
  `UBLK_F_UPDATE_SIZE`，而是先删除设备、离线更新 upper，再重新 add。

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

### ADR-011 模板构建：`template-vm build` 冷启动快照法（build 迭代补充）

- **决策**：`template-vm build --source <OCI 或 overlaybd 镜像> --output <name:tag>`
  把远程源镜像转换为本地产物形式的模板对（`<tag>_rootfs` / `<tag>_snapfiles`）。
  CLI 先用 ORAS 解析 registry manifest；普通 OCI/Docker tar 镜像调用
  `accelerated-container-image` 的用户态 `convertor` 转成 Native OverlayBD 后，再进入
  与原生加速镜像相同的构建链路。template-vm 不调用 docker CLI。
- **build 与 create 的读取路径分歧**：create 保持 manifest-only（lowers 以
  `digest`+`repoBlobUrl` 远程按需 range read）；build 先解析并分类宿主平台 manifest。
  Native 源经 ORAS 下载；普通 OCI 源用 `convertor --no-upload --dump-manifest --reserve`
  本地转换。两条路径最终都把 Native layers 放入产物 `blobs/sha256/`，rootfs
  `config.v1.json` 的 `lowers[]` 用 `file` 指向本地 blob。
- **源格式门槛**：逐层校验 mediaType（分类规则移植自 AgentENV
  `src/image/oci_image.rs`）。全 Native 直接构建；全标准 OCI/Docker tar 先转换；拒绝
  turbo-OCI、tar-wrapped、Native/tar 混合与未知类型。转换后的 manifest 再次经过
  Native 校验，原始 OCI tar blob 不进入最终产物。
- **转换器前置依赖**：`accelerated-container-image` 独立下载并执行
  `make bin/convertor`，默认安装到 `/opt/overlaybd/snapshotter/convertor`，可用
  `--overlaybd-convertor` 覆盖。构建始终传 `--no-upload`；私有 registry 凭证可由
  `--username username:password` 或 `--auth-file` 提供并传给 converter。
- **rootfs 目标尺寸**：`--disk-size-gb` 接受正整数 GiB，默认 20，可显式指定其他尺寸。
  源镜像的虚拟盘尺寸烧在 LSMT 层头（常见 64~256 GiB，与内容大小无关），因此 build
  先以 lowers-only 设备探测源尺寸，并用 `max(源尺寸向上取整, 目标尺寸)` 创建初始 upper。
  设备保持未挂载，依次执行 `e2fsck -fy`、必要时 `resize2fs <device> <目标>G` 和
  `e2fsck -fn`；文件系统先到达目标后才删除 ublk 设备，通过
  `overlaybd-resize --config ... --size <目标>` 更新 upper，再更新 config 中的 vsize 并
  重新 add。最终以 BLKGETSIZE64 和 `dumpe2fs -h` 同时校验设备与 ext4 几何精确等于目标。
  metadata.json 保留既有 `disk_size_mb` 字段，写入 `disk_size_gb × 1024`。
- **快照拍摄**：向可写 rootfs 注入 init 脚本（内置 redis 样例默认脚本，
  `--init-script-file` 覆盖）→ 冷启动 Firecracker（boot args 含
  `init=<init-path>`）→ 等待串口出现 ready pattern（默认
  `Ready to accept connections`，可配，超时输出串口尾部）→ pause →
  Full Snapshot（`vmstate.bin` + `memfile`）→ 终止 Firecracker。
- **snapfiles 不经 raw 导入**：`overlaybd-create --hybrid` 建无 lower 的空可写盘 →
  `mkfs.ext4` → rw 挂载 → 写入 `/vmstate.bin`、`/memfile`、`/metadata.json` →
  sync → umount → ublkd del → `overlaybd-commit` 封装为只读层。排除项：
  `build/sandboxtemplate-builder/overlaybd-import-raw.cpp`（不参与本链路）。
- **密封顺序**：先 commit snapfiles upper，再删除 rootfs ublk 设备后 commit rootfs
  upper；commit 产物即新模板对的增量只读层。

### ADR-012 build 本地产物布局与发布契约（build 迭代补充）

- **决策**：build 只生成本地自包含产物，**本期不向 registry push**：
  ```text
  <output-dir>/<name>_<tag>/
    blobs/sha256/<源 layer digests + 双 commit 层 digest + config digest>
    manifest/source-<tag>.json
            <name>_<tag>_rootfs.json
            <name>_<tag>_snapfiles.json
            index.json
  ```
- **自包含性**：rootfs manifest 保持源 layer descriptor 顺序与属性、追加 rootfs
  commit 层；全部引用 blob（含继承层与生成的最小 config）必须存在于本地
  `blobs/sha256/`。build 在同级临时目录中逐项校验 digest/size/JSON schema，全部
  通过后才原子发布最终目录；同名产物已存在时拒绝覆盖。
- **commit 层约定**：mediaType 为 `...overlaybd.image.layer.v1.zfile`（默认 `-z`
  压缩）或 `...overlaybd.image.layer.v1.lsmt`（`--no-compress`），并打
  `containerd.io/snapshot/overlaybd/blob-digest == 自身 digest` 注解，与严格消费侧
  的 native 判定约定一致。
- **index.json（布局索引）**：记录 `source_ref`、源 manifest 文件/digest、双镜像的
  manifest 文件/digest（schema_version=1）。后续 push 迭代以此为唯一输入：读取
  目标 manifest → 从 `blobs/sha256/` 上传其引用的全部 blob → 提交 manifest
  （ORAS SDK）。因源 blobs 已落盘，可发布到任意 registry，无跨 registry blob copy。

### ADR-013 registry 凭证匹配归一化（build 迭代补充）

- **决策**：ORAS client 的凭证选取对 docker config 的 auths key 做与引用一致的
  归一化：去 scheme/尾斜杠/`/v1`、`/v2` API 尾巴，Docker Hub 别名
  （`docker.io`、`index.docker.io`、`registry.hub.docker.com` 等）统一映射到
  `registry-1.docker.io`；之后按最长 registry/repository 前缀匹配。
- **理由**：`docker login` 写入的标准 key 是 `https://index.docker.io/v1/`，不归一
  则 Hub 凭证永远匹配不到、静默降级为匿名（公开镜像可容忍，私有镜像 401）。

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

## build 主流程（编排顺序，严格分步、失败逆序回滚；build 迭代补充）

1. 校验参数；ping ublkd；读取 `--auth-file` 或 `--username username:password`。
2. ORAS 解析源 manifest（穿透一层 image index，按 `linux/<宿主 arch>` 选子 manifest）
   并在下载 layers 前分类（ADR-011）。
3. Native 源直接下载 config/layers；普通 OCI 源把选中的 manifest digest 和凭证传给
   `convertor --no-upload --dump-manifest --reserve`，校验其 manifest/config/
   `overlaybd.commit` 后导入 `manifest/` 与 `blobs/sha256/`。随后以统一的本地 Native
   lowers 探测源镜像虚拟尺寸。
4. rootfs：按 `max(源尺寸, --disk-size-gb)` 创建 hybrid upper 并 add → 离线
   `e2fsck -fy` → 必要时 `resize2fs <目标>G` → `e2fsck -fn` → 若 upper 尺寸不同则
   del、`overlaybd-resize`、更新 config vsize、重新 add → 校验块设备和 ext4 均为目标尺寸。
5. rw 挂载已校验的 rootfs，注入 init 脚本（0755），sync、umount。
6. 冷启动 Firecracker（1C1G 默认）→ 串口等待 ready pattern → pause → Full Snapshot
   → 终止 Firecracker。
7. snapfiles：空 hybrid upper → `/v1/add` → `mkfs.ext4` → rw 挂载 → 写入
   vmstate.bin/memfile/metadata.json → sync → umount → ublkd del → commit。
8. rootfs ublk del → commit rootfs upper；两个 commit 层移入 `blobs/sha256/`。
9. 生成最小 config blob、双 manifest、index.json；全量校验布局完整性 → 清理临时目录
   → 输出产物清单。

## 风险与缓解

| 风险 | 缓解 |
|------|------|
| hypervisor_type=dragonball 的存量模板无法恢复 | 启动前硬校验 + 明确报错；VMM 抽象预留 |
| vmstate 版本与宿主 firecracker 不兼容 | `firecracker_version` 校验；`--force` 逃生门 |
| 盘内布局仅口头约定 | 联调用真实镜像验证；路径集中为一个常量区块 |
| cred.json 并发覆盖 | 原子写（tmp+rename）+ 文件锁 |
| ublkd 设备泄漏（CLI 崩溃后） | state.json 记录 dev_id；delete 幂等；后续可加 janitor |
| 内核要求 | ublk 基础能力即可；build 通过 del/resize/add 离线变更容量，不依赖 6.11+ 的在线 UBLK_F_UPDATE_SIZE |
| 缺少缩盘工具 | build 启动前确保 `e2fsck`、`resize2fs`、`dumpe2fs` 和 `/opt/overlaybd/bin/overlaybd-resize` 可执行；任一步失败即逆序回滚 |
| 普通 OCI 转换失败或产物损坏 | 固定调用外部 convertor 的 no-upload 模式；按源层序号定位产物并逐项校验 manifest/config/layer digest 与 size，失败时不进入 ublk 阶段 |
| 源镜像为 turbo-OCI、tar-wrapped、混合或未知格式 | manifest 分类阶段明确拒绝；只有全标准 tar 镜像进入 convertor（ADR-011） |
| 私有 registry 凭证泄漏 | ORAS 与 convertor 复用同一凭证；CLI 日志和错误不打印完整参数。外部 convertor 仅支持 argv 传参，因此文档明确同用户/root 可在运行期查看该参数 |
| upper 先于 ext4 缩小导致 EXT4 bad geometry | 始终先在初始大设备上离线缩 ext4，再 del/resize upper/add，并对两层几何做精确终检（ADR-011） |
| guest 内 workload 无统一启动约定 | init 脚本注入（内置 redis 样例默认值）+ `--init-script-file`/`--ready-pattern` 覆盖；ready 判定不只看进程启动 |
| build 产物与 registry 状态漂移 | 本期不 push；index.json 固化 source_ref 与全部 digest，发布时以此为唯一输入对账（ADR-012） |
