# API Interface Reference

本文档定义 `xworkmate-bridge` 当前对 `xworkmate-app` 暴露的稳定 contract。

当前定位：

- `xworkmate-bridge` 是 **APP-facing ACP control plane and provider runtime layer**
- App-facing canonical transport 是 `GET /acp` WebSocket upgrade 后的 JSON-RPC stream
- `POST /acp/rpc` 仅作为 CI、脚本、调试和兼容 fallback
- `POST /gateway/openclaw` 仅是 OpenClaw `session.start` / `session.message` task submit 专用入口，不是全局 ACP base endpoint
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
| `/gateway/openclaw` | `POST` | 是 | OpenClaw `session.start` / `session.message` task submit 专用入口 |

线上 Caddy 反代 `/api*`、`/acp*`、`/gateway/openclaw` 和 `/` 到 bridge origin。`/acp-server/*` 显式返回 `404`。

## 3. Auth / Origin

环境变量：

- `BRIDGE_AUTH_TOKEN`
- `ACP_ALLOWED_ORIGINS`

规则：

- `/acp` 与 `/acp/rpc` 都做 origin allowlist 校验
- 空 `Origin` 默认允许
- `/api/ping`、`/acp`、`/acp/rpc` 在 `BRIDGE_AUTH_TOKEN` 非空时都要求 bearer header
- `BRIDGE_AUTH_TOKEN` 为空时默认放行
- `BRIDGE_AUTH_TOKEN` 非空时，接受裸 token 或 `Bearer <token>`
- `xworkmate-app` 生产 Origin 固定为 `https://xworkmate.svc.plus`

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

路径约束：

- `/acp/rpc` 是 capabilities、routing、agent、multi-agent、cancel、close 的 canonical HTTP RPC 入口。
- `/gateway/openclaw` 只允许 OpenClaw `session.start` 和 follow-up `session.message`。
- `/gateway/openclaw` 拒绝 `acp.capabilities`、`xworkmate.routing.resolve`、`xworkmate.gateway.*`、`session.cancel` 和 `session.close`。

## 6. `acp.capabilities`

用途：返回 bridge-owned provider catalog、gatewayProviders、availableExecutionTargets。

返回示例：

```json
{
  "singleAgent": true,
  "multiAgent": false,
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

OpenClaw gateway 任务的 HTTP task submit 路径是 `/gateway/openclaw`，并且只用于 `session.start` 与同一 OpenClaw task 的 follow-up `session.message`。Bridge 会强制 routing 到 `gateway/openclaw`，并拒绝 `multiAgent=true` 或 agent/provider 冲突参数。

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
- gateway control-plane method 仍走 `/acp` 或 `/acp/rpc`，不走 `/gateway/openclaw`

## 11. 非 Contract 内容

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
