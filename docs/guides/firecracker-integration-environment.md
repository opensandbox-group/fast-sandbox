# Firecracker 全链路集成环境：Kind 单机方案

> 文档类型：集成环境方案（实操指南）
>
> 日期：2026-08-29
>
> 目标：在一台裸金属服务器（KVM + Docker）上搭建 fast-sandbox 全系统
> 集成环境，跑通完整链路：**SandboxTemplate 制作（controller 驱动 Job）
> → publish MinIO → fastlet 创建 sandbox（agent pull + golden restore）
> → execd 交付验证**。
>
> 已确认决策：K8s 发行版 = **Kind**；模板制作 = **方案 A**（SandboxTemplate
> controller → builder Job，K8s 内跑）；agent 部署载体 = **独立 DaemonSet**
> （设计待决策项 4 定案）。

---

- [拓扑](#拓扑)
- [关键决策记录](#关键决策记录)
- [组件清单与部署形态](#组件清单与部署形态)
- [前置：Kind 集群（KVM 透传）](#前置kind-集群kvm-透传)
- [分步搭建](#分步搭建)
- [关键实现细节](#关键实现细节)
- [现有资产复用 / 新增清单](#现有资产复用--新增清单)
- [端到端验收清单](#端到端验收清单)
- [后续演进（阶段 2+ 接入点）](#后续演进阶段-2-接入点)

## 拓扑

```text
┌──────────────── 裸金属服务器（/dev/kvm + 504GiB + Docker）──────────────┐
│                                                                          │
│  Docker（宿主）：MinIO（:9000，对象存储）                                │
│  ┌─ Kind 集群（单节点 kindest/node，extraMounts 透传 KVM 设备）────────┐ │
│  │  ┌─ controller（Deployment，config/all-in-one）───────────────────┐ │ │
│  │  ├─ SandboxTemplate CR → builder Job（privileged + /dev/kvm +   │ │ │
│  │  │    /dev/net/tun + MinIO 凭据）→ publish 到 MinIO              │ │ │
│  │  ├─ firecracker-runtime-agent（DaemonSet：UDS + 缓存 + registry │ │ │
│  │  │    挂载 MinIO 只读凭据）                                      │ │ │
│  │  ├─ fastlet Pod（特权 + hostPath /dev/kvm + containerd socket + │ │ │
│  │  │    agent UDS socket）                                          │ │ │
│  │  ├─ fastlet-proxy / janitor（节点组件）                          │ │ │
│  │  └─ SandboxPool（firecracker + image + warmImages）→ Sandbox CR │ │ │
│  └───────────────────────────────────────────────────────────────────┘ │
│                                                                          │
│  链路：SandboxTemplate 构建 → publish MinIO → Pool 声明 image →        │
│        controller 调度 fastlet → agent PinImage（pull + 校验）→        │
│        driver golden restore（jailer --netns）→ execd 交付 → 验证       │
└──────────────────────────────────────────────────────────────────────────┘
```

## 关键决策记录

| 决策 | 结论 | 备注 |
|------|------|------|
| K8s 发行版 | **Kind**（单节点，kata.yaml 的 KVM 透传先例） | 可重制（一条命令重建）；功能链路验证充分 |
| 模板制作 | **方案 A**：SandboxTemplate controller → builder Job | controller 已支持 `/dev/kvm`+`/dev/net/tun` hostPath + privileged + 节点 label（`sandboxtemplate_controller.go:435-521`） |
| agent 部署载体 | **独立 DaemonSet**（待决策项 4 定案） | 与 fastlet 解耦；UDS socket 经 hostPath 共享 |
| runtime | **firecracker**（restore 唯一路径，无 kernel 依赖） | 节点运行时资产 = firecracker 二进制 + jailer |

## 组件清单与部署形态

| 组件 | 形态 | 关键挂载/凭据 |
|------|------|----------------|
| MinIO | 宿主 Docker 容器（:9000） | AK/SK（发布写 + 拉取只读可共用同一组起步） |
| controller | Deployment（config/all-in-one） | 集群内 RBAC；CRD 由 config/crd 安装 |
| builder Job | SandboxTemplate 驱动（privileged） | `/dev/kvm`、`/dev/net/tun`（hostPath）、MinIO 发布凭据（publishSecretRef → Job env） |
| firecracker-runtime-agent | **DaemonSet** | UDS socket 目录、缓存目录（hostPath）、registryconfig 挂载（MinIO 只读） |
| fastlet | SandboxPool.fastletTemplate（特权） | `/dev/kvm`、节点 containerd socket、agent UDS socket、registryconfig、runtime plan |
| fastlet-proxy / janitor | 节点组件 | 按现有部署形态 |
| execd | builder bake 进快照（`opensandbox/execd:1.1.0`） | 无独立部署 |

## 前置：Kind 集群（KVM 透传）

新增 `config/dev/kind-firecracker.yaml`（以现有 kata.yaml 的 extraMounts
为基础，firecracker 需要子集）：

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: /dev/kvm
    containerPath: /dev/kvm
  - hostPath: /sys/devices/virtual/misc/kvm
    containerPath: /sys/devices/virtual/misc/kvm
  - hostPath: /dev/net/tun
    containerPath: /dev/net/tun
  - hostPath: /dev/vhost-vsock
    containerPath: /dev/vhost-vsock
  - hostPath: /sys/devices/virtual/misc/vhost-vsock
    containerPath: /sys/devices/virtual/misc/vhost-vsock
  # /dev/shm 放大：builder 与 firecracker 快照需要
  - hostPath: /dev/shm
    containerPath: /dev/shm
```

- 创建：`kind create cluster --config config/dev/kind-firecracker.yaml`
- 节点 label：`kubectl label node <node> fast-sandbox.io/kvm-node=true`
  （builder Job 调度与 fastlet 亲和用）

## 分步搭建

### 步骤 1：服务器准备

```bash
# 依赖：docker, kind, kubectl, go, aws CLI, jq
# sysctl（参考 test/e2e/env/manager.go 的检查项）：
sysctl -w fs.inotify.max_user_instances=8192
# 构建二进制：
make build  # 或 go build ./cmd/...（controller/fastlet/firecracker-runtime-agent/fastctl）
```

### 步骤 2：Kind 集群（§前置）

### 步骤 3：MinIO + 凭据

- 宿主 Docker 起 MinIO（复用 chain-e2e 逻辑）：
  `docker run -d -p 9000:9000 -e MINIO_ROOT_USER=... -e MINIO_ROOT_PASSWORD=... minio/minio server /data`
- bucket：`sandbox-images`
- 两份凭据文件：
  - **发布凭据**（SandboxTemplate publishSecretRef）：
    `accessKeyId/secretAccessKey/endpoint: http://<kind-node-ip>:9000`——builder Job 在 Kind 节点容器内，MinIO 是宿主容器：Job 内访问用 **节点容器 → 宿主** 的地址（Kind 节点容器网络：`host.docker.internal` 不可用；用节点 IP（`kind get kubeconfig` 里的 server IP）或把 MinIO 也放进 Kind（不推荐，宿主 Docker 更简单）+ `--add-host` 或直接用宿主 Docker bridge IP（`172.17.0.1:9000`）——**实现时确认**（Job 网络出到宿主 Docker 桥 IP）
  - **拉取凭据**（registryconfig 挂载 fastlet/agent）：
    `{host: <endpoint>, username, password}`——fastlet/agent pod 到 MinIO 同理（hostPath/网络出宿主）
- 网络细节：**Kind 节点容器 → 宿主 Docker 容器**的通路 = Docker 默认桥（`172.17.0.1`）或 kind 网络；实现时实测选择，写入环境脚本。

### 步骤 4：CRD + controller

```bash
kubectl apply -k config/crd          # SandboxPool/SandboxTemplate CRD
kubectl apply -f config/all-in-one   # controller + service + PDB
kubectl rollout status deploy/controller
```

### 步骤 5：firecracker 节点资产 + runtime 环境

- **firecracker 二进制 + jailer** 放节点容器 hostPath 目录
  （如 `/opt/fast-sandbox/firecracker/{firecracker,jailer}`）：
  用 `kind load` 不可行（非镜像）——**在节点容器内直接放**：
  `docker cp` 到 kind 节点容器，或 config/runtime-installers 新增
  firecracker installer（DaemonSet init 容器拷贝）；实现时选后者
  （可重制）；
- **runtime 环境 ConfigMap**：`config/runtime-environments.yaml` 新增
  firecracker 条目——containerd socket（Kind 节点容器
  `/run/containerd/containerd.sock`）、namespace `k8s.io`、firecracker
  二进制/jailer 路径、StateRoot（节点容器内 hostPath，如
  `/var/lib/fast-sandbox/firecracker`）；
- **节点 label**：`fast-sandbox.io/firecracker-node=true`（Pool 亲和）。

### 步骤 6：agent DaemonSet

- 新增 `config/dev/agent-daemonset.yaml`：
  - hostPath：UDS socket 目录（`/run/fast-sandbox/firecracker`）、
    StateRoot（与 fastlet 共享）、registryconfig 文件；
  - env：`FAST_SANDBOX_RUNTIME_AGENT_SOCKET`、`FAST_SANDBOX_STATE_ROOT`、
    `FAST_SANDBOX_REGISTRY_CONFIG_PATH`；
  - mount：共享 ConfigMap `fast-sandbox-artifact-store` 挂到
    `/etc/fast-sandbox/artifact-store`（每次 pull 读取，改 CM 无需重启）；
  - 镜像：本地构建 `firecracker-runtime-agent`（`kind load docker-image`）。

### 步骤 7：SandboxTemplate（模板制作，方案 A）

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxTemplate
metadata: { name: ai-office-sandbox }
spec:
  image: registry.example.com/sandbox:v1
  execd: opensandbox/execd:1.1.0
  kernel: vmlinux.bin
  machine: { vcpu: "1", memory: "512Mi" }
  init: /usr/local/sbin/sandbox-init
  readiness: { warmupSeconds: 15 }
  output:
    rootfsSize: "2Gi"
    format: native
    publish: s3://sandbox-images/publish
    publishSecretRef: { name: sandbox-oss-credentials }
```

- builder Job 自动获得 `/dev/kvm`+`/dev/net/tun`（controller 已支持，
  节点需 label）；
- **SandboxTemplate controller 镜像**：`kind load docker-image controller`
  （或 config/all-in-one 引用本地镜像）；
- apply 后等 `status.phase=Succeeded` + `manifestRef`；
- 用 `mc`/aws CLI 核对 MinIO 布局（index + digest16 构建）。

### 步骤 8：SandboxPool（firecracker）

- 新增 `config/samples/pool-firecracker.yaml`：
  ```yaml
  spec:
    runtime: firecracker
    sandboxResources: { cpu: "1", memory: "512Mi" }
    warmImages: ["registry.example.com/sandbox:v1"]   # 预热触发 agent pull
    fastletTemplate:
      spec:
        containers:
        - name: fastlet
          image: <本地 fastlet 镜像>
          securityContext: { privileged: true }
          volumeMounts: [/dev/kvm, containerd.sock, agent socket, registry.json, runtime plan]
        # hostNetwork 按现状；nodeSelector: fast-sandbox.io/firecracker-node=true
  ```
- apply 后等 fastlet pod Running + warmImages 状态 Ready（agent 已
  PinImage → 缓存就绪）。

### 步骤 9：Sandbox 创建与交付验证

```bash
kubectl apply -f config/samples/sandbox-firecracker.yaml   # 或 fastctl create
# 断言链：
kubectl get sandbox -o wide                          # Phase=Running
kubectl logs <fastlet-pod> | grep "firecracker sandbox created"   # restore 阶段耗时
# execd 交付：从 fastlet pod 网络（或 Kind 节点容器）访问
curl http://<slot.IP>:44772/ping                     # execd /ping OK
# 重复创建第二个 sandbox：验证 clone 网络（共享快照 + per-clone netns）
```

### 步骤 10：一键脚本固化

新增 `scripts/integration-env.sh`（对齐 chain-e2e.sh 风格）：

```text
up     # 环境准备 + Kind + MinIO + controller + 节点资产 + agent + 模板 + pool
down   # 清理（kind delete + MinIO 容器）
status # 各组件健康 + 模板状态 + warmImages 状态
verify # 步骤 9 的断言链（sandbox Running + execd /ping）
```

## 关键实现细节

| 细节 | 处理 |
|------|------|
| Kind 节点容器 → 宿主 MinIO 通路 | Job/pod 用宿主 Docker 桥 IP（`172.17.0.1:9000`）或 kind 网络实测；写入脚本 |
| fastlet 的 containerd socket | Kind 节点容器 `/run/containerd/containerd.sock`（hostPath 挂入 fastlet pod） |
| /dev/kvm 两层透传 | 宿主 → Kind 节点（extraMounts）→ fastlet pod/builder Job（hostPath） |
| firecracker 二进制/jailer 进节点 | DaemonSet init 容器拷贝到 hostPath（可重制），runtime plan 指向 |
| execd bake | SandboxTemplate `spec.execd=opensandbox/execd:1.1.0`（chain-e2e 验证 tag） |
| guestNetwork | builder bake 的 guest IP（172.30.0.3 约定）与 #28 per-clone netns 一致 |
| registryconfig | fastlet/agent 挂载同一 registry.json（MinIO 只读凭据，Host=MinIO endpoint） |
| 镜像进集群 | `kind load docker-image`（controller/fastlet/agent/fastlet-proxy） |

## 现有资产复用 / 新增清单

| 资产 | 状态 |
|------|------|
| `config/crd`（CRD）、`config/all-in-one`（controller） | 复用 |
| `config/dev/kind-firecracker.yaml`（KVM 透传） | **新增**（kata.yaml 精简） |
| `config/runtime-environments.yaml`（firecracker 条目） | **新增** |
| `config/runtime-installers/`（firecracker 二进制/jailer 安装） | **新增** |
| `config/dev/agent-daemonset.yaml` | **新增** |
| `config/samples/pool-firecracker.yaml` | **新增** |
| `config/samples/sandbox-firecracker.yaml` | **新增** |
| `scripts/integration-env.sh` | **新增**（up/down/status/verify） |
| builder/MinIO 逻辑（chain-e2e.sh） | 逻辑抽取 |

## Egress 集成部署（network policy 通道，actions 驱动）

方案与任务清单见 `docs/design/egress-integration-plan.md` /
`egress-integration-plan-tasks.md`。本期仅 actions 通道：策略经
`SET_BINDING.binding.input` 传递，credential 通道（proxy route / UID /
vault）延后。

1. **镜像**：`docker.io/opensandbox/egress:latest`（需求方提供），
   `kind load docker-image docker.io/opensandbox/egress:latest` 进集群；
2. **Pool**：apply `config/samples/pool-firecracker-egress.yaml`
   （= pool-firecracker + `actionHandlers[egress]` + host-process
   infra 组件 + fastletTemplate 追加 egress 容器）；
3. **策略**：server 在 Sandbox `spec.actionBindings[egress].input` 写入
   策略（opaque string 编码，与 egress 侧核对 base64/JSON 约定）；
4. **验证**：`integration-env.sh verify-egress`（生命周期 deny-first →
   runtime-ready → data-plane-ready → active；per-subject 隔离；
   firecracker src=slot.IP；REMOVE_BINDING 清理；egress 重启重放）。

## 端到端验收清单

1. `SandboxTemplate` Succeeded + MinIO 产物布局正确（index/digest16/
   SHA256SUMS/manifest 与 pull 层解析一致）；
2. fastlet pod Running，`warmImages` Ready（agent PinImage 幂等完成，
   缓存落盘）；
3. `Sandbox` Phase=Running；fastlet 日志含 restore 阶段耗时；
4. **execd /ping 可达**（交付验证）；
5. **第二个 sandbox**：clone 网络（共享快照 + per-clone netns），
   per-instance slot.IP 可达（issue #26 语义在集成环境的证明）；
6. 删除 sandbox：agent 引用归零、jail 目录清理；
7. `integration-env.sh down` 后宿主无残留（kind 集群 + MinIO 容器清理）。

## 后续演进（阶段 2+ 接入点）

- **阶段 2（DART）**：agent DaemonSet 增加 DART 进程（同容器），
  pull 路径切 DART 前缀模式——本环境的 MinIO 即 origin，可直接验证
  P2P（加第二个节点/复用现有）；
- **快照（阶段 4）**：`SandboxSnapshot` CRD + 保存/恢复；
- 多节点扩展：Kind 多节点（extraMounts 每节点配）或迁移 K3s
  （生产形态验收）。
