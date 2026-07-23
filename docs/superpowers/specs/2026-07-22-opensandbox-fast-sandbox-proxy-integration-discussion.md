# OpenSandbox 与 Fast Sandbox 双 Proxy 集成议题

**状态**：待讨论，不形成架构决策，不包含本轮代码实现
**日期**：2026-07-22

## 1. 已确定边界

Fast Sandbox Proxy 是独立的平台数据面入口，负责：

- 根据 Sandbox UID、target port 和 assignment fence 定位 Fastlet；
- 校验短期 route credential；
- 覆盖调用方伪造的内部路由 header；
- 透明转发 HTTP、SSE、WebSocket 或后续明确支持的字节流；
- 在 reset、reassignment、delete 后拒绝旧 route generation。

它不负责：

- 解释或转换 OpenSandbox Execd 的 command、file、session、PTY payload；
- 实现一份 Execd OpenAPI；
- 把 OpenSandbox 对象模型翻译成 Fast Sandbox CRD；
- 代替 Sandbox 内的 execd。

因此，OpenSandbox 可以接入 Fast Sandbox，但 Fast Sandbox Proxy 仍保持独立。协议翻译如果确有需要，应位于 OpenSandbox-facing adapter/gateway，而不是透明代理核心。

## 2. 当前基线链路

```text
fastctl opensandbox / official OpenSandbox SDK
  -> FastPath GetSandbox + ResolveEndpoint
  -> Fast Sandbox Proxy
  -> assigned Fastlet Proxy
  -> Sandbox private network :44772
  -> opensandbox-execd
```

Fast Sandbox 只把 endpoint 和必须的 route headers 交给官方 SDK。官方 SDK 拥有 Execd 的 HTTP/SSE/file 语义，两个 Proxy 只处理传输、鉴权和 fencing。

## 3. 后续可讨论的集成形态

### 方案 A：SDK 直连 Fast Sandbox Proxy

OpenSandbox SDK 先通过一个轻量 resolver 获取 Fast Sandbox route，然后直接访问 Execd。

优点：

- 链路最短；
- 不需要协议翻译；
- Fast Sandbox 与 OpenSandbox 版本边界清晰；
- 当前 `fastctl opensandbox` 已验证这种组合方式。

待确认：

- OpenSandbox SDK 是否提供稳定的 base URL/header 注入扩展点；
- credential 刷新与长连接重连如何暴露给 SDK；
- PTY WebSocket 等扩展是否能完整保留 header 和流式语义。

### 方案 B：OpenSandbox-facing Gateway + Fast Sandbox Proxy

增加一个面向 OpenSandbox API/对象模型的 adapter 或 gateway。它负责 OpenSandbox 生命周期与 Fast Sandbox 生命周期的映射，并向下游解析 Fast Sandbox route；数据仍经过 Fast Sandbox Proxy。

优点：

- 可以向 OpenSandbox 用户提供原生产品入口；
- 生命周期协议翻译与透明数据代理明确分层；
- 不污染 FastPath 和 Fast Sandbox Proxy。

代价：

- 多一个控制面/接入层组件；
- 需要定义 OpenSandbox sandbox identity 与 Kubernetes Sandbox CRD identity 的映射；
- 需要独立处理幂等、错误码和 credential 生命周期。

### 方案 C：OpenSandbox Proxy 前置 Fast Sandbox Proxy

如果 OpenSandbox 本身要求一个不可绕过的集中数据代理，可以把它放在 Fast Sandbox Proxy 前面，后者继续负责 Kubernetes assignment 与 Fastlet fencing。

```text
OpenSandbox client -> OpenSandbox Proxy -> Fast Sandbox Proxy -> Fastlet Proxy -> execd
```

该方案只有在 OpenSandbox Proxy 提供不可替代的租户、审计或协议能力时才值得采用。否则双重中心代理会增加一次 hop、连接数、超时配置和故障面。

## 4. 明确不采用的方向

- 在 Fast Sandbox Proxy 中把 Execd 请求翻译成 Fastlet Control RPC；
- 让 Fastlet Control `:5758` 暴露公共 Exec/File API；
- 由 Fast Sandbox 维护 Execd SSE/file/PTY 的兼容实现；
- 因为 OpenSandbox 接入而取消 target-port 路由或 assignment fencing；
- 在没有端到端证据前合并 OpenSandbox Proxy 与 Fast Sandbox Proxy。

## 5. 决策前需要的实证

1. 用官方 OpenSandbox SDK 完成 command SSE、file upload/download、错误流和大文件流式测试；
2. 明确 PTY/session 的实际传输协议及 header 注入限制；
3. 测量单 Proxy 与双中心 Proxy 的首包、吞吐和长连接资源占用；
4. 验证 route credential 过期、reset 和 reassignment 时官方 SDK 的重连行为；
5. 明确 OpenSandbox 是否需要提供生命周期 API，还是只需要 Execd 数据面兼容；
6. 如果需要 Gateway，先定义 identity、幂等键、错误码和所有权边界，再决定部署形态。

## 6. 本轮结论

本轮只保留方案空间：Fast Sandbox Proxy 继续作为独立、协议透明的路由与鉴权组件；OpenSandbox 官方 SDK 可以通过 route hand-off 接入。是否增加 OpenSandbox-facing Gateway 或前置 Proxy，待上述实证完成后单独拍板。
