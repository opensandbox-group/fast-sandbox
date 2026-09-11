# 集成环境搭建任务清单（转交施工）

> 文档类型：施工任务清单（转交实现）
>
> 日期：2026-08-29
>
> 方案：见 [firecracker-integration-environment.md](../guides/firecracker-integration-environment.md)
> （本清单的每一项对应方案中的步骤；分歧以方案文档为准）。
>
> 环境：裸金属服务器（KVM + Docker + 504GiB），root 权限。

---

## 任务总览

| # | 任务 | 产出物 | 依赖 |
|---|------|--------|------|
| 1 | 服务器准备与工具链 | 安装脚本 | — |
| 2 | Kind 集群（KVM 透传） | `config/dev/kind-firecracker.yaml` | 1 |
| 3 | MinIO + 凭据（发布/拉取两份） | 启动脚本 + 凭据文件 | 1 |
| 4 | CRD + controller 部署 | 应用清单（复用 config/） | 2 |
| 5 | firecracker 节点资产（二进制/jailer）+ runtime 环境 | `config/runtime-installers/` + `config/runtime-environments.yaml` 条目 | 2 |
| 6 | agent DaemonSet | `config/dev/agent-daemonset.yaml` | 3,4,5 |
| 7 | SandboxTemplate（模板制作） | 样例 CR + MinIO 产物验证 | 3,4,5 |
| 8 | SandboxPool（firecracker） | `config/samples/pool-firecracker.yaml` | 4,5,6 |
| 9 | Sandbox 创建 + 交付验证 | 断言脚本 | 7,8 |
| 10 | 一键脚本固化 | `scripts/integration-env.sh`（up/down/status/verify） | 全部 |

---

## 任务 1：服务器准备

- [ ] 安装：`docker`（daemon 可用）、`kind`、`kubectl`、`go`（≥1.25）、`aws` CLI、`jq`、`mc`
- [ ] sysctl：`fs.inotify.max_user_instances=8192`（对齐 test/e2e/env/manager.go 检查项）
- [ ] 构建二进制（供 `kind load docker-image`）：
  - `cmd/controller`、`cmd/fastlet`、`cmd/firecracker-runtime-agent`、`cmd/fastlet-proxy`、`cmd/janitor`
  - 产出：本地镜像 tag（如 `fastlet:dev`、`controller:dev`、`firecracker-runtime-agent:dev`）
- [ ] 记录宿主环境事实：Docker 桥 IP（`172.17.0.1`？）、Kind 网络（`kind get kubeconfig` 的 server IP）——任务 3/7 的网络通路依赖

**验收**：`docker info`、`kind --version`、`go version` 正常。

## 任务 2：Kind 集群（KVM 透传）

- [ ] 新增 `config/dev/kind-firecracker.yaml`（基于 test/e2e/manifests/kind/kata.yaml 精简）：
  - extraMounts：`/dev/kvm`、`/sys/devices/virtual/misc/kvm`、`/dev/net/tun`、
    `/dev/vhost-vsock`、`/sys/devices/virtual/misc/vhost-vsock`、`/dev/shm`
- [ ] `kind create cluster --config ...`；`kubectl` context 可用
- [ ] 验证节点容器内 KVM：`kubectl exec` 或节点内 `ls /dev/kvm` + `grep -c vmx /proc/cpuinfo`
- [ ] 节点 label：`fast-sandbox.io/kvm-node=true`（builder Job 调度）、
  `fast-sandbox.io/firecracker-node=true`（fastlet 亲和）

**验收**：节点容器内 `/dev/kvm` 可打开（可用 firecracker 二进制试 `--version`）。

## 任务 3：MinIO + 凭据

- [ ] 宿主 Docker 启动 MinIO（`-p 9000:9000`），自定义 AK/SK
- [ ] bucket：`sandbox-images`（mc mb + stat 确认）
- [ ] **发布凭据**（SandboxTemplate publishSecretRef 用）：
  `{accessKeyId, secretAccessKey, endpoint}`——endpoint 为 Kind 节点容器可
  达的宿主 MinIO 地址（**实测**：Docker 桥 IP vs kind 网络 vs
  host.docker.internal，记录结论）
- [ ] **拉取凭据**（registryconfig 用）：`{host: <endpoint>, username, password}`，
  写入 `registry.json`（挂载给 fastlet/agent）
- [ ] 通路验证：Kind 节点容器内 `curl <endpoint>/minio/health/live`

**验收**：节点容器内可访问 MinIO；凭据文件格式正确（可 `jq -e .`）。

## 任务 4：CRD + controller

- [ ] `kubectl apply -k config/crd`（SandboxPool/SandboxTemplate/Sandbox CRD）
- [ ] `kubectl apply -f config/all-in-one`（controller + service + PDB）
- [ ] controller 镜像：`kind load docker-image controller:dev`（或 all-in-one
  引用本地镜像 tag）
- [ ] `kubectl rollout status deploy/controller`；日志无 CRD 报错

**验收**：`kubectl get crd | grep sandbox.fast.io` 三个 CRD 就绪。

## 任务 5：firecracker 节点资产 + runtime 环境

- [ ] 新增 `config/runtime-installers/firecracker.yaml`（DaemonSet/init 容器
  把 firecracker 二进制 + jailer 拷贝到节点 hostPath，如
  `/opt/fast-sandbox/firecracker/{firecracker,jailer}`；v1.16.1 版本固定）
  - 二进制来源：firecracker release tarball（含 jailer）
- [ ] `config/runtime-environments.yaml` 新增 firecracker 条目：
  - containerd socket：Kind 节点容器 `/run/containerd/containerd.sock`
  - namespace `k8s.io`；firecracker/jailer 路径；StateRoot
    `/var/lib/fast-sandbox/firecracker`
- [ ] 部署 installer 并验证节点内二进制可执行（`firecracker --version`、
  `jailer --version`）

**验收**：节点容器内 `firecracker`/`jailer` 可运行；runtime plan 可解析。

## 任务 6：agent DaemonSet

- [ ] 新增 `config/dev/agent-daemonset.yaml`：
  - hostPath：UDS socket 目录（`/run/fast-sandbox/firecracker`）、StateRoot、
    `registry.json`
  - env：`FAST_SANDBOX_RUNTIME_AGENT_SOCKET`、`FAST_SANDBOX_ARTIFACT_STORE=
    s3://sandbox-images/publish`、`FAST_SANDBOX_STATE_ROOT`、
    `FAST_SANDBOX_REGISTRY_CONFIG_PATH`
  - 镜像：`firecracker-runtime-agent:dev`（kind load）
- [ ] apply 后验证：socket 文件出现、`curl --unix-socket ... /v1/health`
  返回 `ok:true`

**验收**：agent 健康；UDS socket 权限 0660 属组 fast-sandbox。

## 任务 7：SandboxTemplate（模板制作）

- [ ] 新增样例 `config/samples/sandboxtemplate-firecracker.yaml`：
  `image=registry.example.com/sandbox:v1`、`execd=opensandbox/execd:1.1.0`、
  `kernel=vmlinux.bin`、`machine={1,512Mi}`、`init=/usr/local/sbin/sandbox-init`、
  `readiness={warmupSeconds:15}`、`output={rootfsSize:"2Gi", format:native,
  publish:s3://sandbox-images/publish, publishSecretRef}`
- [ ] 发布凭据 Secret（任务 3 的凭据，imagePullSecrets 风格）
- [ ] apply 后等 `status.phase=Succeeded`（builder Job 自动获得 /dev/kvm+
  /dev/net/tun，节点需 label）
- [ ] **产物验证**（复用 chain-e2e 的断言逻辑）：
  - `index/<sha256(image)>.json` 存在，`manifestRef` 指向 digest16 构建
  - `digest16/` 下 rootfs.ext4/vmstate.snap/memory.snap/SHA256SUMS/manifest.json
  - manifest.files sha256 与本地文件一致；manifest 含 `machine` 与
    `guestNetwork.ip`（=172.30.0.3 约定）
- [ ] 记录构建日志（boot/restore console）留档

**验收**：模板 Succeeded + MinIO 产物布局完整（与 pull 层解析一致）。

## 任务 8：SandboxPool（firecracker）

- [ ] 新增 `config/samples/pool-firecracker.yaml`：
  - `runtime: firecracker`、`sandboxResources={1,512Mi}`
  - `warmImages: ["registry.example.com/sandbox:v1"]`
  - fastletTemplate：privileged、hostPath（/dev/kvm、containerd.sock、
    agent socket、registry.json、runtime plan）、nodeSelector
    （firecracker-node）、镜像 fastlet:dev
  - `FAST_SANDBOX_RUNTIME_AGENT_SOCKET` env 注入（agent 接线）
- [ ] apply 后：fastlet pod Running；**warmImages 状态 Ready**（agent
  PinImage 完成，缓存落盘——任务 6+7 的闭环证明）

**验收**：fastlet Running + warmImages Ready（`kubectl get pool -o yaml`
  status 检查）。

## 任务 9：Sandbox 创建 + 交付验证

- [ ] 新增 `config/samples/sandbox-firecracker.yaml`（Sandbox CR，引用 pool）
- [ ] `kubectl apply` 后断言：
  - `Sandbox Phase=Running`（`kubectl get sandbox -o wide`）
  - fastlet 日志：`firecracker sandbox created`（restore 阶段耗时）
  - **execd 交付**：`curl http://<slot.IP>:44772/ping` → 200
    （从 fastlet pod / 节点容器网络可达处验证）
- [ ] **第二个 sandbox**：clone 网络验证——per-instance slot.IP 可达
  （共享快照 + per-clone netns，issue #26 语义的集成证明）
- [ ] 删除 sandbox：agent 引用归零（ListLeases 空）、jail 目录清理
- [ ] 失败排查手册：记录常见问题（镜像未 load、KVM 未透传、MinIO
  通路、凭据格式）

**验收**：两个 sandbox 各自 execd /ping 可达；删除后资源清理干净。

## 任务 10：一键脚本固化

- [ ] 新增 `scripts/integration-env.sh`（对齐 chain-e2e.sh 风格）：
  - `up`：任务 1-9 全自动（含 kind load 镜像、凭据生成、产物验证）
  - `down`：`kind delete cluster` + MinIO 容器清理（宿主无残留）
  - `status`：组件健康 + 模板状态 + warmImages 状态
  - `verify`：任务 9 的断言链
- [ ] 脚本幂等（重复 `up` 不破坏）；`--cleanup` 支持中断恢复
- [ ] 环境事实参数化：MinIO 端口、凭据、镜像 tag、节点 label（env
  覆盖，同 chain-e2e 风格）

**验收**：`up` → `verify` 全绿；`down` 后宿主干净；二次 `up` 成功。

---

## 日志收集要求（重要，贯穿全部任务）

> 集成环境的调试价值取决于日志完整性——每步失败都要能从日志还原现场。

- [ ] **统一日志目录**：`$WORK/logs/`（默认 `.integration-env/logs/`），
  按组件分文件：
  ```
  logs/
  ├── kind-create.log / kind-delete.log
  ├── minio.log（docker logs 落盘）
  ├── controller.log（kubectl logs -f 落盘）
  ├── agent.log（DaemonSet pod 日志，启动即收集）
  ├── fastlet.log（pod 日志，含 firecracker 启动/restore 阶段输出）
  ├── builder-job.log（builder Job pod 日志 + boot/restore console 尾部）
  ├── sandbox-create.log / sandbox-delete.log（断言链原始输出）
  ├── verify.log（execd /ping、clone 验证原始输出）
  └── environment.txt（版本快照：kind/go/firecracker/jailer/minio 版本、
      kind kubeconfig server IP、Docker 桥 IP、镜像 tag 列表）
  ```
- [ ] **失败即落盘**：任何任务失败时，脚本必须把相关组件日志
  （`kubectl logs`、`docker logs`、`mc ls`、节点容器 `dmesg`/`ls /dev/kvm`）
  追加到 `logs/failure-<task>-<ts>.txt` 后再退出——不得静默失败；
- [ ] 关键日志采集点（对齐 chain-e2e 的既有实践）：
  - builder Job：`boot.console.log`/`restore.console.log` 尾部（KVM 失败
    的现场在串口日志）；
  - fastlet：`firecracker sandbox created`（restore 阶段耗时）、
    firecracker 进程日志；
  - agent：PinImage/LeaseDevices 的 RPC 与拉取错误；
  - MinIO：`docker logs`（请求路径可见，用于回源/幂等判断）；
- [ ] 环境版本快照（`environment.txt`）在 `up` 开头生成；
- [ ] `status`/`verify` 也写日志（不只 stdout）。

## 资源回收要求（重要，贯穿全部任务）

> `down` 之后宿主必须干净，二次 `up` 不因残留失败。

- [ ] **`down` 的回收清单**（逐项执行 + 每项断言）：
  - [ ] `kind delete cluster`（确认集群不存在：`kind get clusters` 空）
  - [ ] MinIO 容器 `docker rm -f`（确认：`docker ps` 无残留）
  - [ ] 临时凭据/Secret 文件删除（`$WORK` 下凭据、registry.json——如保留
    需 0600 并说明）
  - [ ] 宿主 sysctl 恢复（`fs.inotify.max_user_instances` 记录原值并还原）
  - [ ] 节点容器残留：`docker ps -a` 无 kind 容器；kind 网络删除
    （`docker network ls` 无 kind 残留）
  - [ ] 防火墙/iptables 残留：集成环境不改宿主 iptables（Kind/容器内
    隔离），若发现改动需还原（记录）
  - [ ] `$WORK` 大文件：日志保留、缓存/产物目录可删（`down --purge`）
- [ ] **中断恢复**：`--cleanup` 模式执行与 `down` 相同的回收（对齐
  chain-e2e 的 `--cleanup` 实践）；
- [ ] **失败路径也回收**：`up` 中任一步失败 → 默认不自动 `down`（保留
  现场供调试），但脚本提示 `integration-env.sh down` 命令；`--auto-clean`
  选项可自动回收；
- [ ] 二次 `up` 前置自检：检测残留（kind 集群存在/端口占用/MinIO 容器
  在跑）并明确提示或自动清理；
- [ ] 回收断言：`down` 结束时打印"宿主无残留"清单核对（集群/容器/
  端口/文件）。

---

## 交付物汇总

| 产出 | 路径 |
|------|------|
| Kind 配置 | `config/dev/kind-firecracker.yaml` |
| firecracker installer | `config/runtime-installers/firecracker.yaml` |
| runtime 环境条目 | `config/runtime-environments.yaml`（+firecracker） |
| agent DaemonSet | `config/dev/agent-daemonset.yaml` |
| 样例 CR | `config/samples/{sandboxtemplate-firecracker,pool-firecracker,sandbox-firecracker}.yaml` |
| 一键脚本 | `scripts/integration-env.sh` |
| 环境指南（结果记录） | `docs/guides/firecracker-integration-environment.md` 更新实测值 |

## 验收标准（整体）

1. 任务 1-10 勾选完成，每项验收通过；
2. 全链路：SandboxTemplate Succeeded → warmImages Ready → Sandbox
   Running → execd /ping 可达 → 第二个 sandbox clone 网络可达 →
   删除清理无残留；
3. `integration-env.sh up/status/verify` 一键全绿；
4. **日志完整**：`$WORK/logs/` 覆盖全部组件（含失败现场与版本快照），
   任一步失败可从日志还原现场；
5. **资源回收**：`down` 后宿主无残留（集群/容器/端口/文件逐项核对），
   `--cleanup` 可处理中断残留，二次 `up` 成功（Kind 可重制性证明）；
6. 方案文档的待实测项（MinIO 通路地址、installer 方式）记录结论。

## 参考

- 方案：`docs/guides/firecracker-integration-environment.md`
- 复用：`test/e2e/manifests/kind/kata.yaml`（KVM 透传）、
  `config/all-in-one`、`config/runtime-environments.yaml`、
  `scripts/firecracker-chain-e2e.sh`（builder/MinIO/断言逻辑）、
  `internal/controlplane/reconciler/sandboxtemplate_controller.go`
  （builder Job 的 KVM 透传已实现，435-521 行）
