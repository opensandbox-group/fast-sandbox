# 阶段 1 施工方案：消费端 PullImage 拉取链

> 文档类型：施工任务书（转交实现）
>
> 日期：2026-08-27
>
> 前置依赖：opensandbox-group/fast-sandbox#22（publish 端 index + SHA256SUMS，
> 已合入发布侧契约）；#21（设计文档，draft）。
>
> 实现人按本任务书独立完成，有分歧以本任务书 + #21 设计文档为准。

---

## 1. 背景与目标

发布端（builder，#22）已能产出并发布：

```text
s3://<publish>/
├── <sha256(manifest)[:16]>/           # per-build digest 命名空间
│   ├── rootfs.ext4 / vmstate.snap / memory.snap
│   ├── SHA256SUMS
│   └── manifest.json                   # 最后上传
└── index/<sha256(imageRef)>.json      # 引用索引，指向最新 build 的 manifest
```

消费端目前 `internal/runtime/firecracker/images.go` 的 `PullImage` 只查本地
缓存（`<StateRoot>/images/<sha256(image)>/rootfs.img`），未命中返回
`ErrImageNotReady`，**从不拉取**。

本任务实现寻址链消费端：`SandboxSpec.Image → index → manifest → 按 digest
拉取 → sha256 校验 → 落盘`，使 `Image` 引用能真正消费发布产物。

**范围边界（本次不做）**：

- 不接线 UDS / agent server（本任务只做 agent 包内的纯拉取层，`driver` 的
  `PullImage` 暂不迁移）；
- 不做 P2P（阶段 2）、不做 overlaybd（阶段 3）、不做 presigned 签发
  （S3 客户端直接持只读 AK/SK，凭据注入方式后续随 agent 接线调整）；
- 不改 fastlet 协议、不改 `images.go` 现有缓存 key 与 GC 语义。

## 2. 代码位置与包结构

新建包 `internal/runtime/firecracker/agent/`（内聚在 firecracker 领域内，
镜像 `internal/runtime/boxlite/{driver,protocol,server,state}` 的形态）：

```text
internal/runtime/firecracker/agent/
├── pull.go          # PullImage 主流程（幂等编排）
├── index.go         # 引用索引解析（index/<sha256(image)>.json）
├── manifest.go      # manifest 解析与校验（files.digest / layers）
├── s3client.go      # S3 只读 GET 客户端
├── cache.go         # 缓存落盘：临时文件 → 校验 → rename；幂等检查
└── *_test.go        # 对应单测
```

包级导出面（后续 UDS server 与 driver client 消费）：

```go
type Client struct { /* store root + 凭据，构造时注入 */ }

func NewClient(storeRoot string, credential registryconfig.Credential) (*Client, error)
func (c *Client) PullImage(ctx context.Context, stateRoot, image string) error
```

## 3. 数据流（PullImage 主流程）

```text
PullImage(ctx, stateRoot, image):
  1. 校验 image 非空（空串直接报错，对齐发布端 guard 与 images.go 语义）
  2. key := sha256(image)；dir := <stateRoot>/images/<key>
  3. 幂等检查：dir/manifest.json 存在 且 files 中 native 集（见 §5）全部
     在盘且 sha256 匹配 → 返回 nil（不重复拉取）
  4. GET <store>/index/<sha256(image)>.json
       - 404 → ErrImageNotReady（镜像未发布）
       - 解析 {image, manifestRef, artifactDigest, updatedAt}
       - 校验 index.image 与入参 image 逐字节一致（防错索引）
  5. GET manifestRef → manifest.json
       - 校验 sha256(manifest 内容) == index.artifactDigest（防篡改/错配）
       - 解析 manifest.files（publish 文件名 → {sha256, sizeBytes}）
  6. 按 native 集逐个拉取：
       - 目标文件已存在且 sha256 匹配 → 跳过（断点续传语义）
       - 否则：S3 GET 到 <name>.tmp-<rand> → 边收边算 sha256
         → 校验与 manifest.files 一致 → rename 为最终名
         → 校验失败：删 tmp，返回错误（缓存损坏同理删除重拉）
  7. 最后写 dir/manifest.json（commit 点：manifest 在盘 = 本次拉取完成）
  8. 全部完成返回 nil
```

**并发安全**：同一 image 的并发 PullImage 允许竞态（都以完整校验 + rename
收尾，不存在半成品可见）；如需合并可用 per-key 互斥锁（本次可选）。

## 4. S3 客户端

**方案 A（推荐，实现成本 ~150 行，零新依赖）**：轻量 SigV4 GET 客户端

- 只实现 `GET /<bucket>/<key>`（path-style，兼容 endpoint 覆盖的
  OSS/MinIO）；range GET 预留接口但不实现（阶段 3 用）；
- SigV4 签名：`AK/SK + region + service=s3`，请求头 `Host/X-Amz-Date/
  X-Amz-Content-Sha256/Authorization`；
- 凭据结构复用 `internal/registryconfig.Credential`
  （Host/RepositoryPrefix/Username/Password/IdentityToken）；
- store root 解析：`s3://bucket/prefix` → bucket + prefix；
- 网络：`http.Client`，单请求超时可配（默认 5min，大文件），重试仅对
  5xx/网络错误（指数退避，参考 publish.go 的 uploadWithRetry 模式）。

**方案 B（备选）**：引 `aws-sdk-go-v2`（`s3` + `credentials`）——依赖重，
仅当方案 A 实现受阻时启用。

**测试**：httptest server 模拟 S3（`GET /bucket/prefix/...`），验证
path 构造、query、请求头、5xx 重试；不验证真实签名细节（签名正确性由
集成验收兜底）。

## 5. 缓存布局与 native 文件名映射

```text
<StateRoot>/images/<sha256(image)>/
├── rootfs.img        # ← publish 的 rootfs.ext4
├── vmstate.snap
├── memory.snap
└── manifest.json     # commit 点（最后写入）
```

- 文件名映射（publish 名 → 缓存名）：`rootfs.ext4 → rootfs.img`（与现状
  `resolveRootfsImage` 的期望一致，下游零改动）；vmstate/memory 同名；
- overlaybd 的 `overlaybd/*/layer.lsmt` **本次不拉**（阶段 3）；
- 校验粒度：整文件 sha256（manifest.files 的 `sha256` 字段）；
- 缓存损坏处理：`rootfs.img` 等文件存在但 sha256 不匹配 → 删除该文件
  重新拉取（不删 manifest，manifest 由步骤 3 的完整性校验决定）；
- GC：不动现有 `garbageCollectImages`（它按 digest 目录 RemoveAll，天然
  覆盖新文件）。

## 6. 错误语义

| 场景 | 错误 |
|------|------|
| image 为空/空白 | 校验错误（发布端同款 guard 语义） |
| index 404（镜像未发布） | `ErrImageNotReady`（保留现有语义） |
| index 存在但 manifest 404 | 显式错误（发布不完整，index 指向缺失 build） |
| manifest digest ≠ index.artifactDigest | 显式错误（防错配） |
| 文件 sha256 ≠ manifest.files | 显式错误 + 清理该文件（防损坏缓存） |
| 网络/5xx（重试耗尽） | 包装错误（可重试） |

`ErrImageNotReady` 沿用 `internal/runtime/firecracker/images.go` 的既有
sentinel（同包可复用；agent 包引用时通过主包导出或迁移到公共位置，实现时
选最顺的方式并说明）。

## 7. 测试要求

- **单元**（不依赖真实 S3）：
  - index 解析：正常 / 404 / 字段缺失 / image 不匹配；
  - manifest 解析与 digest 校验：正常 / digest 不匹配；
  - 拉取流：httptest 模拟 S3，验证 tmp 落盘 → 校验 → rename 顺序；
  - 幂等：已完整缓存 → 不发起任何请求（httptest 计数断言）；
  - 断点续传：文件已存在且匹配 → 跳过（请求计数断言）；
  - 损坏缓存：sha256 不匹配 → 删除重拉；
  - 空 image 拒绝。
- **集成验收**（可选，有 MinIO 环境时）：
  - 用手工构造的 artifact 集（按 #22 的发布结构）上传 MinIO，
    PullImage 空缓存全量拉取成功；二次调用幂等。

## 8. 验收标准

1. `go build ./...`、`go test ./internal/runtime/firecracker/... -count=1`、
   `go vet ./...` 全绿；
2. 单测覆盖 §7 清单；
3. `PullImage` 拉取后缓存布局与 §5 一致；`resolveRootfsImage` 能命中
   （下游 `EnsureSandbox` restore 路径可用）；
4. 不引入新三方依赖（方案 A 下 go.mod 零变化）。

## 9. 参考材料

- 设计文档：`docs/design/firecracker-on-demand-loading.md`（寻址链 §Artifact
  契约、阶段 1）；细节文档 §1（index 语义约束：**image 引用逐字节一致、
  last-writer-wins、64 位命名空间不得缩短**）；
- 发布端实现（#22）：`cmd/sandboxtemplate-builder/publish.go`（index
  对象内容与上传顺序）、`manifest.go`（files 结构）；
- 现有消费端：`internal/runtime/firecracker/images.go`（imageKey、
  resolveRootfsImage、GC）、`internal/registryconfig/types.go`
  （Credential 结构）；
- 错误风格参考：`internal/runtime/contract/errors.go`。

## 10. 交付物

- 上述包 + 单测；
- 实现说明（PR 描述）：S3 客户端方案选型结论（A/B）、agent 包与主包的
  ErrImageNotReady 复用方式、与 `images.go` 的衔接（哪些留待接线时迁移）。
