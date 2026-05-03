# XWorkmate Bridge 文档导航

本文档集面向 `xworkmate-bridge` 的内部工程协作，目标是把设计、入口、协议、内部实现和测试材料串成一条连续阅读链路。

约定口径：

- “设计” 指系统边界、运行模式、主链路和约束。
- “接口” 同时覆盖对外 HTTP / WebSocket / JSON-RPC 协议，以及 Go 代码中的 `interface` 类型。
- “类” 在本仓库按 Go 语义映射为 `struct` 与 `interface`。
- “参数 / 返回” 既指协议参数 / 返回体，也指 Go 函数与方法的参数 / 返回值。

## 推荐阅读顺序

### 1. 架构与设计

- [架构拓扑：ACP Forwarding Topology](./architecture/acp-forwarding-topology.md)
- [ADR：Unified Bridge Entry Points](./architecture/adr-unified-bridge-entrypoints.md)
- [运行时设计：Bridge Runtime Design](./architecture/bridge-runtime-design.md)

### 2. 运行入口与对外接口

- [Backend API Design](./backend-api-design.md)
- [API Interface Reference](./api-reference.md)

### 3. 内部包与实现参考

- [Internal Reference](./internal-reference.md)

### 4. 测试与验证材料

- [Core Functional Test Plan](./xworkmate-bridge-svc-plus-core-functional-test-plan-v1.md)
- [ACP Public Validation 2026-04-09](./acp-public-validation-2026-04-09.md)
- [Remote Agent Local Workspace Test Matrix](./testing/remote-agent-local-workspace-test-matrix.md)
- [Gemini ACP Adapter Notes](./gemini-acp-adapter.md)

## 文档组织原则

- `docs/api-reference.md` 是对外运行契约的唯一真相来源。
- `docs/architecture/*.md` 负责解释设计，不重复维护完整参数表。
- `docs/internal-reference.md` 负责解释内部包、类型、函数和关键主链路，不再复制大段协议示例。
- 当文档与代码冲突时，以当前仓库代码为准，并优先修正文档。
