# Fast Sandbox E2E Tests

`test/e2e` 正在重构为标准的 Kubernetes 风格 E2E 结构。

目标边界很明确：

- `hack/` 只负责环境准备
- `suites/` 是唯一的可执行 E2E 入口
- `support/` 放 fast-sandbox 领域的 E2E 支撑代码
- `legacy/` 只保留旧 shell case 作为参考

## 目录职责

### `test/e2e/hack/`

Shell 脚本只做这些事：

- kind 集群创建和销毁
- 镜像构建和加载
- controller / agent / janitor 安装
- webhook 或 chaos 辅助环境准备

Shell 不再承载业务断言。

### `test/e2e/suites/`

所有真正的 E2E case 都应放在这里，并使用 Go 编写。这里会逐步切换到 `sigs.k8s.io/e2e-framework` 风格的 suite。

当前已迁移的测试套件：

| Suite | Tests | Description |
|-------|-------|-------------|
| `basicvalidation` | 5 | CRD 验证、namespace 隔离、端口验证、环境变量/工作目录 |
| `lifecycle` | 2 | 基础生命周期、优雅关闭 |
| `scheduling` | 3 | 端口互斥、资源槽容量、自动扩缩容 |
| `cleanupjanitor` | 2 | Namespace 感知、Janitor 恢复 |
| `advancedfeatures` | 1 | Infra 注入验证 |
| `cliintegration` | 3 | fsb-ctl update/reset/logs 命令测试 |
| `faultrecovery` | 4 | 自动过期、内存泄漏防护、受控恢复、Pod 存在性检查 |

### `test/e2e/support/`

这里放仓库专用的 E2E 支撑能力，例如：

- `SandboxPool` / `Sandbox` fixture
- CLI 调用封装
- port-forward 管理
- diagnostics 转储
- suite 级环境 bootstrap

### 旧 shell 目录

当前 `01-basic-validation` 到 `07-fault-recovery` 仍然存在，但它们会被逐步降级为 legacy source。新的行为实现不应该继续写进这些目录。

## 运行入口

### 前置条件

1. **Kubernetes 集群**：需要有可访问的 K8s 集群，并配置好 kubeconfig
2. **Controller 已部署**：fast-sandbox-controller 需要运行在集群中
3. **环境变量**：
   - `FAST_SANDBOX_E2E=1` - 必须，否则测试会跳过
   - `FAST_SANDBOX_AGENT_IMAGE` 或 `AGENT_IMAGE` - Agent 镜像地址（默认 `fast-sandbox/agent:dev`）
   - `KUBECONFIG` - kubeconfig 文件路径（可选，默认使用 `~/.kube/config`）

### 运行所有测试

```bash
# 设置环境变量
export FAST_SANDBOX_E2E=1
export FAST_SANDBOX_AGENT_IMAGE=fast-sandbox/agent:dev

# 运行所有测试套件
go test ./test/e2e/suites/... -v

# 运行特定套件
go test ./test/e2e/suites/basicvalidation/... -v
go test ./test/e2e/suites/lifecycle/... -v
go test ./test/e2e/suites/scheduling/... -v
go test ./test/e2e/suites/cleanupjanitor/... -v
go test ./test/e2e/suites/advancedfeatures/... -v
go test ./test/e2e/suites/cliintegration/... -v
go test ./test/e2e/suites/faultrecovery/... -v

# 运行单个测试
go test ./test/e2e/suites/basicvalidation/... -v -run TestCRDValidation
```

### 运行特定标签的测试

```bash
# 只运行 smoke 级别的测试
go test ./test/e2e/suites/... -v --label-filter=tier=smoke
```

### Makefile 目标（如果已配置）

```bash
make e2e-smoke
make e2e-cli
```

当前仓库仍保留旧 shell 结构，但新的权威方向已经是 `hack + suites + support`。

## 调试技巧

查看 Controller 日志：

```bash
kubectl logs -l app=fast-sandbox-controller -n fast-sandbox-system --tail=50
```

查看 Agent 日志：

```bash
kubectl logs -l app=sandbox-agent -n <namespace> --all-containers --tail=50
```

查看 Sandbox 状态：

```bash
kubectl get sandbox <name> -n <namespace> -o yaml
```
