# Fast Sandbox v1alpha1 配置迁移指南

本轮重构保留旧字段的有界读取能力，但所有新配置必须使用 canonical API。迁移工具只读本地 YAML 并输出转换结果，不连接 Kubernetes，也不会修改集群对象。

## SandboxPool 字段映射

| 旧字段 | 新字段 / 行为 |
|---|---|
| `spec.runtimeType` | `spec.runtime` |
| `spec.runtimeClassName` | 删除；runtime profile 统一决定底层实现 |
| `spec.containerdRuntimeHandler` | 删除；runtime catalog 统一决定 handler |
| 缺失 `spec.sandboxResources` | 显式物化 `cpu: "1"`、`memory: 512Mi`、`pids: 256` |
| 缺失 `spec.maxSandboxesPerPod` | 显式物化 `5` |
| 缺失 `spec.infraProfile` | 显式物化 `minimal` |

canonical runtime 值为：`container`、`gvisor`、`kata-qemu`、`kata-clh`、`kata-fc`、`boxlite`。

旧的 `runtimeClassName` 或 `containerdRuntimeHandler` 只有在与内置 profile 完全一致时才能自动迁移。自定义 override 会直接报错，避免把原本不同的运行时语义静默映射到新 profile。

## 转换和检查

输出到 stdout：

```bash
fastctl migrate pool --file old-pool.yaml
```

输出到新文件：

```bash
fastctl migrate pool --file old-pool.yaml --output new-pool.yaml
```

CI 中检查配置是否已经 canonical：

```bash
fastctl migrate pool --file pool.yaml --check
```

`--check` 在仍需迁移时返回非零；已经 canonical 时返回 `0`。一个文件可以包含多个由 `---` 分隔的 SandboxPool 文档，但不能混入其他 Kind。

应用到集群前建议先做 server-side dry-run：

```bash
kubectl apply --server-side --dry-run=server -f new-pool.yaml
kubectl apply --server-side -f new-pool.yaml
```

迁移只改变字段表达，不改变有效 runtime、每 Pod 容量或旧对象缺省资源值。Pool runtime 和 Sandbox resource profile 仍然是不可变调度边界；如果旧 override 无法映射，创建一个新的 Pool 并迁移流量，不要强行修改现有 Pool 的语义。

## Sandbox 和 Create API 兼容字段

以下字段仍在 v1alpha1/protobuf 中保留，以便读取旧对象或兼容旧客户端，但新调用不得依赖它们：

- `CreateRequest.consistency_mode`：Create 固定执行 reservation -> CRD commit -> assignment CAS -> Fastlet Ensure；Fast/Strong 值不改变路径。
- `CreateRequest.exposed_ports` / `Sandbox.spec.exposedPorts`：不参与调度、网络分配或代理访问；Sandbox 私网可以使用任意目标端口。
- `CreateResponse.endpoints` / `Sandbox.status.endpoints`：不再生成 `PodIP:port`。客户端通过 `ResolveEndpoint` / `IssueRouteCredential` 和 Infra Component adapter 访问数据面。

所有 Create 客户端应发送稳定 `request_id`。相同 request ID 与相同 create spec 返回同一 Sandbox；相同 request ID 与不同 spec 返回冲突。
