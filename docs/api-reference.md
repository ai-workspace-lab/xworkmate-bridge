# API Interface Reference

本文档是 `xworkmate-bridge` 当前运行入口与对外协议契约的唯一真相来源，按实际代码整理。

覆盖范围：

- `xworkmate-go-core serve` 暴露的 bridge HTTP / WebSocket API
- `xworkmate-go-core acp-stdio` 暴露的 stdio ACP bridge
- `xworkmate-go-core gemini-acp-adapter` 暴露的 Gemini adapter HTTP / WebSocket API
- `xworkmate-go-core hermes-acp-adapter` 暴露的 Hermes adapter HTTP / WebSocket API
- `main.go` 默认模式下 `toolbridge.Run` 暴露的本地工具桥协议

不覆盖：

- 仅存在于内部包、但未作为外部入口挂载的 helper
- 作为 bridge 内部实现细节的 provider-specific upstream URL 契约

## 1. Runtime Entry Points

二进制入口在 [main.go](../main.go) 中定义。

| 模式 | 命令 | 作用 |
| --- | --- | --- |
| Bridge HTTP / WS | `./build/bin/xworkmate-go-core serve` | 启动 canonical bridge，对外暴露 `/acp/rpc` 与 `/acp`，并附带健康检查。 |
| ACP stdio | `./build/bin/xworkmate-go-core acp-stdio` | 通过 stdin/stdout 提供 ACP bridge。 |
| Gemini adapter | `./build/bin/xworkmate-go-core gemini-acp-adapter` | 把 Gemini CLI / Gemini ACP stdio 封装成 HTTP / WebSocket ACP 服务。 |
| Hermes adapter | `./build/bin/xworkmate-go-core hermes-acp-adapter` | 把 Hermes stdio ACP 封装成 HTTP / WebSocket ACP 服务。 |
| Tool bridge | `./build/bin/xworkmate-go-core` | 默认模式；暴露本地工具桥协议，不挂 bridge HTTP 路由。 |

## 2. Bridge `serve` Mode

### 2.1 Canonical origin 与 listen 地址

- APP-facing canonical public origin: `https://xworkmate-bridge.svc.plus`
- 默认 listen 地址：`127.0.0.1:8787`
- 读取环境变量：`ACP_LISTEN_ADDR`

### 2.2 实际挂载路由

`serve` 模式当前由 `internal/acp.Server.Handler()` 实际挂载以下路径：

| Path | Method / Protocol | Auth | 用途 |
| --- | --- | --- | --- |
| `/` | `GET` | 否 | 纯文本运行状态探针，返回 `xworkmate-bridge is running`。 |
| `/api/ping` | `GET` | 否 | 返回镜像、tag、commit、version 等版本信息。 |
| `/bridge/bootstrap/health` | `GET` | 否 | bootstrap 健康检查，返回 `bridgeOrigin`。 |
| `/acp/rpc` | `POST` | 是 | JSON-RPC over HTTP 主入口。 |
| `/acp/rpc` | `OPTIONS` | 否 | CORS 预检。 |
| `/acp` | `GET` + WebSocket upgrade | 是 | JSON-RPC over WebSocket 主入口。 |

其他路径返回 `404 Not Found`。

### 2.3 Auth、Origin 与 CORS

bridge 的认证与跨域规则来自：

- `BRIDGE_AUTH_TOKEN`
- `ACP_ALLOWED_ORIGINS`

默认 allowed origins：

- `https://xworkmate.svc.plus`
- `http://localhost:*`
- `http://127.0.0.1:*`

规则：

- `/acp/rpc` 与 `/acp` 都要求 origin 通过 allowlist 校验。
- 空 `Origin` 默认允许。
- `:*` 表示前缀匹配，例如 `http://localhost:*`。
- auth 使用 bearer header。
- 如果 `BRIDGE_AUTH_TOKEN` 为空，则 bridge auth 默认放行。
- 如果 `BRIDGE_AUTH_TOKEN` 非空，则必须匹配裸 token 或 `Bearer <token>`。

错误行为：

- origin 不允许：HTTP `403`，JSON-RPC error code `-32003`
- bearer 缺失或不合法：HTTP `401`，JSON-RPC error code `-32001`
- 非 `POST` 请求打到 `/acp/rpc`：HTTP `405`，JSON-RPC error code `-32600`

### 2.4 JSON-RPC envelope

HTTP 与 WebSocket 统一使用 **JSON-RPC 2.0** 作为默认通信协议。为了兼容旧版 XWorkmate APP，所有的响应报文均采用混合模式（Hybrid Mode），同时包含 JSON-RPC 2.0 字段与 legacy 扩展字段：

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
    "success": true,
    "output": "..."
  },
  "ok": true,
  "type": "res",
  "payload": {
    "success": true,
    "output": "..."
  },
  "seq": 0
}
```

错误响应：

```json
{
  "jsonrpc": "2.0",
  "id": "req-1",
  "error": {
    "code": -32601,
    "message": "unknown method: ..."
  },
  "ok": false,
  "type": "res",
  "payload": null,
  "seq": 0
}
```



### 2.5 HTTP streaming

当 `/acp/rpc` 的 `Accept` 包含 `text/event-stream` 时：

- 中间通知通过 SSE `data: ...` 推送
- 最终结果也通过 SSE 发出
- 非流式场景则返回普通 `application/json`

## 3. Bridge HTTP Route Details

### 3.1 `GET /`

- Auth：不需要
- 返回：`text/plain; charset=utf-8`
- 内容：`xworkmate-bridge is running`

### 3.2 `GET /api/ping`

- Auth：不需要
- 返回：`application/json`
- 返回结构：

```json
{
  "status": "ok",
  "image": "full-image-ref-or-empty",
  "tag": "tag-or-empty",
  "commit": "commit-or-empty",
  "version": "version-or-empty"
}
```

说明：

- 版本信息来自环境变量 `IMAGE`
- `IMAGE` 会被解析成 `image` / `tag` / `commit` / `version`

### 3.3 `GET /bridge/bootstrap/health`

- Auth：不需要
- 返回：`application/json`
- 返回结构：

```json
{
  "ok": true,
  "bridgeOrigin": "https://xworkmate-bridge.svc.plus",
  "issuedBy": "xworkmate-bridge"
}
```

说明：

- `bridgeOrigin` 反映 bootstrap 阶段的 bridge metadata
- 默认值：`https://xworkmate-bridge.svc.plus`

## 4. Bridge JSON-RPC Methods

未知方法统一返回：

- JSON-RPC error code `-32601`
- message 形如 `unknown method: <method>`

### 4.1 `acp.capabilities`

用途：返回 bridge 当前可用能力、provider catalog、gateway provider catalog 和 APP-facing execution target 列表。

必填参数：无

可选参数：无

返回结构：

```json
{
  "singleAgent": true,
  "multiAgent": true,
  "availableExecutionTargets": ["agent", "gateway"],
  "providerCatalog": [
    { "providerId": "codex", "label": "Codex", "targets": ["agent"] },
    { "providerId": "opencode", "label": "OpenCode", "targets": ["agent"] },
    { "providerId": "gemini", "label": "Gemini", "targets": ["agent"] }
  ],
  "gatewayProviders": [
    {
      "providerId": "openclaw",
      "label": "OpenClaw",
      "targets": ["gateway"],
      "providerDisplay": { "logoEmoji": "🦞" }
    }
  ],
  "capabilities": {
    "single_agent": true,
    "multi_agent": true,
    "availableExecutionTargets": ["agent", "gateway"],
    "providerCatalog": [...],
    "gatewayProviders": [...]
  }
}
```

失败条件：

- 一般不返回 RPC error；只要请求合法即给出当前 bridge 视角能力。

### 4.2 `session.start`

用途：开始一个新任务 session；如果相同 `sessionId` 已存在，会 reset 该 session。

必填参数：

- `sessionId: string`
- `routing: object`

常用可选参数：

- `threadId: string`
- `taskPrompt: string`
- `workingDirectory: string`
- `attachments: []`
- `aiGatewayBaseUrl: string`
- `aiGatewayApiKey: string`

`routing` 常见字段：

- `routingMode`
- `preferredGatewayProviderId`
- `explicitExecutionTarget`
- `explicitProviderId`
- `explicitModel`
- `explicitSkills`
- `allowSkillInstall`
- `installApproval`
- `availableSkills`

返回结构：

- 所有成功结果都会被 bridge 补齐 routing 元数据：
  - `resolvedExecutionTarget`
  - `resolvedProviderId`
  - `resolvedGatewayProviderId`
  - `resolvedModel`
  - `resolvedSkills`
- single-agent 成功时通常包含：
  - `success: true`
  - `turnId: string`
  - `mode: "single-agent"`
  - `provider: string`
  - 可能包含 `effectiveWorkingDirectory`、`output`、`workspace`、artifact metadata
- multi-agent 成功时通常包含：
  - `success: true`
  - `summary: string`
  - `finalScore: number`
  - `iterations: number`
  - `turnId: string`
  - `mode: "multi-agent"`
- gateway 成功时至少保证：
  - `turnId`
  - `mode: "gateway-chat"`
  - 其余字段取决于 gateway payload

失败条件：

- 缺少 `sessionId`：`-32602`
- 缺少 `routing`：`-32602`，message 为 `ROUTING_REQUIRED`
- routing 标记 unavailable：返回 `success: false` 与 `unavailable*` 字段
- single-agent provider 未被 bridge 广告：返回 `success: false`
- multi-agent 缺少 `aiGatewayApiKey`：返回 `success: false`

通知：

- 执行期间会通过 `session.update` 推送状态与步骤更新

### 4.3 `session.message`

用途：继续已有 session 的下一轮消息。

必填参数：

- `sessionId: string`
- `routing: object`

可选参数：

- 与 `session.start` 基本一致

返回结构：

- 与 `session.start` 相同；bridge 会复用 session 历史（历史记录会以 `USER: ` 和 `ASSISTANT: ` 前缀进行上下文包裹），尤其在 multi-agent 与 Gemini adapter 路径中。

失败条件：

- 缺少 `sessionId`：`-32602`
- 缺少 `routing`：`-32602`

### 4.4 `session.cancel`

用途：取消活跃 session。

必填参数：

- `sessionId: string`

返回结构：

```json
{
  "accepted": true,
  "cancelled": true
}
```

失败条件：

- 缺少 `sessionId`：`-32602`

### 4.5 `session.close`

用途：关闭 session 状态。

必填参数：

- `sessionId: string`

返回结构：

```json
{
  "accepted": true,
  "closed": true
}
```

失败条件：

- 缺少 `sessionId`：`-32602`

### 4.6 `xworkmate.dispatch.resolve`

用途：根据候选 providers、required capabilities 和 node state 选择 provider，并返回 dispatch metadata。

必填参数：

- 无强制必填；空输入也会返回结构化结果

常用参数：

- `providers: []`
- `preferredProviderId: string`
- `requiredCapabilities: []string`
- `nodeState: object`
- `nodeInfo: object`

返回结构：

```json
{
  "providerId": "codex",
  "provider": {
    "id": "codex",
    "name": "Codex",
    "defaultArgs": [],
    "capabilities": []
  },
  "agentId": "optional-agent-id",
  "metadata": {
    "node": {...},
    "dispatch": {...},
    "bridge": {...},
    "provider": {...}
  }
}
```

失败条件：

- 不走 RPC error；provider 选不到时 `providerId` / `provider` 可能为空。

### 4.7 `xworkmate.routing.resolve`

用途：只做 routing resolve，不执行任务。

必填参数：

- `routing: object`

常用参数：

- `taskPrompt`
- `workingDirectory`
- `aiGatewayBaseUrl`
- `aiGatewayApiKey`

返回结构：

```json
{
  "ok": true,
  "resolvedExecutionTarget": "single-agent",
  "resolvedProviderId": "codex",
  "resolvedGatewayProviderId": "",
  "resolvedModel": "",
  "resolvedSkills": [],
  "skillResolutionSource": "local_match",
  "needsSkillInstall": false,
  "unavailable": false
}
```

失败条件：

- 若未提供 `routing`，bridge 层不会对该方法报错，但返回的 resolved 结果可能为空。
- unavailable 场景通过 `unavailable` / `unavailableCode` / `unavailableMessage` 表达。

### 4.8 `xworkmate.provider.probe`

用途：通过 bridge 对外探测某个 advertised provider 的 `acp.capabilities`。

必填参数：

- `providerId: string`

返回结构：

成功：

```json
{
  "success": true,
  "providerId": "codex",
  "probeMethod": "acp.capabilities",
  "capabilities": {}
}
```

provider 未广告：

```json
{
  "success": false,
  "providerId": "codex",
  "error": "provider is not advertised by the bridge"
}
```

失败条件：

- 缺少 `providerId`：`-32602`
- upstream probe 失败：返回 `success: false` 与 error 文本

### 4.9 `xworkmate.mounts.reconcile`

用途：对 Codex / Claude / Gemini / OpenCode / OpenClaw / ARIS 的 mount 与 MCP 状态做统一 reconcile。

必填参数：无

常用参数：

- `config`
- `aiGatewayUrl`
- `configuredCodexCliPath`
- `codexHome`
- `opencodeHome`
- `openClawHome`
- `aris`

返回结构：

```json
{
  "mountTargets": [
    {
      "targetId": "codex",
      "label": "Codex",
      "available": true,
      "supportsSkills": true,
      "supportsMcp": true,
      "supportsAiGatewayInjection": true,
      "discoveryState": "ready",
      "syncState": "ready",
      "discoveredSkillCount": 0,
      "discoveredMcpCount": 2,
      "managedMcpCount": 1,
      "detail": "..."
    }
  ],
  "arisBundleVersion": "optional",
  "arisCompatStatus": "idle"
}
```

失败条件：

- 不通过 RPC error 暴露；目标缺失或配置异常通过每个 mount target 的 `available` / `discoveryState` / `syncState` / `detail` 体现。

### 4.10 `xworkmate.gateway.connect`

用途：把 bridge runtime 连接到 gateway provider。

必填参数：

- `runtimeId: string`

常用参数：

- `gatewayProviderId: string`
- `clientId`
- `locale`
- `userAgent`
- `connectAuthMode`
- `connectAuthFields`
- `connectAuthSources`
- `hasSharedAuth`
- `hasDeviceToken`
- `endpoint`
- `packageInfo`
- `deviceInfo`
- `identity`
- `auth`

返回结构：

```json
{
  "ok": true,
  "snapshot": {},
  "auth": {},
  "returnedDeviceToken": "optional",
  "error": {}
}
```

说明：

- 当 `gatewayProviderId` 为空时默认使用 `openclaw`
- 对 `openclaw` 会应用 bridge-owned production routing

失败条件：

- 缺少 `runtimeId`：通过 result 中 `ok: false` 与 `error.code = "INVALID_RUNTIME_ID"` 表达
- 建连失败：`ok: false`，错误信息写入 `error`

### 4.11 `xworkmate.gateway.request`

用途：向已连接的 gateway runtime 发送请求。

必填参数：

- `runtimeId: string`
- `method: string`

可选参数：

- `params: object`
- `timeoutMs: number`

返回结构：

```json
{
  "ok": true,
  "payload": {},
  "error": {}
}
```

失败条件：

- runtime 不在线：`ok: false`，`error.code = "OFFLINE"`
- request timeout：`ok: false`，错误消息包含 timeout

### 4.12 `xworkmate.gateway.disconnect`

用途：断开 gateway runtime。

必填参数：

- `runtimeId: string`

返回结构：

```json
{
  "accepted": true
}
```

失败条件：

- 无 RPC error；如果 runtime 不存在，视为幂等接受。

## 5. Bridge Notifications

### 5.1 `session.update`

bridge 在 session 执行期间会通过 `session.update` 推送通知，统一 envelope：

```json
{
  "jsonrpc": "2.0",
  "method": "session.update",
  "params": {
    "sessionId": "s1",
    "threadId": "t1",
    "turnId": "turn-...",
    "seq": 1
  }
}
```

常见 payload 字段：

- `type`
- `event`
- `message`
- `pending`
- `error`
- `mode`
- `title`
- `role`
- `iteration`
- `score`

## 6. ACP stdio Mode

`acp-stdio` 模式使用 `internal/acp.RunStdio`，通过 stdin/stdout 提供 ACP bridge。

特点：

- 不挂 HTTP 路由
- 不处理 CORS / origin
- 协议方法族与 bridge 主控面一致，仍由 `internal/acp` 管理 session / routing / execution

## 7. Gemini Adapter

### 7.1 Listen 与路由

- 默认 listen：`127.0.0.1:8791`
- 环境变量：`GEMINI_ADAPTER_LISTEN_ADDR`

实际挂载路由：

| Path | Method / Protocol | Auth | 用途 |
| --- | --- | --- | --- |
| `/acp/rpc` | `POST` | 取决于 `GEMINI_ADAPTER_AUTH_TOKEN` | Gemini adapter JSON-RPC HTTP 入口 |
| `/acp/rpc` | `OPTIONS` | 否 | CORS 预检 |
| `/acp` | `GET` + WebSocket upgrade | 取决于 `GEMINI_ADAPTER_AUTH_TOKEN` | Gemini adapter WebSocket 入口 |

### 7.2 Auth 与 origin

相关环境变量：

- `GEMINI_ADAPTER_AUTH_TOKEN`
- `GEMINI_ADAPTER_ALLOWED_ORIGINS`

规则：

- 如果 `GEMINI_ADAPTER_AUTH_TOKEN` 为空，则 adapter 默认不强制 auth
- 如果配置了 token，则要求 bearer 校验通过
- allowed origins 默认与 bridge 类似：`https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*`

### 7.3 Gemini adapter methods

支持的方法：

- `acp.capabilities`
- `session.start`
- `session.message`
- `session.cancel`
- `session.close`
- `gemini.initialize`
- `gemini.raw`

未知方法不会返回 RPC error，而是返回：

```json
{
  "success": false,
  "error": "unsupported method: ..."
}
```

#### `acp.capabilities`

用途：返回 adapter 自身 provider 信息以及 Gemini 上游 initialize 结果的摘要。

返回结构：

- `singleAgent: true`
- `multiAgent: false`
- `providers: [providerId]`
- `capabilities.single_agent = true`
- `provider.id` / `provider.label`
- `upstream` 字段包含 Gemini initialize 信息摘要

#### `session.start` / `session.message`

用途：向 Gemini ACP upstream 转发，或在兼容模式下回退到本地 prompt runner。

常用参数：

- `sessionId`
- `taskPrompt`
- `model`
- `workingDirectory`

返回结构：

- 成功时为 Gemini upstream result 或兼容模式汇总结果
- adapter 会维护 `adapterSession.history`

#### `session.cancel`

返回固定结构：

```json
{
  "accepted": true,
  "cancelled": false
}
```

#### `session.close`

必填参数：

- `sessionId`

返回：

```json
{
  "accepted": true,
  "closed": true
}
```

#### `gemini.initialize`

用途：显式暴露 Gemini upstream initialize 结果。

#### `gemini.raw`

用途：直接向 Gemini upstream 调方法。

常用参数：

- `method`
- `params`

如果缺少 `method`，返回：

```json
{
  "success": false,
  "error": "method is required"
}
```

## 8. Tool Bridge Default Mode

默认模式下 `toolbridge.Run` 通过 stdin/stdout 暴露本地工具桥协议。

### 8.1 协议特点

- 支持换行分隔 JSON
- 也支持 `Content-Length` frame
- 无 HTTP / WebSocket 外壳

### 8.2 支持的方法

- `initialize`
- `ping`
- `tools/list`
- `tools/call`

### 8.3 `tools/list`

当前固定暴露三个工具：

- `chat`
- `claude_review`
- `vault_kv`

### 8.4 `tools/call`

参数结构：

```json
{
  "name": "chat",
  "arguments": {}
}
```

成功时返回 text tool result；失败时返回 `isError: true` 的 tool result 或 `-32601` / `-32602` error response。

## 9. Environment Variables

### 9.1 Bridge `serve`

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `ACP_LISTEN_ADDR` | `127.0.0.1:8787` | bridge listen 地址 |
| `BRIDGE_AUTH_TOKEN` | 空 | bridge bearer 校验 token |
| `ACP_ALLOWED_ORIGINS` | `https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*` | bridge allowed origins |
| `ACP_MULTI_AGENT_ENABLED` | `true` | `acp.capabilities` 中的 `multiAgent` 开关 |
| `ACP_MULTI_AGENT_MODEL` | `gpt-4o` | multi-agent 默认模型 |
| `BRIDGE_SERVER_URL` | `https://xworkmate-bridge.svc.plus` | bootstrap health 的 metadata 默认值，不作为 app runtime 真源 |
| `INTERNAL_SERVICE_TOKEN` | 空 | upstream provider / gateway 优先使用的内部服务 token |
| `BRIDGE_AUTH_TOKEN` | 空 | bridge 入站 bearer token；当 `INTERNAL_SERVICE_TOKEN` 为空时也作为 upstream forwarding token |
| `IMAGE` | 空 | `/api/ping` 版本信息来源 |

### 9.2 Gemini adapter

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `GEMINI_ADAPTER_LISTEN_ADDR` | `127.0.0.1:8791` | Gemini adapter listen 地址 |
| `GEMINI_ADAPTER_BIN` | 继承 `ACP_GEMINI_BIN`，再回退到 `gemini` | Gemini CLI 二进制 |
| `ACP_GEMINI_BIN` | `gemini` | Gemini CLI fallback binary |
| `GEMINI_ADAPTER_ARGS` | `--experimental-acp` | Gemini CLI 启动参数 |
| `GEMINI_ADAPTER_PROTOCOL_VERSION` | `1` | Gemini initialize 协议版本 |
| `GEMINI_ADAPTER_AUTH_TOKEN` | 空 | Gemini adapter bearer token |
| `GEMINI_ADAPTER_PROVIDER_ID` | `gemini` | adapter provider ID |
| `GEMINI_ADAPTER_PROVIDER_LABEL` | `Gemini` | adapter provider label |
| `GEMINI_ADAPTER_ALLOWED_ORIGINS` | `https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*` | adapter allowed origins |
| `GEMINI_ADAPTER_UPSTREAM_METHOD` | 空 | adapter 上游方法覆盖 |

### 9.3 Shared / provider command / tools

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `ACP_CODEX_BIN` | `codex` | Codex CLI binary |
| `ACP_OPENCODE_BIN` | `opencode` | OpenCode CLI binary |
| `ACP_CLAUDE_BIN` | `claude` | Claude CLI binary |
| `ACP_GEMINI_BIN` | `gemini` | Gemini CLI binary |
| `ACP_FIND_SKILLS_BIN` | 空 | 外部 skills finder 二进制 |
| `ACP_INSTALL_SKILL_BIN` | 空 | 外部 skills installer 二进制 |
| `LLM_API_KEY` | 空 | `chat` 工具使用的 OpenAI-compatible API key |

## 10. 代码定位

- [main.go](../main.go)
- [internal/acp/server.go](../internal/acp/server.go)
- [internal/acp/web_contract.go](../internal/acp/web_contract.go)
- [internal/acp/bootstrap.go](../internal/acp/bootstrap.go)
- [internal/acp/gateway_runtime.go](../internal/acp/gateway_runtime.go)
- [internal/acp/provider_catalog.go](../internal/acp/provider_catalog.go)
- [internal/acp/execution.go](../internal/acp/execution.go)
- [internal/geminiadapter/server.go](../internal/geminiadapter/server.go)
- [internal/toolbridge/runner.go](../internal/toolbridge/runner.go)
