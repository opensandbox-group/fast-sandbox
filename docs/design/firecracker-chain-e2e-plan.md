# 阶段 1 收尾：builder → S3 → runtime 全链路验证（测试计划与环境准备）

> 文档类型：施工任务书（转交实现）
>
> 日期：2026-08-28
>
> 前置：#22（publish 补全）、#23（pull 层）、#24（UDS 接线）、#25（golden
> restore + E2E 迁移）均已合并。
>
> 背景：阶段 1 各组件内部闭环（单测 + fake store），但
> **builder publish → 真实 S3 → agent pull → driver restore 的完整链路
> 从未真实串通过**（详见 §1 断点盘点）。本任务补齐链路验证，以阶段 1
> 完美收尾。

---

## 1. 背景：链路断点盘点

| 段 | 现状 | 缺口 |
|----|------|------|
| ① builder publish | #22 只改了代码；`sandboxtemplate-e2e.sh` 的 spec 无 `publish`（E2E 文档："publish is not covered"） | **index/SHA256SUMS/产物布局从未真实上传过**；布局只存在于 #22 单测手搓 |
| ② pull 对真实 S3 | `s3client_test.go` 全 fakeStore | **SigV4 签名对 MinIO/OSS 从未实测**；credential 映射（`publishSecretRef` ↔ `registryconfig.Credential`）只写在设计 §1.4 |
| ③ driver restore 产物 | E2E 用**方式 B 自举**快照（自拍 + 手搓 manifest） | **builder 真实快照从未被 driver restore 消费过**（machine tuple/boot args/NIC 状态兼容性未互测） |
| ④ builder 快照网络 | snapshot_stage **完全不配 NIC**（设计："deliberately does not configure external connectivity"） | driver restore 的 `network_overrides{eth0}` 引用不存在的 iface、guest 无网卡不可达——**前置必改** |

## 2. 前置改动：builder snapshot 阶段 bake NIC（必做）

driver restore 的 `network_overrides` 只能替换快照内**已存在**的 iface
（v1.16），且 load 前不能动态 attach。因此 builder 拍快照时必须 bake
一个 NIC，镜像 E2E 自举 prep 的 recipe（`e2e_test.go` 的
`prepareE2EGoldenSnapshot`：tap + eth0 + 静态 IP）：

- `cmd/sandboxtemplate-builder/snapshot_stage.go`：
  - 拍快照的 VM 配置增加 `AttachNetworkInterface`（iface `eth0`，host tap
    用构建机本地 tap，如 `fc-build-tap`）+ boot args 增加静态 IP
    （`ip=<guestIP>::<gw>:<mask>::eth0:off`）；
  - guestIP 与 E2E 约定的 slot 布局解耦：builder 产物记录 guest IP/MAC
    到 manifest（新增字段或复用现有字段，见 §5 断言），driver restore
    侧 `network_overrides` 只换 tap、IP/MAC 由快照承载；
  - 构建机 tap 生命周期：快照阶段创建/删除（构建脚本管理）；
- 同步更新 `docs/design/sandboxtemplate-golden-image-builds.md` 的
  "Guest network" 约束（快照 bake NIC，但**不 bake 活跃连接**——原
  约束仍成立，仅从"无网卡"改为"有网卡无连接"）；
- 影响面：`sandboxtemplate-e2e.sh` 的构建流程需要 tap；构建环境
  （KVM 主机）已具备。

> 若此改动超出阶段 1 收尾范围（builder 归属另一团队），可退化为：
> 链路验证的 restore 段改用**带 NIC 的自举快照**，builder 产物只验证
> 拉取链（验证点 1-3），restore 兼容性（验证点 4）记为已知缺口并
> 开 issue 跟踪。**推荐做完整前置改动**。

## 3. 环境准备

### 3.1 参考主机（复用现有 E2E 环境）

`agent-sandbox033067064046.sg52`（KVM + `/dev/net/tun` + root，参考
`docs/guides/firecracker-runtime-e2e.md` 与
`sandboxtemplate-golden-image-e2e.md`），工作目录 `/home/gaoran/fast-sandbox`。

### 3.2 MinIO（真实 S3 兼容存储）

| 项 | 值 |
|----|-----|
| 部署 | `docker run -d -p 9000:9000 -p 9001:9001 minio/minio server /data --console-address ":9001"`（或下载二进制） |
| 凭据 | 自定义 AK/SK（如 `chain-test` / `chain-test-secret`），启动时注入 |
| Bucket | `sandbox-images`（`mc mb` 或 aws CLI 创建） |
| 生命周期 | 验证完成后由脚本清理（容器 + bucket） |

### 3.3 builder 真实 publish

- 复用 `cmd/sandboxtemplate-builder`：spec.json 增加
  `publish: s3://sandbox-images/publish` + `publishSecretRef`（对应
  MinIO 凭据）；`AWS_ENDPOINT_URL=http://127.0.0.1:9000`；
- 快照集来源：优先重跑 builder（含 §2 的 NIC 改动），产物在
  `SANDBOX_TEMPLATE_WORKDIR`；备选复用 `scripts/sandboxtemplate-e2e.sh`
  已有产物（`.sandboxtemplate-e2e/<format>/build/`）但**必须补拍带
  NIC 的快照**；
- 产物预期（断言依据）：`<bucket>/<prefix>/<digest16>/{rootfs.ext4,
  vmstate.snap, memory.snap, SHA256SUMS, manifest.json}` +
  `index/<sha256(image)>.json`。

### 3.4 registryconfig（agent 只读凭据挂载文件）

```json
// /etc/fast-sandbox/registry/registry.json（或 WORK 下，agent 经 env 指定）
{
  "revision": "sha256:...",
  "credentials": [{
    "host": "127.0.0.1:9000",
    "repositoryPrefix": "",
    "username": "chain-test",
    "password": "chain-test-secret"
  }]
}
```

- 验证 `resolveCredential`（cmd/firecracker-runtime-agent/main.go）：
  store root `s3://sandbox-images/publish` → host 匹配 → 凭据解析；
- **字段映射验证点**：`publishSecretRef`（accessKeyId/secretAccessKey/
  endpoint/region）→ registryconfig（Host/Username/Password）——两端
  值一致、语义等价（设计 §1.4 的实测确认）。

### 3.5 agent 与 driver

- agent：`go build ./cmd/firecracker-runtime-agent`，env：
  `FAST_SANDBOX_RUNTIME_AGENT_SOCKET=/run/fast-sandbox/firecracker/runtime.sock`、
  `FAST_SANDBOX_ARTIFACT_STORE=s3://sandbox-images/publish`、
  `FAST_SANDBOX_STATE_ROOT=/var/lib/fast-sandbox/firecracker`、
  `FAST_SANDBOX_REGISTRY_CONFIG_PATH=<registry.json 路径>`；
- driver：`cmd/fastlet` 或 E2E 测试直接接线（
  `FAST_SANDBOX_RUNTIME_AGENT_SOCKET` 注入，空 = 本地模式）。

## 4. 测试计划（三层场景 + 四个验证点）

### 场景 A：拉取链（builder → MinIO → agent pull）

1. builder 真实 publish（§3.3）→ 断言对象齐全（验证点 1）；
2. agent 启动，`PinImage(image)`（UDS 或直接 `agentpull.Client.PullImage`）
   → 空缓存全量拉取成功（验证点 2：SigV4 对 MinIO 正确）；
3. 二次 `PinImage` 幂等（不重复回源，MinIO 访问日志/计数断言）；
4. 缓存落盘布局断言（§5）；
5. credential 映射断言（验证点 3）。

### 场景 B：restore（builder 快照 → driver restore）

1. builder 产物（带 NIC 快照，§2 改动后）落入 driver 缓存
   （`<StateRoot>/images/<sha256(image)>/`，经 agent pull 或直接布局）；
2. driver `EnsureSandbox` restore 启动：VM Running + **guest
   reachability**（baked guest IP 可达，DNAT 后）；
3. 多 sandbox 从同一快照集 restore：共享 memory.snap（COW）互不干扰
   （验证点 4：builder 快照与 driver restore 兼容）。

### 场景 C：全链路（连续场景 A + B）

- 一键脚本：`scripts/firecracker-chain-e2e.sh`——起 MinIO → builder
  publish → agent pull → driver restore → guest reachability → 断言
  汇总 → 清理（MinIO 容器、bucket、缓存、tap）。

### 验证点清单

| # | 验证点 | 断言 |
|---|--------|------|
| 1 | builder 真实发布产物布局 | `digest16/` 下 5 个对象 + `index/<sha256(image)>.json`；`manifest.files` sha256 与文件一致；`SHA256SUMS` 行与文件一致；index 最后上传语义（拉取时产物完整） |
| 2 | SigV4 签名对 MinIO | agent 拉取成功（签名正确性由成功落盘 + digest 校验证明）；若环境有 OSS 测试桶，追加 region/endpoint 覆盖验证 |
| 3 | credential 映射 | `resolveCredential` 解析 MinIO 凭据成功；pull 鉴权通过；错误凭据 → 明确失败（4xx 不被重试） |
| 4 | builder 快照 ↔ driver restore | restore 成功 + Running + guest reachability；manifest machine tuple 与快照一致（`validateRestoreMachineConfig` 通过）；多实例共享 memory.snap |
| 5 | 幂等与清理 | 二次 PinImage 幂等；删除 sandbox 后 ReleaseDevices/UnpinImage 生效；脚本退出后 MinIO/缓存/tap 清理干净 |

## 5. 缓存与产物断言（场景 A 后）

```text
<StateRoot>/images/<sha256(image)>/
├── rootfs.img       # == publish rootfs.ext4（sha256 一致）
├── vmstate.snap     # == publish vmstate.snap
├── memory.snap      # == publish memory.snap
└── manifest.json    # == publish manifest.json（digest == index.artifactDigest）
```

- `.prep-version` 标记仅 E2E 自举使用；真实产物路径不需要（pull 层
  以 manifest 为 commit 点）；
- builder manifest 新增字段（§2）：guestIP/guestMAC（若实现 §2 时
  落进 manifest，driver 侧可校验与 network_overrides 期望一致）。

## 6. 验收标准

1. 场景 A/B/C 全部通过（一键脚本全绿）；
2. 验证点 1-5 断言全部覆盖；
3. builder 快照带 NIC 改动合入（§2），`sandboxtemplate-e2e.sh` 同步
   更新且保持绿色；
4. 签名/凭据/布局三个"纸上假设"全部有真实环境证据；
5. 输出记录：`docs/guides/firecracker-chain-e2e.md`（环境 + 结果 + 实测
   数据，与 firecracker-runtime-e2e.md / sandboxtemplate-golden-image-e2e.md
   并列）。

## 7. 风险与已知问题

| 风险 | 影响 | 缓解 |
|------|------|------|
| builder NIC 改动跨团队/超范围 | 链路验证 blocked | 退化为"拉取链完整 + restore 用自举快照"，restore 兼容性开 issue 跟踪（§2 注） |
| OSS 与 MinIO 签名差异（region 规范/endpoint style） | 生产 OSS 拉取失败 | 验证点 2 加 OSS 测试桶（可选）；记录差异 |
| builder 快照与 driver restore 的 machine/boot args 细节不兼容 | restore 失败 | 验证点 4 逐项对账（manifest machine vs vmstate 实际值），差异修 builder 或 driver |
| MinIO 与真实 S3 的 ETag/加密/多部分差异 | 大对象行为差异 | 本验证用小对象（<1GiB）；大对象留待生产前专项 |

## 8. 交付物

- `scripts/firecracker-chain-e2e.sh`（一键全链路 + 清理）+ `docs/guides/
  firecracker-chain-e2e.md`（环境/结果/实测数据）；
- builder snapshot_stage NIC 改动（§2）+ sandboxtemplate 文档同步；
- PR 描述：四个验证点结论、实测签名/凭据/布局证据、与自举 E2E 的
  差异说明（哪些断言从此由真实产物覆盖）。
