# Internal Reference

本文档记录 `xworkmate-bridge` 当前内部模块边界，只覆盖当前保留的 control-plane 主链。

## 1. 总体分层

当前内部结构分为五层：

1. `bridge_contract`
2. `capability_catalog`
3. `routing_engine`
4. `session_orchestrator`
5. `provider_compat` / `gateway compat`

核心原则：

- bridge core 不再承担 reverse proxy public surface
- bridge core 不再承担 multi-agent 业务分叉
- stdio / process / framing / restart 细节下沉到 adapter runtime

## 2. `internal/acp`

### 包职责

APP-facing ACP control plane。

负责：

- `/acp/rpc` App-facing HTTP RPC transport
- `/acp` WebSocket transport variant
- JSON-RPC / hybrid envelope
- `acp.capabilities`
- `xworkmate.routing.resolve`
- `session.*`
- `xworkmate.gateway.*`

### 关键类型

- `type Server struct`
  - bridge 聚合根
  - 持有 `routingEngine`、`providers`、`catalog`、`orchestrator`、`gateway`

- `type ProviderCompat interface`
  - bridge 依赖的唯一 provider 接口
  - 方法：
    - `ID()`
    - `Metadata()`
    - `Probe(ctx)`
    - `StartSession(...)`
    - `SendMessage(...)`
    - `CancelSession(...)`
    - `CloseSession(...)`

- `type RoutingResult struct`
  - routing 输出 contract
  - 包括：
    - `TargetID`
    - `ProviderID`
    - `GatewayProviderID`
    - `Model`
    - `Skills`
    - `Status`
    - `UnavailableCode`
    - `UnavailableMsg`

- `type CapabilityCatalog struct`
  - bridge-owned capabilities 真源
  - 包括：
    - `ProviderCatalog`
    - `GatewayProviders`
    - `AvailableExecutionTargets`
    - `ProviderProbeSummary`

### 关键主链

- `func NewServer() *Server`
  - 初始化 routing engine、catalog、providers、orchestrator、gateway manager

- `func (s *Server) Handler() http.Handler`
  - 只挂载：
    - `/`
    - `/api/ping`
    - `/acp`
    - `/acp/rpc`

- `func (s *Server) handleRequest(...)`
  - 统一分派：
    - `acp.capabilities`
    - `session.*`
    - `xworkmate.routing.resolve`
    - `xworkmate.gateway.*`

- `func (o *SessionOrchestrator) Process(...)`
  - bridge session 主语义入口
  - 流程：
    - resolve routing
    - get/create session state
    - dispatch to gateway or provider compat
    - normalize result
    - emit `session.update`
    - record project memory only when routing mode is explicitly auto

### 当前删除的旧路径

以下逻辑已从主链移除：

- `/acp-server/*`
- `/gateway/openclaw` public alias handler
- multi-agent 执行路径
- provider-specific alias handler

## 3. `provider_compat`

bridge 当前通过 compat 层吸收 provider 差异：

- `codex compat`
- `opencode compat`
- `gemini compat`
- `hermes compat`

这些 compat 共享同一套 external ACP 调用基座，但对 bridge core 暴露统一接口。

### compat 负责什么

- 调上游 ACP HTTP / WS
- 解析 JSON-RPC result / error
- 转换 upstream notification
- 吸收 provider transport 差异

### compat 不负责什么

- catalog ownership
- routing ownership
- session state ownership
- app-facing contract 设计

## 4. `routing_engine`

当前 routing 只围绕 bridge 核心能力做决策：

- `single-agent`
- `gateway`

不再升级到 multi-agent。

输入来源：

- prompt
- workingDirectory
- explicit routing
- preferred gateway provider
- available bridge providers
- available skills
- memory preferences

输出由 bridge 统一整形成 `RoutingResult`。

## 5. `gatewayruntime`

`internal/gatewayruntime` 仍负责 openclaw runtime 的连接态、请求配对、快照与 push event。

这个包是 gateway runtime 层，不是 public API 层。

bridge 只通过 `xworkmate.gateway.*` 把它暴露为 control-plane contract。

## 6. `internal/router`

任务：

- normalize routing intent
- resolve execution target
- resolve provider
- resolve gateway provider
- resolve skills
- shape unavailable reason

当前只产出：

- `single-agent`
- `gateway`

## 7. `internal/shared`

保留的 bridge_contract 基础能力：

- JSON-RPC request decode
- hybrid result/error envelope
- notification envelope
- CORS / origin helper
- shared HTTP / WS helper

这里是 envelope 与共用 transport helper，不承载 provider-specific 控制逻辑。
