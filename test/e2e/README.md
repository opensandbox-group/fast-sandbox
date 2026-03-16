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

目标 suite 包括：

- `basicvalidation`
- `scheduling`
- `lifecycle`
- `janitor`
- `advanced`
- `cli`
- `recovery`

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

重构完成后的主入口会是：

```bash
go test ./test/e2e/suites/...
```

以及：

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
