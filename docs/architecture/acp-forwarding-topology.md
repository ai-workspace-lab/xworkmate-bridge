# ACP Forwarding Topology

Last Updated: 2026-04-23

本文档只描述当前保留的 canonical topology。

## App-Facing Mainline

对 `xworkmate-app` 来说，bridge 只有一个 canonical surface：

- `GET /acp` WebSocket
- `POST /acp/rpc`

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
        B2["POST /acp/rpc<br/>App-facing HTTP RPC"]
        B3["acp.capabilities"]
        B4["xworkmate.routing.resolve"]
        B5["session.*"]
        B6["xworkmate.gateway.*"]
        B7["provider_compat"]
        B8["gateway compat"]
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
    B5 --> B7
    B6 --> B8
    B7 --> C1
    B7 --> C2
    B7 --> C3
    B7 --> C4
    B8 --> D1
```

## Invariants

- app 不直接访问 provider-specific public URL
- app 不直接访问 openclaw public URL
- provider catalog 与 gatewayProviders 由 bridge 独占生成
- bridge 只暴露 canonical ACP contract
- provider / gateway 实际地址属于 bridge internal truth

## Non-Contract Facts

下列事实可能存在于部署层，但不是 app contract：

- `127.0.0.1:*` 端口
- systemd unit 名
- adapter runtime 监听地址
- stdio / process lifecycle
