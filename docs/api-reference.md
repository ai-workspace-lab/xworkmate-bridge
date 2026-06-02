# API Interface Reference

本文档定义 `xworkmate-bridge` 当前对 `xworkmate-app` 暴露的稳定 contract。

当前定位：

- `xworkmate-bridge` 是 **APP-facing ACP control plane and provider runtime layer**
- App-facing canonical transport 是 `GET /acp` WebSocket upgrade 后的 JSON-RPC stream
- `POST /acp/rpc` 作为 CI、脚本、调试、HTTP fallback 和 OpenClaw gateway task submit 入口
- `/acp-server/*` 不属于 APP-facing contract，APP 不应保存或拼接这些 provider direct path

## 1. Runtime Entry Points

二进制入口定义在 [main.go](../main.go)。

| 模式 | 命令 | 作用 |
| --- | --- | --- |
| Bridge HTTP / WS | `./build/bin/xworkmate-go-core serve` | 启动 canonical bridge，暴露 `/acp` 与 `/acp/rpc` |
| ACP stdio | `./build/bin/xworkmate-go-core acp-stdio` | 启动 stdio ACP bridge |
| Gemini adapter | `./build/bin/xworkmate-go-core gemini-acp-adapter` | Gemini compatibility/runtime |
| Hermes adapter | `./build/bin/xworkmate-go-core hermes-acp-adapter` | Hermes compatibility/runtime |
| Tool bridge | `./build/bin/xworkmate-go-core` | 本地工具桥，不属于 APP-facing control plane |

## 2. Public HTTP Surface

`serve` 模式对外暴露：

| Path | Method / Protocol | Auth | 用途 |
| --- | --- | --- | --- |
| `/` | `GET` | 否 | 纯文本运行状态 |
| `/api/ping` | `GET` | 是 | 发布版本探针 |
| `/acp` | `GET` + WebSocket upgrade | 是 | App-facing JSON-RPC WebSocket 主入口 |
| `/acp/rpc` | `POST` | 是 | JSON-RPC HTTP fallback / CI / 调试入口 |
| `/acp/rpc` | `OPTIONS` | 否 | CORS preflight |

线上 Caddy 反代 `/api*`、`/acp*`、`/artifacts/*` 和 `/` 到 bridge origin。`/acp-server/*` 显式返回 `404`。

## 3. Auth / Origin

环境变量：

- `BRIDGE_AUTH_TOKEN`
- `BRIDGE_REVIEW_AUTH_TOKEN`（可选）：Apple review / beta 工测专用临时 token。清空该环境变量并重启/reload bridge 即可单独关停，不影响主 token。
- `ACP_ALLOWED_ORIGINS`

规则：

- `/acp` 与 `/acp/rpc` 都做 origin allowlist 校验
- 空 `Origin` 默认允许
- `/api/ping`、`/acp`、`/acp/rpc` 在任一 bridge token 非空时都要求 bearer header
- `BRIDGE_AUTH_TOKEN` 与 `BRIDGE_REVIEW_AUTH_TOKEN` 都为空时默认放行
- token 非空时，接受裸 token 或 `Bearer <token>`
- `xworkmate-app` 生产 Origin 固定为 `https://xworkmate.svc.plus`

## 3.1 Lightweight Distributed Task Forwarding

bridge 可以把本机收到的 HTTP 任务提交转发到另一个 bridge endpoint，用于轻量分布式部署。当前实现是一个静态 task router，不做自动发现，不依赖 config center：

- `nodes` 是静态 peer catalog，记录 bridge 节点身份、角色、能力和私网 endpoint
- `forwarding.rules` 决定哪些 JSON-RPC method 转发到哪个节点或哪类节点
- `forwarding.routes` 决定显式 next-hop，用于星状或网状拓扑
- `session.start` 选中目标后，`session.message` 通过本机 session route store 粘到同一个目标节点
- 公网域名只作为 ingress；bridge 之间的 task forward 只走 WireGuard over VLESS 私网 endpoint

双节点简写配置：

```yaml
distributed:
  topology: "dual-node"
  local_node_id: "cn-xworkmate-bridge"
  task_forward_peer_id: "xworkmate-bridge"
  task_forward_token: ""
```

`task_forward_peer_id: "xworkmate-bridge"` 等价于把 `session.start` / `session.message` 转发到 peer `xworkmate-bridge`。内置 peer catalog 会把它解析为 `http://172.29.10.1:8787`。主 bridge 不配置 `task_forward_peer_id`，因此不会反向转发。

通用多节点配置：

```yaml
distributed:
  local_node_id: "edge-cn"
  nodes:
    - id: "edge-cn"
      role: "edge"
      zone: "cn"
      bridge_endpoint: "http://172.29.10.2:8787"
      capabilities: ["ingress"]
    - id: "worker-a"
      role: "executor"
      zone: "global"
      bridge_endpoint: "http://172.29.10.11:8787"
      capabilities: ["openclaw"]
    - id: "worker-b"
      role: "executor"
      zone: "global"
      bridge_endpoint: "http://172.29.10.12:8787"
      capabilities: ["openclaw"]
  forwarding:
    hop_limit: 3
    default_action: "execute_local"
    rules:
      - methods: ["session.start", "session.message"]
        target:
          selector:
            role: "executor"
            capability: "openclaw"
          strategy: "round_robin"
```

星状或显式 next-hop 网状配置：

```yaml
distributed:
  local_node_id: "edge-cn"
  nodes:
    - id: "edge-cn"
      role: "edge"
      bridge_endpoint: "http://172.29.10.2:8787"
    - id: "hub-main"
      role: "hub"
      bridge_endpoint: "http://172.29.10.1:8787"
    - id: "worker-eu"
      role: "executor"
      bridge_endpoint: "http://172.29.10.30:8787"
  forwarding:
    hop_limit: 3
    rules:
      - methods: ["session.start"]
        target:
          node_id: "worker-eu"
    routes:
      - target_node_id: "worker-eu"
        next_hop_node_id: "hub-main"
```

规则：

- `bridge_endpoint` 是 peer bridge base URL，bridge 会按当前请求路径拼接 `/acp/rpc` 或 `/gateway/openclaw`
- 同步消息不能走公网；`bridge_endpoint` 必须是 loopback、private、link-local 这类本机或 VPN 内网地址，用于 WireGuard over VLESS 等隧道已经提供加密的场景
- 只要求本机网络能路由到 endpoint；bridge 不依赖 config center 或额外注册中心
- `task_forward_token` 为空时复用本机 `BRIDGE_AUTH_TOKEN`
- 转发请求会带 `X-XWorkmate-Bridge-Forwarded: 1`
- `X-XWorkmate-Forward-Source` 是源节点，`X-XWorkmate-Forward-Target` 是最终目标节点
- `X-XWorkmate-Forward-Hop` 逐跳递增，超过 `forwarding.hop_limit` 时拒绝转发，避免循环
- 收到已转发请求时，如果 target 是本机则本机执行；如果 target 不是本机，则按 `forwarding.routes` 查 next-hop

抗干扰建议：

- 跨境或 GFW 环境下，`bridge_endpoint` 应指向 WireGuard over VLESS/TCP/TLS 后的 `http://10.x/172.16-31.x/192.168.x` 私网 bridge endpoint
- 运营商 UDP 阻断时，支持把裸 WireGuard UDP 替换为 WireGuard over VLESS/TCP/TLS、WebSocket/TLS 或等价可靠加密通道，bridge 继续使用同一个 peer catalog 和 forwarding rules
- bridge 层继续使用 bearer token 鉴权；隧道层负责链路加密和抗干扰，应用层负责 peer 身份和任务权限

推荐 APP 配置：

```text
BRIDGE_SERVER_URL=https://xworkmate-bridge.svc.plus
BRIDGE_WS_URL=wss://xworkmate-bridge.svc.plus/acp
BRIDGE_HTTP_RPC_URL=https://xworkmate-bridge.svc.plus/acp/rpc
Authorization: Bearer $BRIDGE_AUTH_TOKEN
Origin: https://xworkmate.svc.plus
```

错误行为：

- origin 不允许：HTTP `403`，JSON-RPC error code `-32003`
- bearer 缺失或不合法：HTTP `401`，JSON-RPC error code `-32001`
- 非 `POST` 请求打到 `/acp/rpc`：HTTP `405`，JSON-RPC error code `-32600`

## 4. JSON-RPC Envelope

WS 与 HTTP 都使用 JSON-RPC 2.0。为兼容现有 APP，bridge 继续输出 hybrid envelope。

请求：

```json
{
  "jsonrpc": "2.0",
  "id": "req-1",
  "method": "acp.capabilities",
  "params": {}
}
```

成功响应：

```json
{
  "jsonrpc": "2.0",
  "id": "req-1",
  "result": {
    "success": true
  },
  "ok": true,
  "type": "res",
  "payload": {
    "success": true
  },
  "seq": 0
}
```

通知：

```json
{
  "jsonrpc": "2.0",
  "method": "session.update",
  "params": {
    "turnId": "turn-1",
    "type": "status",
    "message": "session started"
  },
  "type": "event",
  "event": "session.update",
  "payload": {
    "turnId": "turn-1",
    "type": "status",
    "message": "session started"
  },
  "seq": 0
}
```

## 5. Canonical Method Family

bridge 对 app 的稳定 method family 只有：

- `acp.capabilities`
- `xworkmate.routing.resolve`
- `session.start`
- `session.message`
- `session.cancel`
- `session.close`
- `xworkmate.gateway.connect`
- `xworkmate.gateway.request`
- `xworkmate.gateway.disconnect`
- `xworkmate.jobs.submit`
- `xworkmate.jobs.get`
- `xworkmate.jobs.list`
- `xworkmate.jobs.stats`
- `xworkmate.tools.invoke`

路径约束：

- `/acp/rpc` 是 capabilities、routing、agent、multi-agent、jobs、tools proxy、cancel、close 的 canonical HTTP RPC 入口。
- OpenClaw `session.start` 和 follow-up `session.message` 也通过 `/acp/rpc`，由 `routing.explicitExecutionTarget=gateway` 与 `routing.preferredGatewayProviderId=openclaw` 表达。

## 6. `acp.capabilities`

用途：返回 bridge-owned provider catalog、gatewayProviders、availableExecutionTargets。

返回示例：

```json
{
  "singleAgent": true,
  "multiAgent": true,
  "availableExecutionTargets": ["agent", "gateway"],
  "providerCatalog": [
    { "providerId": "codex", "label": "Codex", "targets": ["agent"], "category": "native" },
    { "providerId": "opencode", "label": "OpenCode", "targets": ["agent"], "category": "native" },
    { "providerId": "gemini", "label": "Gemini", "targets": ["agent"], "category": "protocol-adapter" },
    { "providerId": "hermes", "label": "Hermes", "targets": ["agent"], "category": "protocol-adapter" }
  ],
  "gatewayProviders": [
    { "providerId": "openclaw", "label": "OpenClaw", "targets": ["gateway"] }
  ],
  "providerProbeSummary": [
    { "providerId": "codex", "available": true, "status": "ok" }
  ]
}
```

约束：

- app 只能从这里获取 `providerCatalog`、`gatewayProviders`、`availableExecutionTargets`
- app 不应依赖 bridge 内部 provider URL / 端口 / service 名
- app 不保存 `codex`、`opencode`、`gemini`、`hermes`、`openclaw` 的 URL

## 7. `xworkmate.routing.resolve`

用途：bridge control plane 独立做 routing intent resolve。

输入重点：

- `taskPrompt`
- `workingDirectory`
- `routing.routingMode`
- `routing.explicitExecutionTarget`
- `routing.explicitProviderId`
- `routing.preferredGatewayProviderId`
- `routing.explicitModel`
- `routing.explicitSkills`
- `routing.availableSkills`

输出字段：

- `resolvedExecutionTarget`
- `resolvedProviderId`
- `resolvedGatewayProviderId`
- `resolvedModel`
- `resolvedSkills`
- `status`
- `unavailable`
- `unavailableCode`
- `unavailableMessage`
- `skillResolutionSource`
- `needsSkillInstall`
- `skillInstallRequestId`

available 示例：

```json
{
  "resolvedExecutionTarget": "single-agent",
  "resolvedProviderId": "codex",
  "resolvedGatewayProviderId": "",
  "resolvedModel": "",
  "resolvedSkills": ["PPTX"],
  "status": "available",
  "unavailable": false
}
```

unavailable 示例：

```json
{
  "resolvedExecutionTarget": "single-agent",
  "resolvedProviderId": "",
  "resolvedGatewayProviderId": "",
  "resolvedModel": "",
  "resolvedSkills": [],
  "status": "unavailable",
  "unavailable": true,
  "unavailableCode": "PROVIDER_UNAVAILABLE",
  "unavailableMessage": "explicit provider is unavailable"
}
```

## 8. `session.start` / `session.message`

用途：统一进入 `session_orchestrator`，由 bridge 控制 session state、turnId、history、notification、normalized result。

通用输入：

- `sessionId`
- `threadId`
- `taskPrompt`
- `workingDirectory`
- `routing`

multi-agent 输入仍使用同一个 `session.start` / `session.message` method，不新增 HTTP path：

- `multiAgent: true` 或 `mode: "multi-agent"`
- `routing.orchestrationMode`: `sequence`、`parallel`、`race`、`conversation`
- `routing.steps`: `{ "providerId": "codex", "prompt": "...", "outputAs": "...", "timeoutMs": 300000 }[]`
- `routing.participants`、`routing.maxTurns`、`routing.stopConditions` 用于 `conversation`

multi-agent 只允许通过 `/acp` 或 `/acp/rpc` 进入；OpenClaw gateway 单任务同样使用 `/acp/rpc`，但不能与 `multiAgent=true` 混用。

统一结果字段：

- `success`
- `status`
- `turnId`
- `resolvedExecutionTarget`
- `resolvedProviderId`
- `resolvedGatewayProviderId`
- `resolvedModel`
- `resolvedSkills`
- `output`
- `summary`

bridge 保证：

- provider-specific 差异被 compat layer 吸收
- bridge core 不暴露 stdio/runtime 细节
- 中间通知统一通过 `session.update`
- Native ACP structured event 会透传到 `structuredEvent`，类型包括 `thinking`、`tool_call`、`text`、`status`
- `session.message` 是续写合同；provider compat 缺少原会话状态时返回结构化 JSON-RPC error，不静默降级为新的 `session.start`
- provider 发起 `session/request_permission` 时 bridge 自动返回 allow/approved，避免 ACP provider 卡住等待人工确认

续写失败错误：

```json
{
  "code": -32002,
  "message": "SESSION_CONTINUATION_UNAVAILABLE: provider session state is unavailable",
  "data": {
    "code": "SESSION_CONTINUATION_UNAVAILABLE",
    "sessionId": "<sessionId>",
    "threadId": "<threadId>",
    "providerId": "<providerId>"
  }
}
```

OpenClaw gateway 任务的 HTTP task submit 路径是 `/acp/rpc`。请求必须在 routing 中声明 `explicitExecutionTarget=gateway` 与 `preferredGatewayProviderId=openclaw`；bridge 不再要求或暴露独立的 OpenClaw task URL。

## 9. `session.cancel` / `session.close`

返回：

```json
{ "accepted": true }
```

语义：

- `session.cancel` 调用当前 compat 的 cancel
- `session.close` 调用当前 compat 的 close，并移除 bridge 内部 session state

## 10. `xworkmate.gateway.*`

gateway method family 保留为 control-plane contract：

- `xworkmate.gateway.connect`
- `xworkmate.gateway.request`
- `xworkmate.gateway.disconnect`

对 app 的约束：

- app 调 gateway runtime 时仍然只通过 bridge JSON-RPC methods
- `openclaw` 是 bridge-owned gateway provider，不是 app-facing direct route
- gateway task 和 control-plane method 都走 `/acp` 或 `/acp/rpc`

## 11. Internal Async Jobs

`xworkmate.jobs.*` 只作为 `/acp` / `/acp/rpc` 内部 JSON-RPC method 暴露，不新增 `/jobs` HTTP path。

- `xworkmate.jobs.submit`：提交后台任务，立即返回 `jobId`
- `xworkmate.jobs.get`：按 `jobId` 查询状态和结果
- `xworkmate.jobs.list`：返回 job 列表和 summary
- `xworkmate.jobs.stats`：返回状态统计

`submit` 输入重点：

- `providerId`
- `sessionId`
- `threadId`
- `taskPrompt`
- `workingDirectory`
- `timeoutMs`，默认 10 分钟
- `callbackUrl` / `webhookUrl`
- `target`、`channel`、`accountId`，用于通过 OpenClaw message tool 推送 Markdown card

语义：

- job 复用已有 provider compat/session 映射，不引入第二套 process pool
- 超过 `timeoutMs` 或默认 10 分钟仍未结束时标记 `failed`
- callback webhook 最多重试 3 次
- `target` 非空时内部调用 `xworkmate.tools.invoke`，等价于 acp-bridge 的 OpenClaw `/tools/invoke` message send 能力

## 12. Internal Tools Proxy

`xworkmate.tools.invoke` 是 `/tools/invoke` 的 JSON-RPC 内部等价物。

输入：

```json
{
  "tool": "message",
  "action": "send",
  "args": {
    "channel": "discord",
    "target": "channel:123",
    "message": "..."
  }
}
```

优先使用 `OPENCLAW_TOOLS_INVOKE_URL` / `OPENCLAW_TOOLS_TOKEN` 直连 OpenClaw tools HTTP endpoint；未配置时复用已连接的 OpenClaw gateway runtime 调用 `tools.invoke`。

## 13. 非 Contract 内容

以下内容不是 app contract：

- provider-specific upstream URL
- 本地端口
- systemd service 名
- stdio framing / process lifecycle / stderr / restart 语义
- bridge 内部 compat/runtime 实现细节
- `/acp-server/codex`
- `/acp-server/opencode`
- `/acp-server/gemini`
- `/acp-server/hermes`
