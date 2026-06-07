# XWorkmate Bridge Backend API Design

Last verified: 2026-05-03

本文档定义 `xworkmate-bridge` 为 `xworkmate-app` 提供后端服务时的最佳接口设计。当前设计以线上环境为准，并把 WebSocket 作为默认运行时协议。

## 1. 设计目标

`xworkmate-app` 只连接 `xworkmate-bridge`，不保存也不拼接任何 provider 或 gateway 的内部地址。

APP 侧只需要保存：

```text
BRIDGE_SERVER_URL=https://xworkmate-bridge.svc.plus
BRIDGE_WS_URL=wss://xworkmate-bridge.svc.plus/acp
BRIDGE_HTTP_RPC_URL=https://xworkmate-bridge.svc.plus/acp/rpc
BRIDGE_AUTH_TOKEN=<provided by account/profile sync>
```

APP 所有能力发现、路由决策和任务执行都通过 bridge 的 JSON-RPC contract 完成：

```text
xworkmate-app
  -> wss://xworkmate-bridge.svc.plus/acp
  -> acp.capabilities
  -> xworkmate.routing.resolve
  -> session.start / session.message / session.cancel / session.close
  -> bridge 内部路由到 codex / opencode / gemini / hermes / openclaw
```

OpenClaw task submit 也使用 `/acp/rpc`，通过
`routing.explicitExecutionTarget=gateway` 和
`routing.preferredGatewayProviderId=openclaw` 表达 gateway 目标。

## 2. App-Facing Contract

### 2.1 默认传输

默认传输是 JSON-RPC over WebSocket：

```text
wss://xworkmate-bridge.svc.plus/acp
```

WebSocket 握手必须带：

```http
Authorization: Bearer $BRIDGE_AUTH_TOKEN
Origin: https://xworkmate.svc.plus
```

连接建立后，每个消息都是 JSON-RPC 2.0 frame。

### 2.2 HTTP RPC

HTTP JSON-RPC 仅用于 CI、脚本、调试、诊断和兼容 fallback。除 OpenClaw task submit 之外，canonical HTTP RPC 是：

```text
POST https://xworkmate-bridge.svc.plus/acp/rpc
```

请求头同样必须带：

```http
Authorization: Bearer $BRIDGE_AUTH_TOKEN
Origin: https://xworkmate.svc.plus
Content-Type: application/json
```

### 2.3 Health

发布和运行验证使用：

```text
GET https://xworkmate-bridge.svc.plus/api/ping
```

该接口用于验证运行中的 bridge 镜像、tag、commit、version 和状态，不参与 APP 任务路由。

## 3. JSON-RPC 方法

APP-facing 稳定方法只有以下几组。

| Method | 用途 |
| --- | --- |
| `acp.capabilities` | 获取 bridge 当前能力目录 |
| `xworkmate.routing.resolve` | 让 bridge 计算路由，不执行任务 |
| `session.start` | 启动一个任务或首轮对话 |
| `session.message` | 同 session 继续追问 |
| `session.cancel` | 取消当前 session |
| `session.close` | 关闭 session 并释放 bridge 内部状态 |
| `xworkmate.gateway.connect` | gateway runtime 控制面连接 |
| `xworkmate.gateway.request` | gateway runtime 控制面请求 |
| `xworkmate.gateway.disconnect` | gateway runtime 控制面断开 |

APP 不应调用 provider-specific URL。Provider 与 gateway 只能来自 `acp.capabilities` 的返回值。

## 4. 能力发现

请求：

```json
{
  "jsonrpc": "2.0",
  "id": "cap-1",
  "method": "acp.capabilities",
  "params": {}
}
```

线上验证返回的核心结构：

```json
{
  "availableExecutionTargets": ["agent", "gateway"],
  "providerCatalog": [
    { "providerId": "codex", "label": "Codex", "targets": ["agent"], "category": "native" },
    { "providerId": "opencode", "label": "OpenCode", "targets": ["agent"], "category": "native" },
    { "providerId": "gemini", "label": "Gemini", "targets": ["agent"], "category": "protocol-adapter" },
    { "providerId": "hermes", "label": "Hermes", "targets": ["agent"], "category": "protocol-adapter" }
  ],
  "gatewayProviders": [
    { "providerId": "openclaw", "label": "OpenClaw", "targets": ["gateway"] }
  ]
}
```

APP UI 只能用这些字段驱动 provider/gateway 展示：

- `providerCatalog`
- `gatewayProviders`
- `availableExecutionTargets`

不得在 APP 代码里静态保存：

- `codex` URL
- `opencode` URL
- `gemini` URL
- `hermes` URL
- `openclaw` URL
- 本地端口
- systemd unit 名

## 5. 路由决策

单 Agent 显式路由示例：

```json
{
  "jsonrpc": "2.0",
  "id": "route-agent-1",
  "method": "xworkmate.routing.resolve",
  "params": {
    "taskPrompt": "create a powerpoint deck",
    "workingDirectory": "/tmp/work",
    "routing": {
      "routingMode": "explicit",
      "explicitExecutionTarget": "singleAgent",
      "explicitProviderId": "codex"
    }
  }
}
```

Gateway/OpenClaw 显式路由示例：

```json
{
  "jsonrpc": "2.0",
  "id": "route-gateway-1",
  "method": "xworkmate.routing.resolve",
  "params": {
    "taskPrompt": "run this through OpenClaw",
    "workingDirectory": "/tmp/work",
    "routing": {
      "routingMode": "explicit",
      "explicitExecutionTarget": "gateway",
      "preferredGatewayProviderId": "openclaw"
    }
  }
}
```

返回字段：

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

## 6. 任务执行

单 Agent 任务：

```json
{
  "jsonrpc": "2.0",
  "id": "task-1",
  "method": "session.start",
  "params": {
    "sessionId": "s1",
    "threadId": "t1",
    "taskPrompt": "create a powerpoint deck",
    "workingDirectory": "/tmp/work",
    "routing": {
      "routingMode": "explicit",
      "explicitExecutionTarget": "singleAgent",
      "explicitProviderId": "codex"
    }
  }
}
```

OpenClaw gateway 任务的 HTTP task submit 入口是：

```text
POST https://xworkmate-bridge.svc.plus/acp/rpc
```

它承载 `session.start` 和 follow-up `session.message`，并由 routing 字段明确声明 OpenClaw gateway 目标。

```json
{
  "jsonrpc": "2.0",
  "id": "gateway-task-1",
  "method": "session.start",
  "params": {
    "sessionId": "s-gateway-1",
    "threadId": "t-gateway-1",
    "taskPrompt": "run through OpenClaw",
    "workingDirectory": "/tmp/work",
    "routing": {
      "routingMode": "explicit",
      "explicitExecutionTarget": "gateway",
      "preferredGatewayProviderId": "openclaw"
    }
  }
}
```

OpenClaw 的 `session.message` 复用同一 `sessionId` / `threadId`，继续提交到
`/acp/rpc`。其他 provider 的 `session.message` 也走 `/acp` 或 `/acp/rpc`。

`session.cancel` 和 `session.close` 属于 control-plane 操作，继续走 `/acp` 或 `/acp/rpc`。

## 7. 清理后的 Public Surface

| Path | 协议 | APP 是否使用 | 设计定位 |
| --- | --- | --- | --- |
| `/acp` | WebSocket | 是，默认 | JSON-RPC 主入口 |
| `/acp/rpc` | HTTP POST | fallback / CI / 调试 / OpenClaw task submit | JSON-RPC 辅助入口 |
| `/api/ping` | HTTP GET | 否 | 发布与运行健康检查 |
| `/` | HTTP GET | 否 | 简单运行状态 |
| `/acp-server/*` | 无 APP contract | 否 | 线上 Caddy 显式返回 `404` |

陈旧接口清理规则：

- 删除 APP 侧对 `/acp-server/codex`、`/acp-server/opencode`、`/acp-server/gemini`、`/acp-server/hermes` 的任何引用。
- APP 的 Gateway/OpenClaw `session.start` 与 follow-up `session.message` 使用 `/acp/rpc` 和 routing metadata。
- 禁止把 provider/gateway 专用 URL 保存或解析为全局 ACP base endpoint。
- 不在 APP 侧保存 provider/gateway URL、端口或 service 名。
- 不把 provider 选择逻辑散落在 APP 的 URL 拼接逻辑里。
- 所有 provider/gateway 能力与可用性都来自 `acp.capabilities`。

## 8. 线上环境事实

以下为 2026-06-06 通过 `ssh ubuntu@xworkmate-bridge.svc.plus` 核对的部署事实。它们用于 bridge 运维和验证，不属于 APP contract。

### Caddy

`/etc/caddy/conf.d/xworkmate-bridge.caddy` 当前只反代：

```text
/api* -> 127.0.0.1:8787
/acp* -> 127.0.0.1:8787
/artifacts/* -> 127.0.0.1:8787
/acp-server/* -> 404
/     -> 127.0.0.1:8787
```

Caddy 层要求：

```http
Authorization: Bearer $BRIDGE_AUTH_TOKEN
```

如果配置了 Apple review / beta token，Caddy 层也必须接受：

```http
Authorization: Bearer $BRIDGE_REVIEW_AUTH_TOKEN
```

验证标准：

- 无 token：`401`
- 主 token：`200`
- review token：`200`

### Systemd / Local Listeners

| Unit / Runtime | Listener | 说明 |
| --- | --- | --- |
| `~/.config/systemd/user/xworkmate-bridge.service` | `127.0.0.1:8787` | Public bridge origin, runs as `ubuntu`, binary at `/home/ubuntu/.local/bin/xworkmate-go-core` |
| `acp-codex.service` | `127.0.0.1:9001` | Codex ACP backend |
| `acp-gemini.service` | `127.0.0.1:8791` | Gemini adapter |
| `acp-hermes.service` | `127.0.0.1:3920` | Hermes adapter |
| `acp-opencode.service` | `127.0.0.1:38992` | OpenCode adapter |
| OpenClaw runtime process | `ws://127.0.0.1:18789` | OpenClaw gateway runtime listener |

这些地址只允许 bridge 内部使用。APP 不保存、不展示、不请求这些地址。

验证时 `systemctl --user status xworkmate-bridge.service` 为 `active`，系统级 `/etc/systemd/system/xworkmate-bridge.service` 为 `disabled/inactive`。`acp-codex`、`acp-gemini`、`acp-hermes`、`acp-opencode` 仍由现有本机 service 提供。APP contract 只记录 provider/gateway 能力，不把 systemd unit 类型作为 APP 可见状态。

## 9. 线上验证结果

2026-05-03 使用 `Authorization: Bearer $BRIDGE_AUTH_TOKEN` 与 `Origin: https://xworkmate.svc.plus` 验证：

| 验证项 | 结果 |
| --- | --- |
| `GET /api/ping` | `200`，返回二进制运行元数据 `status/commit/version/buildDate` |
| WebSocket `/acp` 握手 | `101 Switching Protocols` |
| WebSocket `acp.capabilities` | `ok=true`，返回 `agent/gateway`、`codex/opencode/gemini/hermes`、`openclaw` |
| `POST /acp/rpc acp.capabilities` | `200`，返回同一能力目录 |
| `POST /acp/rpc session.start` with OpenClaw routing | `200`，成功或 structured provider failure；不应是 route/auth failure |
| `POST /acp-server/hermes` | `404` |
| `POST /acp-server/codex` | `404` |
| `POST /acp-server/gemini` | `404` |
| `POST /acp-server/opencode` | `404` |

OpenClaw task submit 当前使用 `/acp/rpc` 加 routing metadata。APP 的 capabilities、routing、agent、multi-agent、cancel 和 close 也必须继续使用 `/acp` 或 `/acp/rpc`。
