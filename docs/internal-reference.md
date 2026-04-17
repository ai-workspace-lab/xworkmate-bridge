# Internal Reference

本文档记录 `xworkmate-bridge` 的内部实现参考，覆盖 package、关键 `struct` / `interface`、导出函数，以及核心运行时中的关键未导出主链路函数。

术语约定：

- “类” 在本仓库映射为 Go 的 `struct` 与 `interface`
- “接口” 同时指外部协议接口与 Go `interface`
- “参数 / 返回” 同时包含 Go 签名与运行时语义

## internal/acp

### 包职责

APP-facing bridge 主控面。该模块已全面切换至 **JSON-RPC 2.0** 作为默认协议。负责 HTTP / WebSocket 路由、JSON-RPC method dispatch、session / thread 队列、routing resolve、single-agent / multi-agent / gateway 执行分流，以及 provider / gateway runtime 桥接。

### 主要输入 / 输出

- 输入：HTTP 请求、WebSocket RPC frame、stdio RPC 请求、bridge 环境变量
- 输出：JSON-RPC result / error envelope、`session.update` notification、gateway 转发结果、provider 转发结果

### 关键类型

- `type Server struct`
  - 作用：bridge 运行时聚合根，持有 session map、thread queue、gateway manager、provider catalog、auth service。
  - 主要副作用：维护内存态 session / queue，并向下调用 gatewayruntime、router、mounts、dispatch、shared。

### 关键函数 / 方法

- `func Serve(args []string) error`
  - 参数：`args` 为 `serve` 子命令参数。
  - 返回：监听失败或 server 非正常退出时返回 error。
  - 副作用：创建 `http.Server`，监听 `ACP_LISTEN_ADDR`。
  - 场景：`main.go` 中的 `serve` 模式入口。

- `func NewServer() *Server`
  - 参数：无。
  - 返回：初始化后的 `*Server`。
  - 副作用：装载内建 provider catalog、gateway manager、static token auth service。
  - 场景：`Serve` 和测试中的 server 构造。

- `func (s *Server) Handler() http.Handler`
  - 参数：receiver `s *Server`。
  - 返回：统一的 HTTP handler。
  - 副作用：按路径路由到 `/`、`/api/ping`、`/bridge/bootstrap/health`、`/acp/rpc`、`/acp`。
  - 场景：bridge HTTP 服务挂载。

- `func (s *Server) HandleRPC(w http.ResponseWriter, r *http.Request)`
  - 参数：标准 HTTP writer/request。
  - 返回：无显式返回；通过 HTTP body 输出 JSON-RPC envelope。
  - 副作用：处理 CORS、auth、body decode、SSE streaming、RPC dispatch。
  - 场景：`POST /acp/rpc`。

- `func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request)`
  - 参数：标准 HTTP writer/request。
  - 返回：无显式返回；通过 WebSocket 发回 JSON-RPC result / notification。
  - 副作用：做 origin/auth 校验，升级连接，持续读取 RPC frame。
  - 场景：`GET /acp`。

- `func (s *Server) HandleBridgeBootstrapHealth(w http.ResponseWriter, r *http.Request)`
  - 参数：标准 HTTP writer/request。
  - 返回：无显式返回；输出 bridge health JSON。
  - 副作用：读取 `BRIDGE_SERVER_URL`。
  - 场景：bootstrap 自检或部署健康检查。

- `func RunStdio(input io.Reader, output io.Writer)`
  - 参数：stdio 输入输出流。
  - 返回：无。
  - 副作用：运行 stdio ACP bridge。
  - 场景：`acp-stdio` 模式。

### 核心未导出主链路

- `func (s *Server) handleRequest(request shared.RPCRequest, notify func(map[string]any)) (map[string]any, *shared.RPCError)`
  - 参数：解码后的 RPC 请求、通知回调。
  - 返回：成功 result map 或 RPCError。
  - 副作用：分派所有 bridge JSON-RPC 方法。
  - 场景：HTTP 与 WebSocket 统一 RPC dispatch 核心。

- `func (s *Server) executeSessionTask(task task) (map[string]any, *shared.RPCError)`
  - 参数：排队后的内部任务对象。
  - 返回：执行结果或 RPCError。
  - 副作用：强制 routing、创建 / 更新 session、按 resolved mode 分流。
  - 场景：thread queue 实际执行入口。

- `func resolveRoutingMetadataWithProviders(params map[string]any, availableProviders []string) (router.Result, bool)`
  - 参数：JSON-RPC params、当前 bridge 可用 provider 列表。
  - 返回：`router.Result` 与 “是否提供 routing” 标记。
  - 副作用：读取 routing、skill install approval、memory 偏好。
  - 场景：`xworkmate.routing.resolve` 与 session 执行前置。

- `func (s *Server) runSingleAgent(ctx context.Context, method string, session *session, params map[string]any, turnID string, notify func(map[string]any)) taskResult`
  - 参数：上下文、RPC method、session、执行参数、turnId、通知回调。
  - 返回：内部 `taskResult`。
  - 副作用：选择 provider、归一化 working directory、对外做 ACP forwarding。
  - 场景：single-agent 主路径。

- `func (s *Server) runMultiAgent(ctx context.Context, session *session, params map[string]any, turnID string, notify func(map[string]any)) taskResult`
  - 参数：上下文、session、执行参数、turnId、通知回调。
  - 返回：multi-agent 汇总结果。
  - 副作用：调用 OpenAI-compatible chat completion，产出多智能体总结。
  - 场景：复杂任务升级到 multi-agent 时。

- `func (s *Server) runGateway(ctx context.Context, method string, session *session, params map[string]any, turnID string, notify func(map[string]any)) taskResult`
  - 参数：上下文、RPC method、session、执行参数、turnId、通知回调。
  - 返回：gateway 执行结果。
  - 副作用：调用 `gatewayruntime.Manager.RequestByMode`。
  - 场景：gateway-chat / gateway 模式。

- `func (s *Server) runSingleAgentViaExternalProvider(ctx context.Context, provider syncedProvider, method string, params map[string]any, notify func(map[string]any)) (map[string]any, error)`
  - 参数：上下文、bridge 内建 provider、RPC method、转发参数、通知回调。
  - 返回：upstream ACP result 或 error。
  - 副作用：HTTP / WebSocket forwarding、auth header 选择、结果补全。
  - 场景：对 `codex` / `opencode` / `gemini` 做桥接转发。

- `func handleGatewayConnect(server *Server, params map[string]any, notify func(map[string]any)) map[string]any`
  - 参数：server、JSON-RPC params、通知回调。
  - 返回：gateway connect 结果。
  - 副作用：把 JSON 参数映射为 `gatewayruntime.ConnectRequest`，应用 production routing。
  - 场景：`xworkmate.gateway.connect`。

## internal/gatewayruntime

### 包职责

维护 gateway runtime 生命周期，包括建连、认证挑战、请求 / 响应配对、push 事件规范化、重连与快照通知。

### 关键类型

- `type Endpoint struct { Host string; Port int; TLS bool }`
  - 作用：声明 gateway endpoint。

- `type PackageInfo struct { AppName string; PackageName string; Version string; BuildNumber string }`
  - 作用：附带客户端包信息。

- `type DeviceInfo struct { Platform string; PlatformVersion string; DeviceFamily string; ModelIdentifier string }`
  - 作用：附带设备平台信息。

- `func (d DeviceInfo) PlatformLabel() string`
  - 参数：receiver `DeviceInfo`。
  - 返回：`Platform` 与 `PlatformVersion` 拼出的展示文本。
  - 场景：快照或日志展示。

- `type DeviceIdentity struct { DeviceID string; PublicKeyBase64URL string; PrivateKeyBase64URL string }`
  - 作用：携带设备标识与签名密钥材料。

- `type AuthConfig struct { Token string; DeviceToken string; Password string }`
  - 作用：承载 gateway connect 认证材料。

- `type ConnectRequest struct`
  - 参数字段：`RuntimeID`、`Mode`、`ClientID`、`Locale`、`UserAgent`、`Endpoint`、`ReportedRemoteAddress`、`ConnectAuthMode`、`ConnectAuthFields`、`ConnectAuthSources`、`HasSharedAuth`、`HasDeviceToken`、`PackageInfo`、`DeviceInfo`、`Identity`、`Auth`。
  - 作用：gateway 建连请求的完整输入对象。

- `type ConnectResult struct { OK bool; Snapshot map[string]any; Auth map[string]any; ReturnedDeviceToken string; Error map[string]any }`
  - 作用：建连返回体。

- `type RequestResult struct { OK bool; Payload any; Error map[string]any }`
  - 作用：gateway request 返回体。

- `type GatewayError struct { Message string; Code string; Details map[string]any }`
  - 作用：规范化 gateway 错误。

- `func (e *GatewayError) Error() string`
  - 返回：错误消息字符串。

- `func (e *GatewayError) DetailCode() string`
  - 返回：`Details["code"]` 的规范化 detail code。

- `func (e *GatewayError) Map() map[string]any`
  - 返回：适合写入 JSON / RPC 的错误对象。

- `type Manager struct`
  - 导出字段：`ReconnectDelay`、`ConnectTimeout`、`ChallengeTimeout`
  - 作用：gateway runtime manager，内部持有 runtime session map。

### 关键函数 / 方法

- `func NewManager() *Manager`
  - 返回：带默认重连与超时参数的 manager。

- `func (m *Manager) Connect(request ConnectRequest, notify func(map[string]any)) ConnectResult`
  - 参数：connect request、通知回调。
  - 返回：connect 结果。
  - 副作用：查找或创建 runtime session，配置 session 并发起建连。
  - 场景：`xworkmate.gateway.connect`。

- `func (m *Manager) Request(runtimeID string, method string, params map[string]any, timeout time.Duration, notify func(map[string]any)) RequestResult`
  - 参数：runtimeId、method、params、超时、通知回调。
  - 返回：request 结果。
  - 副作用：把 gateway RPC 发到已连接 runtime。
  - 场景：`xworkmate.gateway.request`。

- `func (m *Manager) RequestByMode(mode string, method string, params map[string]any, timeout time.Duration, notify func(map[string]any)) RequestResult`
  - 参数：gateway mode、method、params、超时、通知回调。
  - 返回：request 结果。
  - 副作用：按 mode 选择当前已连接 session。
  - 场景：bridge gateway 执行路径。

- `func (m *Manager) Disconnect(runtimeID string, notify func(map[string]any))`
  - 参数：runtimeId、通知回调。
  - 返回：无。
  - 副作用：断开指定 runtime session。
  - 场景：`xworkmate.gateway.disconnect`。

### 核心未导出主链路

- `func newSession(manager *Manager, runtimeID string) *session`
- `func (s *session) configure(request ConnectRequest, notify func(map[string]any))`
- `func (s *session) connect() ConnectResult`
- `func (s *session) connectAttempt() (ConnectResult, *GatewayError)`
- `func (s *session) request(method string, params map[string]any, timeout time.Duration) RequestResult`
- `func (s *session) requestRemote(method string, params map[string]any, timeout time.Duration, requireConnected bool) RequestResult`
- `func (s *session) disconnect()`
- `func (s *session) readLoop(conn *websocket.Conn, challengeCh chan string)`
- `func (s *session) scheduleReconnect()`
- `func (s *session) emitNotification(method string, params map[string]any)`

这些函数共同组成 gateway runtime 的状态机：配置、建连、读循环、请求发出、错误处理、重连调度以及 `xworkmate.gateway.snapshot` / `xworkmate.gateway.push` / `xworkmate.gateway.log` 通知发射。

## internal/router

### 包职责

根据 prompt、显式 routing、memory 偏好、provider 可用性与 skill 解析结果，决定任务应该走 `single-agent`、`multi-agent` 还是 `gateway`。

### 常量

- `RoutingModeAuto = "auto"`
- `RoutingModeExplicit = "explicit"`
- `ExecutionTargetSingleAgent = "single-agent"`
- `ExecutionTargetMultiAgent = "multi-agent"`
- `ExecutionTargetGateway = "gateway"`
- `ExecutionTargetGatewayChat = "gateway-chat"`
- `GatewayProviderOpenClaw = "openclaw"`

### 关键类型

- `type Request struct`
  - 关键字段：`Prompt`、`WorkingDirectory`、`RoutingMode`、`PreferredGatewayProviderID`、`ExplicitExecutionTarget`、`ExplicitProviderID`、`ExplicitModel`、`ExplicitSkills`、`AllowSkillInstall`、`InstallApproval`、`AvailableSkills`、`AvailableProviders`、`AIGatewayBaseURL`、`AIGatewayAPIKey`。
  - 作用：routing 计算输入。

- `type Result struct`
  - 关键字段：`ResolvedExecutionTarget`、`ResolvedProviderID`、`ResolvedGatewayProviderID`、`ResolvedModel`、`ResolvedSkills`、`SkillResolutionSource`、`SkillCandidates`、`NeedsSkillInstall`、`SkillInstallRequestID`、`MemorySources`、`Unavailable`、`UnavailableCode`、`UnavailableMessage`。
  - 作用：routing 计算输出。

- `type Resolver struct`
  - 字段：`SkillFinder`、`SkillInstaller`、`MemoryService`、`Classifier`
  - 作用：routing 聚合器。

- `type ClassificationRequest struct { Prompt string; AIGatewayBaseURL string; AIGatewayAPIKey string }`
  - 作用：传给分类器的输入。

- `type Classifier interface { Classify(req ClassificationRequest) string }`
  - 作用：抽象分类器接口。

- `type LLMClassifier struct{}`
  - 作用：默认分类器实现。

### 关键函数 / 方法

- `func NewResolver() Resolver`
  - 返回：绑定默认 skills finder / installer、memory service、classifier 的 resolver。

- `func (r Resolver) Resolve(req Request) Result`
  - 参数：routing request。
  - 返回：routing result。
  - 副作用：读取 memory、解析技能、决定 execution target / provider / gateway provider。
  - 场景：bridge session 执行前置与 `xworkmate.routing.resolve`。

- `func (LLMClassifier) Classify(req ClassificationRequest) string`
  - 参数：classification request。
  - 返回：execution target 候选字符串。
  - 场景：启发式规则未命中时的分类补充。

### 核心未导出主链路

- `func (r Resolver) resolveExecution(req Request, prefs memory.Preferences) (string, string)`
- `func (r Resolver) classify(req Request) string`
- `func mapExplicitTarget(value string, preferredGatewayProviderID string) (string, string)`
- `func resolveGatewayProvider(preferredGatewayProviderID string) string`
- `func resolveProvider(req Request, prefs memory.Preferences, availableProviders []string, executionTarget string) (string, bool, string, string)`

这些函数共同实现了 execution target 与 provider 的两阶段决策。

## internal/dispatch

### 包职责

在 provider 列表、capability 要求和 node runtime state 之间做轻量级 dispatch 解析，并产出 bridge / provider / node metadata。

### 关键类型

- `type Provider struct { ID string; Name string; DefaultArgs []string; Capabilities []string }`
- `type NodeState struct { SelectedAgentID string; GatewayConnected bool; ExecutionTarget string; RuntimeMode string; BridgeEnabled bool; BridgeState string; ResolvedCodexCLIPath string; ConfiguredCodexCLIPath string }`
- `type NodeInfo struct { ID string; Name string; Version string }`
- `type Request struct { Providers []Provider; PreferredProviderID string; RequiredCapabilities []string; NodeState *NodeState; NodeInfo *NodeInfo }`
- `type Result struct { Provider *Provider; AgentID string; Metadata map[string]any }`

### 关键函数

- `func Resolve(request Request) Result`
  - 参数：provider、capability、node state / info。
  - 返回：dispatch result。
  - 副作用：无外部 I/O；纯粹做 provider 选择和 metadata 组装。
  - 场景：`xworkmate.dispatch.resolve`。

- `func ResultMap(result Result) map[string]any`
  - 参数：dispatch result。
  - 返回：适合写回 RPC 的 map。
  - 场景：bridge handler 层输出序列化。

### 核心未导出主链路

- `func selectProvider(providers []Provider, preferredProviderID string, requiredCapabilities []string) *Provider`
  - 作用：优先选显式 provider，否则按 capability 过滤并按 ID 稳定排序。

## internal/geminiadapter

### 包职责

把 Gemini CLI / Gemini ACP stdio 协议包装成独立 HTTP / WebSocket ACP adapter，同时提供一条兼容本地 prompt runner 的 fallback 路径。

### 关键类型

- `type Server struct`
  - 作用：Gemini adapter 聚合根，持有 RPC client、auth service、provider 元数据、session map。

### 关键函数 / 方法

- `func Serve(args []string) error`
  - 参数：`gemini-acp-adapter` 子命令参数。
  - 返回：监听失败或 adapter 异常退出时返回 error。
  - 副作用：读取 `GEMINI_ADAPTER_*` 环境变量，启动 HTTP 服务。

- `func NewServer(client rpcClient) *Server`
  - 参数：Gemini ACP RPC client。
  - 返回：adapter server。
  - 副作用：绑定 auth token、provider label、allowed origins、session runner。

- `func (s *Server) HandleRPC(w http.ResponseWriter, r *http.Request)`
  - 作用：处理 `/acp/rpc`。

- `func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request)`
  - 作用：处理 `/acp` WebSocket。

### 核心未导出主链路

- `func (s *Server) handleRequest(request shared.RPCRequest) map[string]any`
- `func (s *Server) handleCapabilities() map[string]any`
- `func (s *Server) handleSessionRequest(method string, params map[string]any) map[string]any`
- `func (s *Server) handleCompatSessionRequest(method string, params map[string]any) map[string]any`

这些函数负责区分真实 upstream ACP 能力与本地兼容路径，并维护 adapter local session history。

## internal/toolbridge

### 包职责

默认模式下的本地 MCP-style 工具桥。负责从 stdin 读取 JSON-RPC / Content-Length frame，暴露 `chat`、`claude_review`、`vault_kv` 三个工具。

### 关键函数

- `func Run(input io.Reader, output io.Writer)`
  - 参数：输入流、输出流。
  - 返回：无。
  - 副作用：持续读取消息、解析 RPC、写回结果。
  - 场景：`main.go` 默认模式。

### 核心未导出主链路

- `func readMessage(reader *bufio.Reader) ([]byte, error)`
- `func writeMessage(output io.Writer, message map[string]any)`
- `func writeError(output io.Writer, id any, code int, message string)`
- `func handleRequest(request shared.RPCRequest) map[string]any`

## internal/service

### 包职责

认证服务层。一个分支做用户名 / 密码认证，另一个分支做 bearer token 认证。

### 关键类型与函数

- `var ErrInvalidCredentials = errors.New("invalid credentials")`

- `type AuthRepository interface { Verify(ctx context.Context, username, password string) (bool, error) }`
  - 作用：用户名 / 密码认证仓储抽象。

- `type AuthService struct`

- `func NewAuthService(repo AuthRepository) *AuthService`
  - 参数：认证仓储。
  - 返回：认证服务。

- `func (s *AuthService) Authenticate(ctx context.Context, username, password string) error`
  - 参数：上下文、用户名、密码。
  - 返回：认证成功返回 `nil`，失败返回 `ErrInvalidCredentials` 或仓储错误。

- `type StaticTokenAuthService struct`

- `func NewStaticTokenAuthService(expectedToken string) *StaticTokenAuthService`
  - 参数：期望 bearer token。
  - 返回：静态 token 服务。

- `func (s *StaticTokenAuthService) ValidateToken(token string) bool`
  - 参数：token 或 auth header。
  - 返回：是否通过校验。

- `func (s *StaticTokenAuthService) ValidateAuthorizationHeader(header string) bool`
  - 参数：HTTP `Authorization` header。
  - 返回：是否通过校验。
  - 规则：支持裸 token 和 `Bearer <token>`；若 expectedToken 为空，则接受任意非空 bearer。

## internal/handler

### 包职责

HTTP handler 适配层，把 `service` 层认证能力暴露为简单 HTTP endpoint。

### 关键类型与函数

- `type Authenticator interface { Authenticate(username, password string) error }`

- `type AuthHandler struct`

- `func NewAuthHandler(svc Authenticator) *AuthHandler`

- `func (h *AuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request)`
  - 参数：HTTP writer/request。
  - 返回：无显式返回。
  - 副作用：读取 JSON body，调用用户名 / 密码认证，返回 `200` 或 `401`。

- `func NewServiceAdapter(svc *service.AuthService) Authenticator`
  - 作用：把带 `context.Context` 的 `AuthService` 包装成 handler 所需接口。

- `type TokenAuthHandler struct`

- `func NewTokenAuthHandler(service *service.StaticTokenAuthService) *TokenAuthHandler`

- `func (h *TokenAuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request)`
  - 参数：HTTP writer/request。
  - 副作用：读取 `Authorization`，校验静态 token，成功返回 `{"ok": true}`。

## internal/memory

### 包职责

从全局 home memory、项目 home memory、项目本地 `.xworkmate/memory.md` 合并 memory，并在 auto routing 成功后把偏好写回项目级 memory。

### 关键类型与函数

- `type Source struct { Path string; Scope string }`
- `type Preferences struct { PreferredRoute string; PreferredModel string; PreferredSkills []string; Provider string }`
- `type LoadResult struct { MergedText string; Sources []Source; Preferences Preferences; ProjectFiles []string }`
- `type SuccessEntry struct { ResolvedExecutionTarget string; ResolvedProviderID string; ResolvedModel string; ResolvedSkills []string; Summary string }`
- `type Service struct { HomeDir string }`

- `func NewService(homeDir string) Service`
  - 参数：home 目录。
  - 返回：memory service。

- `func (s Service) Load(workingDirectory string) LoadResult`
  - 参数：工作目录。
  - 返回：合并后的文本、来源、偏好、项目文件路径。
  - 副作用：读文件；会过滤 token / password / secret 等敏感文本。

- `func (s Service) RecordSuccess(workingDirectory string, entry SuccessEntry) error`
  - 参数：工作目录、成功条目。
  - 返回：写入错误。
  - 副作用：向 `.xworkmate/memory.md` 或 home project memory 追加 auto route block。

## internal/mounts

### 包职责

计算 Codex / Claude / Gemini / OpenCode / OpenClaw / ARIS 的 mount 与 MCP 就绪状态，并在可行时把 managed MCP block 写入配置文件。

### 关键类型与函数

- `type ManagedMCPServer struct { ID string; Name string; Transport string; Command string; URL string; Args []string; Enabled bool }`
- `type Config struct { AutoSync bool; UsesAris bool; ManagedMCPServers []ManagedMCPServer }`
- `type ArisInput struct { Available bool; BundleVersion string; LLMChatServerPath string; SkillCount int; BridgeAvailable bool; Error string }`
- `type Request struct { Config Config; AIGatewayURL string; ConfiguredCodexCLIPath string; CodexHome string; OpencodeHome string; OpenClawHome string; Aris ArisInput }`
- `type MountTargetState struct { TargetID string; Label string; Available bool; SupportsSkills bool; SupportsMCP bool; SupportsAIGatewayInjection bool; DiscoveryState string; SyncState string; DiscoveredSkillCount int; DiscoveredMCPCount int; ManagedMCPCount int; Detail string }`
- `type Result struct { MountTargets []MountTargetState; ArisBundleVersion string; ArisCompatStatus string }`

- `func Reconcile(request Request) Result`
  - 参数：mount reconcile request。
  - 返回：所有目标的 reconcile 结果。
  - 副作用：在 `AutoSync` 打开时可能更新 Codex / OpenCode / OpenClaw 配置文件中的 managed block。

- `func ResultMap(result Result) map[string]any`
  - 作用：把 reconcile 结果序列化为 RPC 返回格式。

## internal/skills

### 包职责

解析显式技能、本地技能候选、fallback finder 和安装批准流，输出本轮应启用的技能集合及安装请求标记。

### 关键类型与函数

- `type Candidate struct { ID string; Label string; Description string; Installed bool }`
- `type Finder interface { Find(prompt string) []Candidate }`
- `type Installer interface { Install(candidates []Candidate) ([]Candidate, error) }`
- `type ResolveRequest struct { Prompt string; ExplicitSkills []string; AvailableSkills []Candidate; AllowSkillInstall bool; InstallApproval InstallApproval }`
- `type InstallApproval struct { RequestID string; ApprovedSkillKeys []string }`
- `type ResolveResult struct { ResolvedSkills []string; Candidates []Candidate; Source string; NeedsInstall bool; InstallRequestID string }`
- `type StaticFinder struct{}`
- `type ChainFinder struct { Primary Finder; Fallback Finder }`
- `type CommandFinder struct { Binary string }`
- `type CommandInstaller struct { Binary string }`

- `func Resolve(req ResolveRequest, finder Finder, installer Installer) ResolveResult`
  - 参数：技能解析请求、候选查找器、安装器。
  - 返回：resolved skills、候选、来源、安装请求。
  - 副作用：在批准条件满足时可能触发外部 installer 二进制。

- `func (StaticFinder) Find(prompt string) []Candidate`
- `func (f ChainFinder) Find(prompt string) []Candidate`
- `func (f CommandFinder) Find(prompt string) []Candidate`
- `func (i CommandInstaller) Install(candidates []Candidate) ([]Candidate, error)`
- `func NewDefaultFinder() Finder`
- `func NewDefaultInstaller() Installer`

## internal/shared

### 包职责

公共工具层：RPC envelope、参数读取、provider command 执行、OpenAI-compatible HTTP 调用、Vault KV bridge、prompt 组装等。

### 关键类型

- `type RPCRequest struct { JSONRPC string; ID any; Method string; Params map[string]any }`
- `type RPCError struct { Code int; Message string }`
- `type ToolCallParams struct { Name string; Arguments map[string]any }`
- `type VaultKVResult struct { Operation string; Mount string; Path string; Data map[string]any; Keys []string; Metadata map[string]any }`

### 关键函数

- `func DecodeRPCRequest(payload []byte) (RPCRequest, error)`
  - 参数：原始 JSON payload。
  - 返回：解码后的 RPCRequest；若 method 缺失则返回 error。

- `func WriteSSE(w http.ResponseWriter, payload map[string]any)`
  - 参数：writer、payload。
  - 副作用：按 SSE `data: ...` 形式写出。

- `func ResultEnvelope(id any, result map[string]any) map[string]any` (已升级为混合模式，支持 JSON-RPC 2.0 规范同时兼顾 legacy APP 字段)
- `func ErrorEnvelope(id any, code int, message string) map[string]any` (已升级为混合模式，确保 401 等错误能以 JSON 格式被 legacy APP 解析)
- `func NotificationEnvelope(method string, params map[string]any) map[string]any`
- `func ErrorResponse(id any, code int, message string) map[string]any`
- `func ToolTextResult(id any, content string) map[string]any`
- `func ToolErrorResult(id any, err error) map[string]any`

- `func EnvOrDefault(key, fallback string) string`
- `func StringArg(arguments map[string]any, key, fallback string) string`
- `func ListArg(arguments map[string]any, key string) []any`
- `func IntArg(raw string, fallback int) int`
- `func BoolArg(raw string, fallback bool) bool`
- `func NormalizeBaseURL(raw string) string`

- `func ResolveProviderCommand(provider, model, prompt, cwd string) (string, []string)`
  - 参数：provider、model、prompt、工作目录。
  - 返回：二进制路径和参数列表。
  - 场景：Codex / OpenCode / Claude / Gemini 命令构造。

- `func RunProviderCommand(ctx context.Context, provider, model, prompt, workingDirectory string) (string, error)`
  - 参数：上下文、provider、model、prompt、工作目录。
  - 返回：CLI 输出文本或 error。
  - 副作用：启动外部命令。

- `func NormalizeProviderWorkingDirectory(provider, requested string) (string, string)`
  - 参数：provider、请求目录。
  - 返回：规范化目录与有效目录。
  - 场景：Codex / OpenCode 目录可访问性保护。

- `func AugmentPromptWithAttachments(prompt string, params map[string]any) string`
- `func ComposeHistoryPrompt(history []string) string`
- `func CallOpenAICompatibleCtx(ctx context.Context, baseURL, apiKey, model string, messages []map[string]string) (string, error)`
- `func CallOpenAICompatible(baseURL, apiKey, model string, messages []map[string]string) (string, error)`
- `func HandleChatTool(arguments map[string]any) (string, error)`
- `func HandleClaudeReviewTool(arguments map[string]any) (string, error)`
- `func RunClaudeReview(prompt, model, system, tools string, timeout time.Duration) (string, error)`
- `func ParseClaudeJSON(raw string) (map[string]any, error)`
- `func HandleVaultKVTool(arguments map[string]any) (string, error)`

### 调用关系

- `internal/acp` 同时依赖 shared 的 RPC、provider command、OpenAI-compatible、Vault / tool helpers。
- `internal/toolbridge` 直接把 shared 作为工具实现层。
- `internal/geminiadapter` 通过 shared 运行 provider command 和读取环境变量。
