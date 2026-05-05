# Fast Sandbox E2E 测试

`test/e2e` 包含 Fast Sandbox 的端到端测试。这些测试会创建真实的 Kubernetes 资源，构建并加载项目镜像，并在 kind 集群中验证 controller、fastlet、`fastctl`、gVisor 和 Kata 的行为。

所有可执行的 e2e 测试用例均位于 `test/e2e/suites`。Go 测试会自动准备对应的运行环境，因此可以从 IDE 或命令行直接执行单个测试用例，无需预先手动执行 setup 脚本。

## 测试目录

| 目录 | Profile | 覆盖内容 |
| --- | --- | --- |
| `suites/basicvalidation` | `basic` | CRD 校验、namespace 隔离、端口、环境变量、工作目录、fast-path 环境传递 |
| `suites/lifecycle` | `basic` | sandbox 生命周期、优雅关闭 |
| `suites/scheduling` | `basic` | 端口互斥、资源槽容量、自动扩缩容 |
| `suites/cleanupjanitor` | `basic` | namespace 感知清理、janitor 恢复 |
| `suites/advancedfeatures` | `basic` | infra 注入 |
| `suites/cliintegration` | `basic` | `fastctl update`、`reset`、`logs`、`run` |
| `suites/faultrecovery` | `basic` | 自动过期、内存泄漏防护、受控恢复、Pod 存在性检查 |
| `suites/secureruntime` | `basic`、`gvisor`、`kata-qemu`、`kata-clh`、`kata-fc` | RuntimeClass 校验、gVisor、Kata QEMU、Kata Cloud Hypervisor、Firecracker 调查 |

支撑代码组织如下：

- `env`: profile 自动 setup、kind 集群准备、runtime 配置、镜像加载、组件部署、`fastctl` 封装
- `manifests`: profile 使用的 kind 和 RuntimeClass manifest
- `support`: fixture、CLI client、诊断、port-forward、suite 环境工具

## 运行要求

执行这些测试需要 Linux 开发机，并安装以下工具：

- Go
- Docker
- kind
- kubectl
- make

如果在 macOS 上开发，不应在本地直接执行 kind、gVisor、Kata 或 container runtime 相关测试。请按照仓库根目录 `AGENTS.md` 的说明，通过远端 Linux VM 运行。

测试默认构建并加载这些镜像：

```bash
fast-sandbox/controller:dev
fast-sandbox/fastlet:dev
fast-sandbox/janitor:dev
```

可以通过以下环境变量覆盖 sandbox pool 使用的 fastlet 镜像：

```bash
FAST_SANDBOX_FASTLET_IMAGE=my-registry/fastlet:tag
```

## 运行测试

运行所有 suite：

```bash
go test ./test/e2e/suites/... -v -count=1 -timeout 30m
```

运行单个 suite：

```bash
go test ./test/e2e/suites/basicvalidation/... -v -count=1
go test ./test/e2e/suites/cliintegration/... -v -count=1
go test ./test/e2e/suites/secureruntime/... -v -count=1 -timeout 30m
```

运行单个测试用例：

```bash
go test ./test/e2e/suites/basicvalidation/... -run 'TestSandboxCRDValidation$' -v -count=1
go test ./test/e2e/suites/secureruntime/... -run 'TestKataQemuSandbox$' -v -count=1 -timeout 30m
go test ./test/e2e/suites/secureruntime/... -run 'TestGVisorSandbox$' -v -count=1 -timeout 30m
```

也可以使用 Makefile 入口：

```bash
make test-e2e
make test-e2e-basicvalidation
make test-e2e-cliintegration
make test-e2e-secureruntime
```

## 单独准备环境

默认情况下无需提前准备环境。测试中的 `suiteenv.Require*` 会自动完成 setup。如需单独准备某个 profile，可以执行：

```bash
go run ./test/e2e/env/cmd/setup -profile basic
go run ./test/e2e/env/cmd/setup -profile gvisor
go run ./test/e2e/env/cmd/setup -profile kata-qemu
go run ./test/e2e/env/cmd/setup -profile kata-clh
```

Makefile 入口：

```bash
E2E_PROFILE=basic make setup-e2e
E2E_PROFILE=kata-qemu make setup-e2e
```

Profile 与 kind 集群对应关系：

| Profile | 集群 | 说明 |
| --- | --- | --- |
| `basic` | `fsb-e2e-basic` | 默认 container runtime |
| `gvisor` | `fsb-e2e-gvisor` | 配置 `runsc` RuntimeClass |
| `kata-qemu` | `fsb-e2e-kata` | 配置 Kata QEMU RuntimeClass |
| `kata-clh` | `fsb-e2e-kata` | 配置 Kata Cloud Hypervisor RuntimeClass |
| `kata-fc` | `fsb-e2e-kata` | Firecracker 目前为 opt-in 调查项 |

## 在 IDE 里调试

可以打开 `test/e2e/suites` 下的任意测试文件，并在 IDE 中运行或调试单个 `Test*`。测试进入用例主体前会调用：

```go
suiteenv.RequireBasic(t)
suiteenv.RequireGVisor(t)
suiteenv.RequireKataQemu(t)
suiteenv.RequireKataClh(t)
suiteenv.RequireKataFc(t)
```

如果缺少命令、宿主机设备或 runtime 二进制，测试会在 setup 阶段返回明确错误。gVisor 或 Kata 缺少 RuntimeClass 会被视为环境准备失败，不会被静默跳过。

## Secure Runtime 说明

gVisor 需要宿主机提供以下可执行文件。默认路径：

```bash
/usr/local/bin/runsc
/usr/local/bin/containerd-shim-runsc-v1
```

需要覆盖默认路径时：

```bash
GVISOR_RUNSC_BIN=/path/to/runsc \
GVISOR_SHIM_BIN=/path/to/containerd-shim-runsc-v1 \
go test ./test/e2e/suites/secureruntime/... -run 'TestGVisorSandbox$' -v -count=1
```

Kata profile 需要宿主机支持 nested virtualization，并提供以下路径：

```text
/dev/kvm
/sys/devices/virtual/misc/kvm
/dev/vhost-vsock
/sys/devices/virtual/misc/vhost-vsock
/dev/vhost-net
/sys/devices/virtual/misc/vhost-net
/dev/net/tun
/dev/shm
```

Kata 静态包默认配置：

```bash
KATA_VERSION=3.27.0
KATA_ARCH=amd64
KATA_CACHE_DIR=$HOME/.cache
DATA_DIR=$HOME/data
```

`kata-fc` 当前默认跳过。2026-05-04 在远端 kind 环境中，最小 `RuntimeClass=kata-fc` pod 在 Fast Sandbox 介入前即失败，表现为 Firecracker hybrid vsock `/root/kata.hvsock` 连接超时。如需显式调查：

```bash
FAST_SANDBOX_E2E_KATA_FC=1 \
go test ./test/e2e/suites/secureruntime/... -run 'TestKataFcSandbox$' -v -count=1 -timeout 30m
```

## 故障排查

查看当前 context 和 Pod 状态：

```bash
kubectl config current-context
kubectl get pods -A -o wide
```

如果 kube-proxy 或 kind node systemd 出现 `Too many open files`，需要调大 Linux 宿主机的 inotify 限制：

```bash
sudo sysctl -w fs.inotify.max_user_instances=1024
```

查看 controller 日志：

```bash
kubectl logs deployment/fast-sandbox-controller --tail=120
```

查看资源状态：

```bash
kubectl get sandbox -A
kubectl get sandboxpool -A
```

如需完全重置某个 profile，可以删除对应的 kind 集群：

```bash
kind delete cluster --name fsb-e2e-basic
kind delete cluster --name fsb-e2e-gvisor
kind delete cluster --name fsb-e2e-kata
```

## 新增测试规范

新增面向用户行为的 e2e 测试用例时，应放置在 `test/e2e/suites/<suite>`。在测试入口选择所需 profile：

```go
suiteenv.RequireBasic(t)
suiteenv.RequireGVisor(t)
suiteenv.RequireKataQemu(t)
suiteenv.RequireKataClh(t)
suiteenv.RequireKataFc(t)
```

创建 sandbox 的用户路径应优先使用 `fastctl`。只有当测试用例明确验证 CRD 或 controller 内部行为时，才直接使用 Kubernetes client fixture。
