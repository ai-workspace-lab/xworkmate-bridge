# ACP Forwarding Topology

Last Updated: 2026-05-03

本文档只描述当前保留的 canonical topology。

## App-Facing Mainline

对 `xworkmate-app` 来说，bridge 只有一个 canonical surface：

- `GET /acp` WebSocket，默认主链
- `POST /acp/rpc`，CI、脚本、调试和兼容 fallback
- `POST /gateway/openclaw`，仅 OpenClaw `session.start` / follow-up `session.message` task submit

app 只感知 method family：

- `acp.capabilities`
- `xworkmate.routing.resolve`
- `session.*`
- `xworkmate.gateway.*`

## Canonical Topology

```mermaid
flowchart LR
    subgraph APP["xworkmate-app"]
        A1["Assistant / Settings / Runtime UI"]
        A2["Canonical ACP client"]
        A1 --> A2
    end

    subgraph BRIDGE["xworkmate-bridge"]
        B1["GET /acp<br/>JSON-RPC over WebSocket"]
        B2["POST /acp/rpc<br/>HTTP fallback / CI"]
        B3["acp.capabilities"]
        B4["xworkmate.routing.resolve"]
        B5["session.*"]
        B6["xworkmate.gateway.*"]
        B7["POST /gateway/openclaw<br/>OpenClaw task submit only"]
        B8["provider_compat"]
        B9["gateway compat"]
    end

    subgraph ADAPTERS["adapter runtime"]
        C1["codex"]
        C2["opencode"]
        C3["gemini"]
        C4["hermes"]
    end

    subgraph GATEWAY["gateway runtime"]
        D1["openclaw"]
    end

    A2 --> B1
    A2 --> B2
    B1 --> B3
    B1 --> B4
    B1 --> B5
    B1 --> B6
    B2 --> B3
    B2 --> B4
    B2 --> B5
    B2 --> B6
    B7 --> B5
    B5 --> B8
    B6 --> B9
    B8 --> C1
    B8 --> C2
    B8 --> C3
    B8 --> C4
    B9 --> D1
```

## Invariants

- app 不直接访问 provider-specific public URL
- app 只在 OpenClaw `session.start` / follow-up `session.message` 时使用 `/gateway/openclaw`
- app 不把 `/gateway/openclaw` 解析或保存为全局 ACP base endpoint
- provider catalog 与 gatewayProviders 由 bridge 独占生成
- bridge 只暴露 canonical ACP contract
- provider / gateway 实际地址属于 bridge internal truth

## Non-Contract Facts

下列事实可能存在于部署层，但不是 app contract：

- `127.0.0.1:*` 端口
- systemd unit 名
- adapter runtime 监听地址
- stdio / process lifecycle
