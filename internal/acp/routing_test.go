package acp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"xworkmate-bridge/internal/shared"
)

func handleRoutingResolve(params map[string]any) map[string]any {
	server := NewServer()
	res, _ := server.routingEngine.Resolve(context.Background(), params)

	// Convert to map for tests
	m := map[string]any{
		"resolvedExecutionTarget":   res.TargetID,
		"resolvedProviderId":        res.ProviderID,
		"resolvedGatewayProviderId": res.GatewayProviderID,
		"resolvedModel":             res.Model,
		"resolvedSkills":            res.Skills,
		"status":                    res.Status,
		"unavailable":               res.Status == "unavailable",
		"unavailableCode":           res.UnavailableCode,
		"unavailableMessage":        res.UnavailableMsg,
		"skillResolutionSource":     res.SkillResolutionSource,
		"needsSkillInstall":         res.NeedsSkillInstall,
		"skillInstallRequestId":     res.SkillInstallRequestID,
	}
	return m
}

type task struct {
	req    shared.RPCRequest
	notify func(map[string]any)
}

func (s *Server) executeSessionTask(t task) (map[string]any, *shared.RPCError) {
	if t.req.Method == "" {
		t.req.Method = "session.start"
	}
	response, rpcErr := s.handleRequest(t.req, t.notify)
	if rpcErr != nil || strings.TrimSpace(shared.StringArg(response, "status", "")) != string(TaskStateRunning) {
		return response, rpcErr
	}
	return s.handleRequest(shared.RPCRequest{
		Method: "xworkmate.tasks.get",
		Params: map[string]any{
			"sessionId":                  shared.StringArg(response, "sessionId", ""),
			"threadId":                   shared.StringArg(response, "threadId", ""),
			"turnId":                     shared.StringArg(response, "turnId", ""),
			"runId":                      shared.StringArg(response, "runId", ""),
			"appThreadKey":               shared.StringArg(response, "appThreadKey", ""),
			"openclawSessionKey":         shared.StringArg(response, "openclawSessionKey", ""),
			"artifactScope":              shared.StringArg(response, "artifactScope", ""),
			"artifactDirectory":          shared.StringArg(response, "artifactDirectory", ""),
			"gatewayProviderId":          shared.StringArg(response, "resolvedGatewayProviderId", ""),
			"runtimeBudgetMinutes":       shared.StringArg(response, "runtimeBudgetMinutes", ""),
			"taskLoadClass":              shared.StringArg(response, "taskLoadClass", ""),
			"expectedArtifactExtensions": shared.ListArg(response, "expectedArtifactExtensions"),
			"requiredArtifactExtensions": shared.ListArg(response, "requiredArtifactExtensions"),
		},
	}, t.notify)
}

func newExternalSingleAgentProvider(
	t *testing.T,
	providerID string,
	output string,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acp/rpc" {
			http.NotFound(w, r)
			return
		}
		defer func() {
			_ = r.Body.Close()
		}()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		method := strings.TrimSpace(shared.StringArg(request, "method", ""))
		result := map[string]any{
			"success":  true,
			"output":   output,
			"turnId":   "turn-" + providerID,
			"provider": providerID,
			"mode":     "single-agent",
		}
		switch method {
		case "thread/start", "thread/resume":
			result = map[string]any{"id": "provider-thread-" + providerID}
		case "turn/start":
			result["summary"] = output
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result":  result,
		})
	}))
}

func TestHandleRoutingResolveCoversNineScenarioBuckets(t *testing.T) {
	localAvailableSkills := []map[string]any{
		{"id": "pptx", "label": "PPTX", "description": "slides", "installed": true},
		{"id": "docx", "label": "DOCX", "description": "docs", "installed": true},
		{"id": "xlsx", "label": "XLSX", "description": "sheets", "installed": true},
		{"id": "pdf", "label": "PDF", "description": "pdf", "installed": true},
		{"id": "image-resizer", "label": "image-resizer", "description": "image resize", "installed": true},
		{"id": "browser-automation", "label": "Browser Automation", "description": "browser", "installed": true},
	}

	cases := []struct {
		name                      string
		prompt                    string
		expectedExecutionTarget   string
		expectedGatewayProviderID string
		expectedSkillSource       string
		expectedResolvedSkill     string
		expectedNeedsSkillInstall bool
	}{
		{
			name:                    "powerpoint-pptx",
			prompt:                  "create a powerpoint deck for this launch",
			expectedExecutionTarget: "single-agent",
			expectedSkillSource:     "local_match",
			expectedResolvedSkill:   "PPTX",
		},
		{
			name:                    "word-docx",
			prompt:                  "draft a word document memo",
			expectedExecutionTarget: "single-agent",
			expectedSkillSource:     "local_match",
			expectedResolvedSkill:   "DOCX",
		},
		{
			name:                    "excel-xlsx",
			prompt:                  "build an excel workbook with formulas",
			expectedExecutionTarget: "single-agent",
			expectedSkillSource:     "local_match",
			expectedResolvedSkill:   "XLSX",
		},
		{
			name:                    "pdf",
			prompt:                  "merge and fill this pdf form",
			expectedExecutionTarget: "single-agent",
			expectedSkillSource:     "local_match",
			expectedResolvedSkill:   "PDF",
		},
		{
			name:                    "image-resizer",
			prompt:                  "batch resize image assets",
			expectedExecutionTarget: "single-agent",
			expectedSkillSource:     "local_match",
			expectedResolvedSkill:   "image-resizer",
		},
		{
			name:                      "image-cog",
			prompt:                    "use image-cog to generate consistent characters",
			expectedExecutionTarget:   "gateway",
			expectedGatewayProviderID: "openclaw",
			expectedSkillSource:       "find_skills",
			expectedNeedsSkillInstall: true,
		},
		{
			name:                      "image-video-generation-editting",
			prompt:                    "wan 图生视频并做视频编辑",
			expectedExecutionTarget:   "gateway",
			expectedGatewayProviderID: "openclaw",
			expectedSkillSource:       "find_skills",
			expectedNeedsSkillInstall: true,
		},
		{
			name:                      "video-translator",
			prompt:                    "translate video subtitles and dub the clip",
			expectedExecutionTarget:   "gateway",
			expectedGatewayProviderID: "openclaw",
			expectedSkillSource:       "find_skills",
			expectedNeedsSkillInstall: true,
		},
		{
			name:                      "browser-search-news",
			prompt:                    "跨浏览器执行并搜索最新资讯采集结果",
			expectedExecutionTarget:   "gateway",
			expectedGatewayProviderID: "openclaw",
			expectedSkillSource:       "local_match",
			expectedResolvedSkill:     "Browser Automation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := handleRoutingResolve(map[string]any{
				"taskPrompt":       tc.prompt,
				"workingDirectory": "/tmp/workspace",
				"routing": map[string]any{
					"routingMode":                "auto",
					"preferredGatewayProviderId": "openclaw",
					"allowSkillInstall":          false,
					"availableSkills": func() []any {
						values := make([]any, 0, len(localAvailableSkills))
						for _, item := range localAvailableSkills {
							values = append(values, item)
						}
						return values
					}(),
				},
			})

			if got := result["resolvedExecutionTarget"]; got != tc.expectedExecutionTarget {
				t.Fatalf("expected execution target %q, got %#v", tc.expectedExecutionTarget, got)
			}
			if tc.expectedGatewayProviderID != "" {
				if got := result["resolvedGatewayProviderId"]; got != tc.expectedGatewayProviderID {
					t.Fatalf("expected gateway provider %q, got %#v", tc.expectedGatewayProviderID, got)
				}
			}
			if _, exists := result["resolvedEndpointTarget"]; exists {
				t.Fatalf("expected resolvedEndpointTarget compatibility field to be removed, got %#v", result)
			}
			if got := result["skillResolutionSource"]; got != tc.expectedSkillSource {
				t.Fatalf("expected skill source %q, got %#v", tc.expectedSkillSource, got)
			}
			if tc.expectedResolvedSkill != "" {
				resolvedSkills, _ := result["resolvedSkills"].([]string)
				if len(resolvedSkills) == 0 || resolvedSkills[0] != tc.expectedResolvedSkill {
					t.Fatalf("expected resolved skill %q, got %#v", tc.expectedResolvedSkill, result["resolvedSkills"])
				}
			}
			if got := result["needsSkillInstall"]; got != tc.expectedNeedsSkillInstall {
				t.Fatalf("expected needsSkillInstall=%v, got %#v", tc.expectedNeedsSkillInstall, got)
			}
		})
	}
}

func TestHandleRoutingResolveAcceptsTopLevelGatewayContract(t *testing.T) {
	server := NewServer()
	server.mu.Lock()
	server.providerOrder = []string{"codex"}
	server.providers = map[string]ProviderCompat{
		"codex": newProviderCompat(syncedProvider{
			ProviderID: "codex",
			Label:      "Codex",
			Endpoint:   "ws://127.0.0.1:9001/acp",
			Enabled:    true,
		}),
	}
	server.mu.Unlock()

	res, err := server.routingEngine.Resolve(context.Background(), map[string]any{
		"taskPrompt":        "openclaw gateway task",
		"executionTarget":   "gateway",
		"gatewayProviderId": "openclaw",
	})
	if err != nil {
		t.Fatalf("resolve routing: %v", err)
	}

	if got := res.TargetID; got != "gateway" {
		t.Fatalf("expected gateway execution target, got %#v", got)
	}
	if got := res.GatewayProviderID; got != "openclaw" {
		t.Fatalf("expected openclaw gateway provider, got %#v", got)
	}
	if got := res.ProviderID; got != "" {
		t.Fatalf("expected no single-agent provider for gateway, got %#v", got)
	}
}

func TestExecuteSessionTaskAutoRoutingRecordsProjectMemory(t *testing.T) {
	homeDir := t.TempDir()
	workspaceDir := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	t.Setenv("HOME", homeDir)

	server := NewServer()
	providerServer := newExternalSingleAgentProvider(t, "codex", "done")
	defer providerServer.Close()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID: "codex",
		Label:      "Codex",
		Endpoint:   providerServer.URL,
		Enabled:    true,
	})
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Params: map[string]any{
				"sessionId":        "session-auto",
				"threadId":         "thread-auto",
				"provider":         "codex",
				"taskPrompt":       "create a powerpoint deck for launch",
				"workingDirectory": workspaceDir,
				"routing": map[string]any{
					"routingMode": "auto",
					"availableSkills": []any{
						map[string]any{
							"id":          "pptx",
							"label":       "PPTX",
							"description": "slides",
							"installed":   true,
						},
					},
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected success, got rpc error: %v", rpcErr)
	}
	if success, _ := response["success"].(bool); !success {
		t.Fatalf("expected success response, got %#v", response)
	}

	projectLocalMemory := filepath.Join(workspaceDir, ".xworkmate", "memory.md")
	content, err := os.ReadFile(projectLocalMemory)
	if err != nil {
		t.Fatalf("expected memory file %s: %v", projectLocalMemory, err)
	}
	text := string(content)
	if !strings.Contains(text, "preferred-route: single-agent") {
		t.Fatalf("expected preferred route in %s, got %q", projectLocalMemory, text)
	}
	if !strings.Contains(text, "preferred-skills: PPTX") {
		t.Fatalf("expected preferred skills in %s, got %q", projectLocalMemory, text)
	}
	projectHomeMemory := filepath.Join(
		homeDir,
		"self-improving",
		"projects",
		filepath.Base(workspaceDir)+".md",
	)
	if _, err := os.Stat(projectHomeMemory); !os.IsNotExist(err) {
		t.Fatalf("expected auto memory write to stay project-local only, got stat err=%v", err)
	}
}

func TestExecuteSessionTaskExplicitRoutingDoesNotRecordProjectMemory(t *testing.T) {
	homeDir := t.TempDir()
	workspaceDir := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	t.Setenv("HOME", homeDir)

	server := NewServer()
	providerServer := newExternalSingleAgentProvider(t, "codex", "done")
	defer providerServer.Close()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID: "codex",
		Label:      "Codex",
		Endpoint:   providerServer.URL,
		Enabled:    true,
	})
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Params: map[string]any{
				"sessionId":        "session-explicit",
				"threadId":         "thread-explicit",
				"provider":         "codex",
				"taskPrompt":       "create a powerpoint deck for launch",
				"workingDirectory": workspaceDir,
				"routing": map[string]any{
					"routingMode":             "explicit",
					"explicitExecutionTarget": "singleAgent",
					"explicitProviderId":      "codex",
					"availableSkills": []any{
						map[string]any{
							"id":          "pptx",
							"label":       "PPTX",
							"description": "slides",
							"installed":   true,
						},
					},
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected success, got rpc error: %v", rpcErr)
	}
	if success, _ := response["success"].(bool); !success {
		t.Fatalf("expected success response, got %#v", response)
	}

	projectHomeMemory := filepath.Join(
		homeDir,
		"self-improving",
		"projects",
		filepath.Base(workspaceDir)+".md",
	)
	projectLocalMemory := filepath.Join(workspaceDir, ".xworkmate", "memory.md")
	for _, target := range []string{projectHomeMemory, projectLocalMemory} {
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("expected no memory write for explicit routing at %s, err=%v", target, err)
		}
	}
}

func TestExecuteSessionTaskExplicitProviderRequiresAdvertisedBridgeProvider(t *testing.T) {
	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":  "session-explicit-provider",
				"threadId":   "thread-explicit-provider",
				"taskPrompt": "create a powerpoint deck for launch",
				"routing": map[string]any{
					"routingMode":             "explicit",
					"explicitExecutionTarget": "singleAgent",
					"explicitProviderId":      "claude",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected structured response, got rpc error: %v", rpcErr)
	}
	if success, _ := response["success"].(bool); success {
		t.Fatalf("expected unavailable response, got %#v", response)
	}
	if got := response["unavailableCode"]; got != "PROVIDER_UNAVAILABLE" {
		t.Fatalf("expected PROVIDER_UNAVAILABLE, got %#v", response)
	}
	if got := response["unavailableMessage"]; got != "explicit provider is unavailable" {
		t.Fatalf("expected explicit provider unavailable message, got %#v", response)
	}
}

func TestExecuteSessionTaskExplicitGatewayUsesResolvedGatewayProvider(t *testing.T) {
	server := NewServer()

	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":  "session-explicit-gateway",
				"threadId":   "thread-explicit-gateway",
				"taskPrompt": "search latest news",
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"explicitProviderId":         "claude",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr == nil {
		t.Fatalf("expected gateway connectivity rpc error, got response: %v", response)
	} else if rpcErr.Message == "GATEWAY_PROVIDER_REQUIRED" {
		t.Fatalf("expected resolved gateway provider to be reused, got %q", rpcErr.Message)
	}
}

func TestExecuteSessionTaskGatewayAutoConnectsLocalOpenClaw(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw",
				"threadId":         "thread-openclaw",
				"taskPrompt":       "say pong",
				"workingDirectory": t.TempDir(),
				"metadata": map[string]any{
					"xworkmateTaskArtifactContract": map[string]any{
						"expectedArtifactDirs": []any{"assets/images/", "reports/"},
					},
				},
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected gateway response, got rpc error: %#v", rpcErr)
	}
	if got := response["output"]; got != "gateway pong" {
		t.Fatalf("expected gateway pong output, got %#v", response)
	}
	if got := response["resolvedGatewayProviderId"]; got != "openclaw" {
		t.Fatalf("expected openclaw gateway provider, got %#v", response)
	}
	if gateway.ConnectCount() != 1 {
		t.Fatalf("expected one automatic gateway connect, got %d", gateway.ConnectCount())
	}
	if gateway.ChatSendCount() != 1 {
		t.Fatalf("expected one OpenClaw chat.send request, got %d", gateway.ChatSendCount())
	}
	if gateway.ArtifactPrepareCount() != 1 {
		t.Fatalf("expected one OpenClaw artifact prepare request before chat.send, got %d", gateway.ArtifactPrepareCount())
	}
	prepareParams := gateway.LastArtifactPrepareParams()
	if got := shared.StringArg(prepareParams, "appThreadKey", ""); got != "thread-openclaw" {
		t.Fatalf("expected prepare appThreadKey to match app thread, got %#v", prepareParams)
	}
	if got := shared.StringArg(prepareParams, "openclawSessionKey", ""); got != "agent:main:thread-openclaw" {
		t.Fatalf("expected readable OpenClaw session key, got %#v", prepareParams)
	}
	if _, ok := prepareParams["sessionKey"]; ok {
		t.Fatalf("expected prepare params to omit legacy sessionKey, got %#v", prepareParams)
	}
	if _, ok := prepareParams["sessionId"]; ok {
		t.Fatalf("expected prepare params to omit app sessionId, got %#v", prepareParams)
	}
	if _, ok := prepareParams["threadId"]; ok {
		t.Fatalf("expected prepare params to omit app threadId, got %#v", prepareParams)
	}
	if got := shared.StringArg(prepareParams, "workspaceDir", ""); got != "~/.openclaw/workspace" {
		t.Fatalf("expected bridge to supply OpenClaw workspaceDir, got %#v", prepareParams)
	}
	if got := shared.ListArg(prepareParams, "expectedArtifactDirs"); !sameAnyStringSlice(got, []string{"assets/images/", "reports/"}) {
		t.Fatalf("expected prepare expectedArtifactDirs from app contract, got %#v", prepareParams)
	}
	chatParams := gateway.LastChatSendParams()
	if got, want := shared.StringArg(prepareParams, "requestId", ""), shared.StringArg(chatParams, "idempotencyKey", ""); got == "" || got != want {
		t.Fatalf("expected prepare requestId to match chat idempotencyKey %q, got %#v", want, prepareParams)
	}
	if got, want := shared.StringArg(prepareParams, "externalTaskId", ""), shared.StringArg(chatParams, "idempotencyKey", ""); got == "" || got != want {
		t.Fatalf("expected prepare externalTaskId to match chat idempotencyKey %q, got %#v", want, prepareParams)
	}
	for _, key := range []string{
		"artifactDirectory",
		"artifactScope",
		"artifactScopeKind",
		"relativeArtifactDirectory",
		"remoteWorkingDirectory",
		"remoteWorkspaceRefKind",
		"xworkmateArtifacts",
		"expectedArtifactDirs",
	} {
		if _, ok := chatParams[key]; ok {
			t.Fatalf("expected chat.send params to omit bridge artifact/workspace field %q, got %#v", key, chatParams)
		}
	}
	receipt := strings.TrimSpace(shared.StringArg(chatParams, "systemProvenanceReceipt", ""))
	openClawSessionKey := shared.StringArg(chatParams, "sessionKey", "")
	if openClawSessionKey == "" {
		t.Fatalf("expected mapped OpenClaw sessionKey, got %#v", chatParams)
	}
	for _, expected := range []string{
		"artifactDirectory: /remote/openclaw/workspace/tasks/" + openClawSessionKey + "/" + shared.StringArg(chatParams, "idempotencyKey", ""),
		"artifactScope: tasks/" + openClawSessionKey + "/" + shared.StringArg(chatParams, "idempotencyKey", ""),
		"export XWORKMATE_TASK_ARTIFACT_DIR='/remote/openclaw/workspace/tasks/" + openClawSessionKey + "/" + shared.StringArg(chatParams, "idempotencyKey", "") + "'",
		"cd '/remote/openclaw/workspace/tasks/" + openClawSessionKey + "/" + shared.StringArg(chatParams, "idempotencyKey", "") + "'",
	} {
		if !strings.Contains(receipt, expected) {
			t.Fatalf("expected chat.send systemProvenanceReceipt to include %q, got %q", expected, receipt)
		}
	}
	if gateway.AgentWaitCount() != 0 {
		t.Fatalf("expected native task lookup to avoid Bridge-owned agent.wait, got %d", gateway.AgentWaitCount())
	}
	if gateway.ArtifactExportCount() != 0 {
		t.Fatalf("expected native task lookup to avoid Bridge-owned artifact export, got %d", gateway.ArtifactExportCount())
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.session.prepare", "chat.send", "xworkmate.tasks.get"}) {
		t.Fatalf("expected connect, prepare, chat.send, then native task lookup, got %#v", got)
	}
	client := gateway.LastConnectClient()
	if got := client["id"]; got != "openclaw-macos" {
		t.Fatalf("expected OpenClaw-compatible client id, got %#v", client)
	}
	if got := strings.TrimSpace(shared.StringArg(client, "modelIdentifier", "")); got == "" {
		t.Fatalf("expected non-empty modelIdentifier, got %#v", client)
	}
}

func TestOpenClawAgentWaitTimeoutAdaptsToVideoWork(t *testing.T) {
	base := openClawAgentWaitTimeout(
		map[string]any{"taskPrompt": "say pong"},
		map[string]any{"message": "say pong"},
	)
	video := openClawAgentWaitTimeout(
		map[string]any{
			"taskPrompt": "测试制作 云原生ServiceMesh网络 主题的 科普视频，使用 it-infra-evolution-video skill 渲染 mp4",
			"attachments": []any{
				map[string]any{"path": "assets/images/001.png"},
				map[string]any{"path": "assets/images/002.png"},
			},
		},
		map[string]any{"message": "测试制作 云原生ServiceMesh网络 主题的 科普视频，使用 it-infra-evolution-video skill 渲染 mp4"},
	)

	if base != openClawAgentWaitDefaultTimeout {
		t.Fatalf("expected simple task to use default timeout, got %s", base)
	}
	if video <= base {
		t.Fatalf("expected video task timeout %s to exceed simple task timeout %s", video, base)
	}
	if video > openClawAgentWaitMaxTimeout {
		t.Fatalf("expected video task timeout %s to stay within max %s", video, openClawAgentWaitMaxTimeout)
	}
}

func TestOpenClawAgentWaitTimeoutUsesOneHourForLongPDFImageWork(t *testing.T) {
	prompt := "拆分为多章节。每个章节调用 Codex 使用 GPT images2 模型制作插图，最后输出为更新版本 PDF 文件"

	timeout := openClawAgentWaitTimeout(
		map[string]any{"taskPrompt": prompt},
		map[string]any{"message": prompt},
	)

	if openClawAgentWaitMaxTimeout != time.Hour {
		t.Fatalf("expected OpenClaw max agent.wait timeout to be one hour, got %s", openClawAgentWaitMaxTimeout)
	}
	if timeout != openClawAgentWaitMaxTimeout {
		t.Fatalf("expected long PDF image task to use max timeout %s, got %s", openClawAgentWaitMaxTimeout, timeout)
	}
}

func TestGatewayRequestForwardsOpenClawSkillsStatus(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	connected, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "xworkmate.gateway.connect",
		Params: map[string]any{
			"runtimeId":         "app-runtime",
			"gatewayProviderId": "openclaw",
		},
	}, nil)
	if rpcErr != nil {
		t.Fatalf("expected gateway connect, got rpc error: %#v", rpcErr)
	}
	if connected["ok"] != true {
		t.Fatalf("expected gateway connect ok, got %#v", connected)
	}

	response, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "xworkmate.gateway.request",
		Params: map[string]any{
			"runtimeId": "app-runtime",
			"method":    "skills.status",
			"params": map[string]any{
				"agentId": "main",
			},
			"timeoutMs": float64(15000),
		},
	}, nil)
	if rpcErr != nil {
		t.Fatalf("expected skills.status gateway response, got rpc error: %#v", rpcErr)
	}
	if response["ok"] != true {
		t.Fatalf("expected skills.status ok, got %#v", response)
	}
	payload := shared.AsMap(response["payload"])
	if got := shared.StringArg(payload, "workspaceDir", ""); got != "/remote/openclaw/workspace" {
		t.Fatalf("expected workspaceDir from OpenClaw payload, got %#v", payload)
	}
	if got := shared.StringArg(payload, "managedSkillsDir", ""); got != "/remote/openclaw/skills" {
		t.Fatalf("expected managedSkillsDir from OpenClaw payload, got %#v", payload)
	}
	skills := shared.ListArg(payload, "skills")
	if len(skills) != 2 {
		t.Fatalf("expected two OpenClaw skill records, got %#v", payload["skills"])
	}
	first := shared.AsMap(skills[0])
	if got := shared.StringArg(first, "skillKey", ""); got != "it-infra-continuous-png" {
		t.Fatalf("expected bridge to preserve skillKey, got %#v", first)
	}
	if first["eligible"] != true {
		t.Fatalf("expected eligible OpenClaw skill, got %#v", first)
	}
	second := shared.AsMap(skills[1])
	if second["blockedByAgentFilter"] != true || second["disabled"] != true {
		t.Fatalf("expected disabled/blocked skill metadata to be preserved, got %#v", second)
	}
	if !slices.Contains(gateway.Methods(), "skills.status") {
		t.Fatalf("expected fake OpenClaw gateway to receive skills.status, got %#v", gateway.Methods())
	}
}

func TestGatewayRequestSkillsStatusAutoConnectsOpenClaw(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_IDENTITY_PATH", filepath.Join(t.TempDir(), "openclaw-device.json"))
	resetBridgeGatewayIdentityForTest()
	t.Cleanup(resetBridgeGatewayIdentityForTest)

	server := NewServer()
	response, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "xworkmate.gateway.request",
		Params: map[string]any{
			"runtimeId": "app-runtime",
			"method":    "skills.status",
			"params": map[string]any{
				"agentId": "main",
			},
			"timeoutMs": float64(15000),
		},
	}, nil)
	if rpcErr != nil {
		t.Fatalf("expected skills.status gateway response, got rpc error: %#v", rpcErr)
	}
	if response["ok"] != true {
		t.Fatalf("expected skills.status ok, got %#v", response)
	}
	payload := shared.AsMap(response["payload"])
	skills := shared.ListArg(payload, "skills")
	if len(skills) != 2 {
		t.Fatalf("expected two OpenClaw skill records, got %#v", payload["skills"])
	}
	if got := gateway.ConnectCount(); got != 1 {
		t.Fatalf("expected skills.status to auto-connect OpenClaw once, got %d", got)
	}
	if !slices.Contains(gateway.Methods(), "skills.status") {
		t.Fatalf("expected fake OpenClaw gateway to receive skills.status, got %#v", gateway.Methods())
	}
}

func TestExecuteSessionTaskGatewayFailsWhenPrepareUnsupported(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	gateway.unsupportedSessionPrepare.Store(true)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-legacy-prepare",
				"threadId":         "thread-openclaw-legacy-prepare",
				"taskPrompt":       "say pong",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr == nil {
		t.Fatalf("expected prepare error without legacy fallback, got response: %#v", response)
		return
	}
	if rpcErr.Code != -32002 || !strings.Contains(rpcErr.Message, "unknown method: xworkmate.session.prepare") {
		t.Fatalf("expected surfaced prepare unsupported error, got %#v", rpcErr)
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.session.prepare"}) {
		t.Fatalf("expected bridge to stop before chat.send when prepare is unsupported, got %#v", got)
	}
}

func TestExecuteSessionTaskGatewayNoDisplayableOutputFails(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-no-output",
				"threadId":         "thread-openclaw-no-output",
				"taskPrompt":       "completed-empty",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected structured no-output response, got rpc error: %#v", rpcErr)
	}
	if success, _ := response["success"].(bool); success {
		t.Fatalf("expected no-output gateway response to fail, got %#v", response)
	}
	if got := response["status"]; got != "failed" {
		t.Fatalf("expected failed status for no-output gateway response, got %#v", response)
	}
	if got := response["code"]; got != "OPENCLAW_NO_DISPLAYABLE_OUTPUT" {
		t.Fatalf("expected structured no-displayable code, got %#v", response)
	}
	if got := response["output"]; got != openClawNoDisplayableText {
		t.Fatalf("expected no-displayable output message, got %#v", response)
	}
	if gateway.ArtifactExportCount() != 0 {
		t.Fatalf("expected native task-registry failure to avoid Bridge artifact export sync, got %d", gateway.ArtifactExportCount())
	}
}

func TestExecuteSessionTaskGatewayFailsClosedWhenOpenClawAcceptsDifferentSession(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	gateway.alternateSessionKey = "dashboard:c061bfeb-ad08-45f5-971d-d9018f745d7a"
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "draft:1780669943199412-3",
				"threadId":         "draft:1780669943199412-3",
				"taskPrompt":       "say pong",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})

	if rpcErr == nil {
		t.Fatalf("expected OpenClaw session mismatch rpc error, got response %#v", response)
		return
	}
	if !strings.Contains(rpcErr.Message, "OPENCLAW_SESSION_MISMATCH") {
		t.Fatalf("expected structured session mismatch error, got %#v", rpcErr)
	}
	if gateway.AgentWaitCount() != 0 {
		t.Fatalf("session mismatch must fail before agent.wait, got %d waits", gateway.AgentWaitCount())
	}
	if gateway.ArtifactExportCount() != 0 {
		t.Fatalf("session mismatch must fail before artifact export, got %d exports", gateway.ArtifactExportCount())
	}
	chatParams := gateway.LastChatSendParams()
	if got := shared.StringArg(chatParams, "sessionKey", ""); got != "agent:main:draft:1780669943199412-3" {
		t.Fatalf("expected Bridge to request the app-mapped OpenClaw session, got %#v", chatParams)
	}
}

func TestExecuteSessionTaskGatewayFailsArtifactContractAfterWaitFailure(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()
	gateway.artifactWorkspaceRoot = t.TempDir()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-wait-recover",
				"threadId":         "thread-openclaw-wait-recover",
				"taskPrompt":       "wait-timeout",
				"workingDirectory": t.TempDir(),
				"metadata": map[string]any{
					"taskLoadClass":              "complex_long_chain_task",
					"expectedArtifactExtensions": []any{"pdf"},
				},
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected wait-timeout probe to keep task running, got rpc error: %#v", rpcErr)
	}
	if got := response["status"]; got != string(TaskStateRunning) {
		t.Fatalf("expected wait-timeout probe to keep running status, got %#v", response)
	}
	if got := gateway.ChatSendCount(); got != 1 {
		t.Fatalf("expected no automatic repair model turn, got %d", got)
	}
	if got := gateway.AgentWaitCount(); got != 0 {
		t.Fatalf("expected native task-registry lookup without Bridge-owned status probe, got %d", got)
	}
	if got := gateway.ArtifactExportCount(); got != 0 {
		t.Fatalf("expected no artifact export before terminal state, got %d", got)
	}
}

func TestExecuteSessionTaskGatewayKeepsRunningOnNonTerminalWaitPayload(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()
	gateway.artifactWorkspaceRoot = t.TempDir()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-running-wait",
				"threadId":         "thread-openclaw-running-wait",
				"taskPrompt":       "wait-running",
				"workingDirectory": t.TempDir(),
				"metadata": map[string]any{
					"taskLoadClass":              "complex_long_chain_task",
					"expectedArtifactExtensions": []any{"pdf"},
				},
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected running wait payload to keep task running, got rpc error: %#v", rpcErr)
	}
	if got := response["status"]; got != string(TaskStateRunning) {
		t.Fatalf("expected running status from non-terminal wait payload, got %#v", response)
	}
	if got := gateway.ChatSendCount(); got != 1 {
		t.Fatalf("expected no repair turn, got %d", got)
	}
	if got := gateway.AgentWaitCount(); got != 0 {
		t.Fatalf("expected native task-registry lookup without Bridge-owned status probe, got %d", got)
	}
	if got := gateway.ArtifactExportCount(); got != 0 {
		t.Fatalf("expected no artifact export before terminal wait payload, got %d", got)
	}
	if _, ok := response["code"]; ok {
		t.Fatalf("expected no terminal failure code, got %#v", response)
	}
}

func TestExecuteSessionTaskGatewayAgentFailedBeforeReplyReturnsFailureCode(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-agent-failed",
				"threadId":         "thread-openclaw-agent-failed",
				"taskPrompt":       "agent failed before reply",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected structured agent failure response, got rpc error: %#v", rpcErr)
	}
	if got := response["success"]; got != false {
		t.Fatalf("expected agent failure response to fail, got %#v", response)
	}
	if got := response["status"]; got != "failed" {
		t.Fatalf("expected failed status for agent failure, got %#v", response)
	}
	if got := response["code"]; got != "OPENCLAW_AGENT_FAILED_BEFORE_REPLY" {
		t.Fatalf("expected agent failure code, got %#v", response)
	}
	if got := shared.StringArg(response, "error", ""); !strings.Contains(got, "No available auth profile") {
		t.Fatalf("expected agent failure details, got %#v", response)
	}
}

func TestExecuteSessionMessageGatewayUsesOpenClawChatSend(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.message",
			Params: map[string]any{
				"sessionId":        "session-openclaw",
				"threadId":         "thread-openclaw",
				"taskPrompt":       "continue",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected gateway response, got rpc error: %#v", rpcErr)
	}
	if got := response["output"]; got != "gateway pong" {
		t.Fatalf("expected gateway pong output, got %#v", response)
	}
	if gateway.ChatSendCount() != 1 {
		t.Fatalf("expected one OpenClaw chat.send request, got %d", gateway.ChatSendCount())
	}
	if gateway.AgentWaitCount() != 0 {
		t.Fatalf("expected native task-registry lookup without Bridge-owned agent.wait, got %d", gateway.AgentWaitCount())
	}
	if gateway.ArtifactExportCount() != 0 {
		t.Fatalf("expected native task-registry lookup without Bridge artifact export sync, got %d", gateway.ArtifactExportCount())
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.session.prepare", "chat.send", "xworkmate.tasks.get"}) {
		t.Fatalf("expected connect, prepare, chat.send, then native task lookup, got %#v", got)
	}
}

func TestInternalJobsSubmitCompletesAndReportsStats(t *testing.T) {
	server := NewServer()
	providerServer := newExternalSingleAgentProvider(t, "opencode", "job-output")
	defer providerServer.Close()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID: "opencode",
		Label:      "OpenCode",
		Endpoint:   providerServer.URL,
		Enabled:    true,
	})

	submitted, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "xworkmate.jobs.submit",
		Params: map[string]any{
			"providerId":        "opencode",
			"sessionId":         "job-session",
			"threadId":          "job-thread",
			"taskPrompt":        "run async job",
			"workingDirectory":  t.TempDir(),
			"timeoutMs":         30_000,
			"orchestrationMode": "sequence",
		},
	}, nil)
	if rpcErr != nil {
		t.Fatalf("expected job submission, got %#v", rpcErr)
	}
	jobID := strings.TrimSpace(shared.StringArg(submitted, "jobId", ""))
	if jobID == "" {
		t.Fatalf("expected job id, got %#v", submitted)
	}

	var job map[string]any
	waitForCondition(t, func() bool {
		job, _ = server.handleJobMethod(context.Background(), "xworkmate.jobs.get", map[string]any{"jobId": jobID}, nil)
		return shared.StringArg(job, "status", "") == "completed"
	})
	result := shared.AsMap(job["result"])
	if got := strings.TrimSpace(shared.StringArg(result, "output", "")); got != "job-output" {
		t.Fatalf("expected job output, got %#v", job)
	}
	stats, rpcErr := server.handleJobMethod(context.Background(), "xworkmate.jobs.stats", nil, nil)
	if rpcErr != nil {
		t.Fatalf("expected stats, got %#v", rpcErr)
	}
	summary := shared.AsMap(stats["summary"])
	if got := fmt.Sprint(summary["completed"]); got != "1" {
		t.Fatalf("expected completed job stats, got %#v", stats)
	}
}

func TestInternalJobWebhookRetriesUntilSuccess(t *testing.T) {
	server := NewServer()
	providerServer := newExternalSingleAgentProvider(t, "opencode", "job-output")
	defer providerServer.Close()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID: "opencode",
		Label:      "OpenCode",
		Endpoint:   providerServer.URL,
		Enabled:    true,
	})
	var callbackCount atomic.Int32
	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := callbackCount.Add(1)
		if count == 1 {
			http.Error(w, "retry", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer callbackServer.Close()

	submitted, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "xworkmate.jobs.submit",
		Params: map[string]any{
			"providerId":       "opencode",
			"sessionId":        "job-webhook-session",
			"threadId":         "job-webhook-thread",
			"taskPrompt":       "run async job",
			"workingDirectory": t.TempDir(),
			"callbackUrl":      callbackServer.URL,
			"timeoutMs":        30_000,
		},
	}, nil)
	if rpcErr != nil {
		t.Fatalf("expected job submission, got %#v", rpcErr)
	}
	jobID := strings.TrimSpace(shared.StringArg(submitted, "jobId", ""))
	waitForCondition(t, func() bool {
		job, _ := server.handleJobMethod(context.Background(), "xworkmate.jobs.get", map[string]any{"jobId": jobID}, nil)
		return parseBool(job["webhookSent"]) && callbackCount.Load() >= 2
	})
}

func TestInternalToolsInvokeUsesOpenClawHTTPProxy(t *testing.T) {
	var received map[string]any
	toolsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tool-token" {
			t.Fatalf("expected bearer token, got %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode tool request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer toolsServer.Close()
	t.Setenv("OPENCLAW_TOOLS_INVOKE_URL", toolsServer.URL)
	t.Setenv("OPENCLAW_TOOLS_TOKEN", "tool-token")

	server := NewServer()
	response, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "xworkmate.tools.invoke",
		Params: map[string]any{
			"tool":   "message",
			"action": "send",
			"args": map[string]any{
				"target":  "channel:1",
				"message": "hello",
			},
		},
	}, nil)
	if rpcErr != nil {
		t.Fatalf("expected tools response, got %#v", rpcErr)
	}
	if !parseBool(response["ok"]) {
		t.Fatalf("expected ok tools response, got %#v", response)
	}
	if got := shared.StringArg(received, "tool", ""); got != "message" {
		t.Fatalf("expected message tool payload, got %#v", received)
	}
}

func TestExternalACPWebSocketAutoApprovesPermissionRequests(t *testing.T) {
	var sawApproval atomic.Bool
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()
		var request map[string]any
		if err := conn.ReadJSON(&request); err != nil {
			t.Fatalf("read request: %v", err)
		}
		requestID := request["id"]
		if err := conn.WriteJSON(map[string]any{
			"jsonrpc": "2.0",
			"id":      "perm-1",
			"method":  "session/request_permission",
			"params": map[string]any{
				"reason": "test permission",
			},
		}); err != nil {
			t.Fatalf("write permission request: %v", err)
		}
		var approval map[string]any
		if err := conn.ReadJSON(&approval); err != nil {
			t.Fatalf("read approval: %v", err)
		}
		if approval["id"] == "perm-1" && parseBool(shared.AsMap(approval["result"])["approved"]) {
			sawApproval.Store(true)
		}
		_ = conn.WriteJSON(map[string]any{
			"jsonrpc": "2.0",
			"id":      requestID,
			"result": map[string]any{
				"success": true,
				"output":  "approved output",
			},
		})
	}))
	defer wsServer.Close()

	compat := newProviderCompat(syncedProvider{
		ProviderID: "hermes",
		Label:      "Hermes",
		Endpoint:   "ws" + strings.TrimPrefix(wsServer.URL, "http") + "/acp",
		Enabled:    true,
	})
	result, err := compat.StartSession(context.Background(), "permission-session", "permission-thread", map[string]any{
		"taskPrompt": "needs permission",
	}, nil)
	if err != nil {
		t.Fatalf("expected permission auto approval, got %v", err)
	}
	if !sawApproval.Load() {
		t.Fatalf("expected permission approval response")
	}
	if got := shared.StringArg(result, "output", ""); got != "approved output" {
		t.Fatalf("expected approved output, got %#v", result)
	}
}

func TestStructuredExternalACPEventClassifiesNativeStreams(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{
			name: "thinking",
			raw: map[string]any{
				"method": "session.update",
				"params": map[string]any{"update": map[string]any{"thinking": "planning"}},
			},
			want: "thinking",
		},
		{
			name: "tool_call",
			raw: map[string]any{
				"method": "item/tool_call",
				"params": map[string]any{"item": map[string]any{"toolCall": map[string]any{"name": "read"}}},
			},
			want: "tool_call",
		},
		{
			name: "text",
			raw: map[string]any{
				"method": "session.update",
				"params": map[string]any{"update": map[string]any{"text": "hello"}},
			},
			want: "text",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := structuredExternalACPEvent(tc.raw)
			if got := shared.StringArg(event, "type", ""); got != tc.want {
				t.Fatalf("expected event type %q, got %#v", tc.want, event)
			}
		})
	}
}

func TestExecuteSessionTaskGatewaySurfacesOpenClawChatSendError(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-fail",
				"threadId":         "thread-openclaw-fail",
				"taskPrompt":       "fail",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr == nil {
		t.Fatalf("expected OpenClaw chat.send error, got response: %#v", response)
	} else if rpcErr.Code != -32002 || !strings.Contains(rpcErr.Message, "openclaw chat failed") {
		t.Fatalf("expected surfaced chat.send failure, got %#v", rpcErr)
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.session.prepare", "chat.send"}) {
		t.Fatalf("expected connect, artifact prepare, then chat.send, got %#v", got)
	}
}

func TestExecuteSessionTaskGatewayRetriesOpenClawChatSendSocketClose(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	gateway.closeNextChatSend.Store(true)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-retry",
				"threadId":         "thread-openclaw-retry",
				"taskPrompt":       "retry after socket close",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected retry success, got rpc error: %#v", rpcErr)
	}
	if got := response["output"]; got != "gateway pong" {
		t.Fatalf("expected gateway pong output after retry, got %#v", response)
	}
	if gateway.ConnectCount() != 2 {
		t.Fatalf("expected reconnect after socket close, got %d connects", gateway.ConnectCount())
	}
	if gateway.ChatSendCount() != 2 {
		t.Fatalf("expected chat.send to be retried once, got %d", gateway.ChatSendCount())
	}
}

func TestExecuteSessionTaskGatewayReturnsStructuredOpenClawSocketCloseAfterRetry(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	gateway.alwaysCloseChatSend.Store(true)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-retry-fail",
				"threadId":         "thread-openclaw-retry-fail",
				"taskPrompt":       "retry fails",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr == nil {
		t.Fatalf("expected OpenClaw socket close error, got response: %#v", response)
	} else if rpcErr.Code != -32002 || !strings.Contains(rpcErr.Message, "OPENCLAW_GATEWAY_SOCKET_CLOSED") {
		t.Fatalf("expected structured socket close failure, got %#v", rpcErr)
	}
	data := shared.AsMap(rpcErr.Data)
	if got := shared.StringArg(data, "code", ""); got != "OPENCLAW_GATEWAY_SOCKET_CLOSED" {
		t.Fatalf("expected socket close detail code, got %#v", rpcErr.Data)
	}
	if gateway.ChatSendCount() != 2 {
		t.Fatalf("expected one retry only, got %d chat.send attempts", gateway.ChatSendCount())
	}
}

func TestExecuteSessionTaskGatewaySurfacesOpenClawAgentWaitError(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-wait-fail",
				"threadId":         "thread-openclaw-wait-fail",
				"taskPrompt":       "wait-error",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected OpenClaw agent.wait error as task result, got rpc error: %#v", rpcErr)
	}
	if got := response["status"]; got != string(TaskStateFailed) {
		t.Fatalf("expected failed task result, got %#v", response)
	}
	if got := response["code"]; got != "OPENCLAW_WAIT_FAILED" {
		t.Fatalf("expected OpenClaw wait failure code, got %#v", response)
	}
	if got := shared.StringArg(response, "message", ""); !strings.Contains(got, "openclaw wait failed") {
		t.Fatalf("expected surfaced agent.wait failure, got %#v", response)
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.session.prepare", "chat.send", "xworkmate.tasks.get"}) {
		t.Fatalf("expected connect, prepare, chat.send, then native task lookup, got %#v", got)
	}
	snapshot := server.handleTaskGet(context.Background(), map[string]any{
		"appThreadKey":       "thread-openclaw-wait-fail",
		"openclawSessionKey": "agent:main:thread-openclaw-wait-fail",
		"runId":              shared.StringArg(response, "runId", ""),
	}, nil)
	if got := snapshot["status"]; got != string(TaskStateFailed) {
		t.Fatalf("expected failed session snapshot, got %#v from %#v", got, snapshot)
	}
	result := snapshot
	if got := result["success"]; got != false {
		t.Fatalf("expected failed result snapshot, got %#v", result)
	}
	if got := shared.StringArg(snapshot, "code", ""); got != "OPENCLAW_WAIT_FAILED" {
		t.Fatalf("expected OpenClaw wait code in snapshot, got %#v", result)
	}
	if got := shared.StringArg(snapshot, "message", ""); !strings.Contains(got, "openclaw wait failed") {
		t.Fatalf("expected OpenClaw wait message in snapshot, got %#v", result)
	}
}

func TestSessionCloseReturnsAcceptedAndClosedState(t *testing.T) {
	server := NewServer()
	sessionID := "session-close-contract"
	threadID := "thread-close-contract"
	_ = server.getOrCreateSession(sessionID, threadID)

	response, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "session.close",
		Params: map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
		},
	}, nil)
	if rpcErr != nil {
		t.Fatalf("expected close response, got rpc error: %#v", rpcErr)
	}
	if got := response["accepted"]; got != true {
		t.Fatalf("expected accepted close, got %#v", response)
	}
	if got := response["closed"]; got != true {
		t.Fatalf("expected closed=true for existing session, got %#v", response)
	}

	response, rpcErr = server.handleRequest(shared.RPCRequest{
		Method: "session.close",
		Params: map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
		},
	}, nil)
	if rpcErr != nil {
		t.Fatalf("expected idempotent close response, got rpc error: %#v", rpcErr)
	}
	if got := response["accepted"]; got != true {
		t.Fatalf("expected accepted idempotent close, got %#v", response)
	}
	if got := response["closed"]; got != false {
		t.Fatalf("expected closed=false for missing session, got %#v", response)
	}
}

func TestExecuteSessionTaskGatewayExportsOpenClawArtifacts(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-artifact",
				"threadId":         "thread-openclaw-artifact",
				"taskPrompt":       "make artifact",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected gateway response, got rpc error: %#v", rpcErr)
	}
	if gateway.ArtifactExportCount() != 0 {
		t.Fatalf("expected native task-registry artifact payload without Bridge export, got %d", gateway.ArtifactExportCount())
	}
	if got := response["remoteWorkingDirectory"]; got != "/remote/openclaw/workspace" {
		t.Fatalf("expected remote working directory from manifest, got %#v", response)
	}
	artifacts, ok := response["artifacts"].([]map[string]any)
	if !ok {
		raw, ok := response["artifacts"].([]any)
		if !ok {
			t.Fatalf("expected artifacts payload, got %#v", response["artifacts"])
		}
		artifacts = make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			artifacts = append(artifacts, shared.AsMap(item))
		}
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected one artifact from manifest, got %#v", artifacts)
	}
	if got := artifacts[0]["relativePath"]; got != "reports/final.md" {
		t.Fatalf("expected manifest artifact relative path, got %#v", artifacts[0])
	}
	if _, ok := artifacts[0]["encoding"]; ok {
		t.Fatalf("expected OpenClaw task response to omit inline artifact encoding, got %#v", artifacts[0])
	}
	if _, ok := artifacts[0]["content"]; ok {
		t.Fatalf("expected OpenClaw task response to omit inline artifact content, got %#v", artifacts[0])
	}
	downloadURL := strings.TrimSpace(shared.StringArg(artifacts[0], "downloadUrl", ""))
	if downloadURL == "" {
		t.Fatalf("expected bridge downloadUrl on artifact, got %#v", artifacts[0])
	}
	parsedDownloadURL, err := url.Parse(downloadURL)
	if err != nil {
		t.Fatalf("parse downloadUrl: %v", err)
	}
	if got := parsedDownloadURL.Path; got != openClawArtifactDownloadPath {
		t.Fatalf("expected bridge artifact download path, got %q from %q", got, downloadURL)
	}
	if got := parsedDownloadURL.Query().Get("sessionKey"); got != shared.StringArg(response, "openclawSessionKey", "") {
		t.Fatalf("expected mapped sessionKey in downloadUrl, got %q", got)
	}
	if got := parsedDownloadURL.Query().Get("relativePath"); got != "reports/final.md" {
		t.Fatalf("expected artifact relativePath in downloadUrl, got %q", got)
	}
	if artifactScope := parsedDownloadURL.Query().Get("artifactScope"); artifactScope != "tasks/"+shared.StringArg(response, "openclawSessionKey", "")+"/"+response["runId"].(string) {
		t.Fatalf("expected prepared artifact scope in downloadUrl, got %q", artifactScope)
	}
	if parsedDownloadURL.Query().Get("sig") == "" {
		t.Fatalf("expected signed downloadUrl, got %q", downloadURL)
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.session.prepare", "chat.send", "xworkmate.tasks.get"}) {
		t.Fatalf("expected connect, prepare, chat.send, then native task lookup, got %#v", got)
	}
}

func TestExecuteSessionTaskGatewayDoesNotTreatPromptTextAsArtifactContract(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-latest-artifact",
				"threadId":         "thread-openclaw-latest-artifact",
				"taskPrompt":       "检查 workspace 已有真实制品，输出 artifacts files download。不要生成新文件，只简短说明。",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected gateway response, got rpc error: %#v", rpcErr)
	}
	if got := response["success"]; got != true {
		t.Fatalf("expected prompt text not to be converted into bridge artifact failure, got %#v", response)
	}
	if _, ok := response["artifacts"]; ok {
		t.Fatalf("expected no stale artifacts when gateway exported none, got %#v", response["artifacts"])
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.session.prepare", "chat.send", "xworkmate.tasks.get"}) {
		t.Fatalf("expected connect, prepare, chat.send, then native task lookup, got %#v", got)
	}
}

func TestExecuteSessionTaskGatewayExportsWithActualOpenClawRunID(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	gateway.alternateRunID = "openclaw-run-actual"
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-actual-run",
				"threadId":         "thread-openclaw-actual-run",
				"taskPrompt":       "make artifact",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected gateway response, got rpc error: %#v", rpcErr)
	}
	if got := response["runId"]; got != "openclaw-run-actual" {
		t.Fatalf("expected response to keep actual OpenClaw runId, got %#v", response)
	}
	if gateway.ArtifactPrepareCount() != 2 {
		t.Fatalf("expected bridge to prepare initial turn scope and actual OpenClaw run scope, got %d", gateway.ArtifactPrepareCount())
	}
	artifacts, ok := response["artifacts"].([]map[string]any)
	if !ok {
		raw, rawOK := response["artifacts"].([]any)
		if !rawOK {
			t.Fatalf("expected actual-run artifact manifest, got %#v", response["artifacts"])
		}
		artifacts = make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			artifacts = append(artifacts, shared.AsMap(item))
		}
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected actual-run artifact manifest, got %#v", response["artifacts"])
	}
	downloadURL := strings.TrimSpace(shared.StringArg(artifacts[0], "downloadUrl", ""))
	parsedDownloadURL, err := url.Parse(downloadURL)
	if err != nil {
		t.Fatalf("parse downloadUrl: %v", err)
	}
	if got := parsedDownloadURL.Query().Get("runId"); got != "openclaw-run-actual" {
		t.Fatalf("expected download URL to use actual OpenClaw runId, got %q from %q", got, downloadURL)
	}
	if got := parsedDownloadURL.Query().Get("artifactScope"); got != "tasks/"+shared.StringArg(response, "openclawSessionKey", "")+"/openclaw-run-actual" {
		t.Fatalf("expected download URL to use actual OpenClaw artifact scope, got %q", got)
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.session.prepare", "chat.send", "xworkmate.session.prepare", "xworkmate.tasks.get"}) {
		t.Fatalf("expected bridge to reprepare actual OpenClaw run before native task lookup, got %#v", got)
	}
}

func TestExecuteSessionTaskGatewayDoesNotExportArtifactScopeDeclaredInOutput(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	gateway.artifactWorkspaceRoot = t.TempDir()
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	const sessionKey = "draft-1780110089528457-2"
	const actualRunID = "turn-1780353431540965793"
	actualScope := "tasks/draft_1780110089528457-2/" + actualRunID
	actualFile := filepath.Join(
		gateway.artifactWorkspaceRoot,
		filepath.FromSlash(actualScope),
		"AI_Agent_News_June_2_2026.md",
	)
	if err := os.MkdirAll(filepath.Dir(actualFile), 0o755); err != nil {
		t.Fatalf("create actual artifact dir: %v", err)
	}
	if err := os.WriteFile(actualFile, []byte("news artifact"), 0o644); err != nil {
		t.Fatalf("write actual artifact: %v", err)
	}
	gateway.alternateRunID = actualRunID

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        sessionKey,
				"threadId":         sessionKey,
				"taskPrompt":       "declare output artifact path",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected gateway response, got rpc error: %#v", rpcErr)
	}
	if got := response["success"]; got != true {
		t.Fatalf("expected text-only response to complete without adopting output-declared artifact, got %#v", response)
	}
	if got := gateway.ArtifactExportCount(); got != 0 {
		t.Fatalf("expected no Bridge export from output-declared artifact path, got %d", got)
	}
	if artifacts, ok := response["artifacts"]; ok {
		t.Fatalf("expected no artifact from output-declared path, got %#v", artifacts)
	}
}

func TestExecuteSessionTaskGatewayDoesNotExportDraftScopeVariant(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	gateway.artifactWorkspaceRoot = t.TempDir()
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	const sessionKey = "draft-1780110089528457-2"
	const actualRunID = "turn-1780353431540965793"
	actualScope := "tasks/draft_1780110089528457-2/" + actualRunID
	actualFile := filepath.Join(
		gateway.artifactWorkspaceRoot,
		filepath.FromSlash(actualScope),
		"AI_Agent_News_June_2_2026.md",
	)
	if err := os.MkdirAll(filepath.Dir(actualFile), 0o755); err != nil {
		t.Fatalf("create actual artifact dir: %v", err)
	}
	if err := os.WriteFile(actualFile, []byte("news artifact"), 0o644); err != nil {
		t.Fatalf("write actual artifact: %v", err)
	}
	gateway.alternateRunID = actualRunID

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        sessionKey,
				"threadId":         sessionKey,
				"taskPrompt":       "plain done",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected gateway response, got rpc error: %#v", rpcErr)
	}
	if got := response["success"]; got != true {
		t.Fatalf("expected text-only task to complete without adopting draft variant artifact, got %#v", response)
	}
	if got := gateway.ArtifactExportCount(); got != 0 {
		t.Fatalf("expected no Bridge export from draft scope variant, got %d", got)
	}
	if artifacts, ok := response["artifacts"]; ok {
		t.Fatalf("expected no artifact from draft scope variant, got %#v", artifacts)
	}
}

func TestExecuteSessionMessageGatewayDoesNotRewriteClaimedArtifactsWithoutGatewayFiles(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.message",
			Params: map[string]any{
				"sessionId":        "session-openclaw-claimed-artifact",
				"threadId":         "thread-openclaw-claimed-artifact",
				"taskPrompt":       "hi hallucinate-files",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected gateway response, got rpc error: %#v", rpcErr)
	}
	if got := response["success"]; got != true {
		t.Fatalf("expected bridge to preserve gateway terminal state, got %#v", response)
	}
	if output := strings.TrimSpace(shared.StringArg(response, "output", "")); !strings.Contains(output, "文件已就绪") {
		t.Fatalf("expected bridge not to rewrite gateway text output, got %q", output)
	}
	if gateway.ArtifactExportCount() != 0 {
		t.Fatalf("expected native task-registry lookup without Bridge artifact export, got %d", gateway.ArtifactExportCount())
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.session.prepare", "chat.send", "xworkmate.tasks.get"}) {
		t.Fatalf("expected connect, prepare, chat.send, then native task lookup, got %#v", got)
	}
}

func TestExecuteSessionMessageGatewayExportsArtifactsWithoutPromptHeuristic(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.message",
			Params: map[string]any{
				"sessionId":        "session-openclaw-message-artifact",
				"threadId":         "thread-openclaw-message-artifact",
				"workingDirectory": t.TempDir(),
				"messages": []any{
					map[string]any{
						"role":    "assistant",
						"content": "上一轮只是分析。",
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "text", "text": "继续，把这些内容 make artifact 输出为 Markdown 文件。"},
						},
					},
				},
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected message artifact response, got rpc error: %#v", rpcErr)
	}
	if got := response["success"]; got != true {
		t.Fatalf("expected artifact response success, got %#v", response)
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.session.prepare", "chat.send", "xworkmate.tasks.get"}) {
		t.Fatalf("expected connect, prepare, chat.send, then native task lookup, got %#v", got)
	}
}

func TestNormalizeResultStripsOpenClawInlineArtifactsAfterRecordNormalization(t *testing.T) {
	server := NewServer()
	orchestrator := NewSessionOrchestrator(server)
	sess := server.getOrCreateSession("session-openclaw-normalize", "thread-openclaw-normalize")
	response := orchestrator.normalizeResult(
		sess,
		map[string]any{
			"success": true,
			"output":  "created files",
			"artifacts": []any{
				map[string]any{
					"relativePath": "reports/final.md",
					"label":        "final.md",
					"contentType":  "text/markdown",
					"sizeBytes":    12,
					"sha256":       "fake-sha256",
					"downloadUrl":  "https://xworkmate-bridge.svc.plus/artifacts/openclaw/download?relativePath=reports%2Ffinal.md",
					"encoding":     "base64",
					"content":      "ZmluYWwgcmVwb3J0",
				},
			},
		},
		RoutingResult{
			TargetID:   "gateway",
			ProviderID: "gateway",
		},
		"turn-openclaw-normalize",
		map[string]any{},
	)

	artifacts, ok := response["artifacts"].([]map[string]any)
	if !ok {
		t.Fatalf("expected normalized artifacts, got %#v", response["artifacts"])
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected one artifact, got %#v", artifacts)
	}
	if _, ok := artifacts[0]["encoding"]; ok {
		t.Fatalf("expected normalized OpenClaw artifact to omit encoding, got %#v", artifacts[0])
	}
	if _, ok := artifacts[0]["content"]; ok {
		t.Fatalf("expected normalized OpenClaw artifact to omit content, got %#v", artifacts[0])
	}
	if got := strings.TrimSpace(shared.StringArg(artifacts[0], "downloadUrl", "")); got == "" {
		t.Fatalf("expected normalized artifact to keep downloadUrl, got %#v", artifacts[0])
	}
}

func TestOpenClawArtifactResponseUsesRequestRoutingProvider(t *testing.T) {
	result := map[string]any{
		"success":           true,
		"output":            "created files",
		"artifactScope":     "tasks/thread-openclaw/run-openclaw",
		"artifactDirectory": "/remote/openclaw/workspace/tasks/thread-openclaw/run-openclaw",
	}
	routing := RoutingResult{
		TargetID:   "gateway",
		ProviderID: "gateway",
	}
	params := map[string]any{
		"routing": map[string]any{
			"routingMode":                "explicit",
			"explicitExecutionTarget":    "gateway",
			"preferredGatewayProviderId": "openclaw",
		},
	}

	if !openClawArtifactResponse(result, routing, params) {
		t.Fatalf("expected scoped result with request routing to be treated as OpenClaw artifact response")
	}
	if got := openClawGatewayProviderForArtifacts(result, routing, params); got != "openclaw" {
		t.Fatalf("expected OpenClaw provider from request routing, got %q", got)
	}
}

func TestHTTPHandlerOpenClawArtifactDownloadReadsViaGateway(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	downloadURL := server.openClawArtifactDownloadURL(
		"thread-openclaw-artifact",
		"run-1",
		"tasks/thread-openclaw-artifact/run-1",
		"reports/final.md",
		time.Now(),
	)
	if downloadURL == "" {
		t.Fatal("expected signed download URL")
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, downloadURL, nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "final report" {
		t.Fatalf("expected artifact content from OpenClaw read, got %q", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/markdown" {
		t.Fatalf("expected content type from artifact metadata, got %q", got)
	}
	if gateway.ArtifactReadCount() != 1 {
		t.Fatalf("expected one OpenClaw artifact read request, got %d", gateway.ArtifactReadCount())
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.artifacts.read"}) {
		t.Fatalf("expected connect, then artifact read, got %#v", got)
	}
}

func TestHTTPHandlerOpenClawArtifactDownloadSupportsRangeResume(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	downloadURL := server.openClawArtifactDownloadURL(
		"thread-openclaw-artifact",
		"run-1",
		"tasks/thread-openclaw-artifact/run-1",
		"reports/final.md",
		time.Now(),
	)
	if downloadURL == "" {
		t.Fatal("expected signed download URL")
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, downloadURL, nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	request.Header.Set("Range", "bytes=6-")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "report" {
		t.Fatalf("expected resumed artifact content, got %q", got)
	}
	if got := recorder.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("expected byte ranges to be advertised, got %q", got)
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 6-11/12" {
		t.Fatalf("expected content range, got %q", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != "6" {
		t.Fatalf("expected partial content length, got %q", got)
	}
}

func TestHTTPHandlerOpenClawArtifactDownloadRejectsInvalidRange(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	downloadURL := server.openClawArtifactDownloadURL(
		"thread-openclaw-artifact",
		"run-1",
		"tasks/thread-openclaw-artifact/run-1",
		"reports/final.md",
		time.Now(),
	)
	if downloadURL == "" {
		t.Fatal("expected signed download URL")
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, downloadURL, nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	request.Header.Set("Range", "bytes=99-")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes */12" {
		t.Fatalf("expected unsatisfied content range, got %q", got)
	}
}

func TestHTTPHandlerOpenClawArtifactDownloadRetriesTransientReadFailure(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	gateway.FailNextArtifactReads(1)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	downloadURL := server.openClawArtifactDownloadURL(
		"thread-openclaw-artifact",
		"run-1",
		"tasks/thread-openclaw-artifact/run-1",
		"reports/final.md",
		time.Now(),
	)
	if downloadURL == "" {
		t.Fatal("expected signed download URL")
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, downloadURL, nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected retry to return 200, got %d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "final report" {
		t.Fatalf("expected artifact content from retry, got %q", got)
	}
	if gateway.ArtifactReadCount() != 2 {
		t.Fatalf("expected one failed read and one retry, got %d", gateway.ArtifactReadCount())
	}
}

func TestHTTPHandlerOpenClawArtifactDownloadReturnsArtifactMissing(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	downloadURL := server.openClawArtifactDownloadURL(
		"thread-openclaw-artifact",
		"run-1",
		"tasks/thread-openclaw-artifact/run-1",
		"missing.txt",
		time.Now(),
	)
	if downloadURL == "" {
		t.Fatal("expected signed download URL")
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, downloadURL, nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "artifact_missing") {
		t.Fatalf("expected artifact_missing response, got %q", recorder.Body.String())
	}
	if gateway.ArtifactReadCount() != 1 {
		t.Fatalf("expected one OpenClaw artifact read request, got %d", gateway.ArtifactReadCount())
	}
}

func TestHTTPHandlerOpenClawArtifactDownloadRequiresBearer(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	downloadURL := server.openClawArtifactDownloadURL(
		"thread-openclaw-artifact",
		"run-1",
		"tasks/thread-openclaw-artifact/run-1",
		"reports/final.md",
		time.Now(),
	)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, downloadURL, nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestHTTPHandlerOpenClawArtifactDownloadRejectsInvalidSignature(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	downloadURL := server.openClawArtifactDownloadURL(
		"thread-openclaw-artifact",
		"run-1",
		"tasks/thread-openclaw-artifact/run-1",
		"reports/final.md",
		time.Now(),
	)
	parsed, err := url.Parse(downloadURL)
	if err != nil {
		t.Fatalf("parse downloadUrl: %v", err)
	}
	query := parsed.Query()
	query.Set("sig", "bad")
	parsed.RawQuery = query.Encode()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, parsed.String(), nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestHTTPHandlerOpenClawArtifactDownloadRejectsExpiredSignature(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	values := url.Values{}
	expires := fmt.Sprintf("%d", time.Now().Add(-time.Minute).Unix())
	values.Set("sessionKey", "thread-openclaw-artifact")
	values.Set("runId", "run-1")
	values.Set("artifactScope", "tasks/thread-openclaw-artifact/run-1")
	values.Set("relativePath", "reports/final.md")
	values.Set("expires", expires)
	values.Set("sig", signOpenClawArtifactDownload(
		"thread-openclaw-artifact",
		"run-1",
		"tasks/thread-openclaw-artifact/run-1",
		"reports/final.md",
		expires,
	))

	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, openClawArtifactDownloadPath+"?"+values.Encode(), nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestHTTPHandlerOpenClawArtifactDownloadRejectsTraversalPath(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	values := url.Values{}
	values.Set("sessionKey", "thread-openclaw-artifact")
	values.Set("runId", "run-1")
	values.Set("relativePath", "../secret.txt")
	values.Set("expires", fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()))
	values.Set("sig", "irrelevant")

	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, openClawArtifactDownloadPath+"?"+values.Encode(), nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestHTTPHandlerOpenClawArtifactDownloadRejectsInvalidArtifactScope(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	values := url.Values{}
	values.Set("sessionKey", "thread-openclaw-artifact")
	values.Set("runId", "run-1")
	values.Set("artifactScope", "../outside")
	values.Set("relativePath", "reports/final.md")
	values.Set("expires", fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()))
	values.Set("sig", "irrelevant")

	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, openClawArtifactDownloadPath+"?"+values.Encode(), nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestOpenClawChatSendParamsPreservesRawPrompt(t *testing.T) {
	for _, prompt := range []string{
		"输出 PPT PDF docx 文件",
		"生成一张图片并返回制品",
		"render a video artifact for download",
		"write a csv dataset file",
	} {
		t.Run(prompt, func(t *testing.T) {
			chatParams, rpcErr := openClawChatSendParams(map[string]any{
				"threadId":   "thread-artifact-instructions",
				"taskPrompt": prompt,
			}, "turn-artifact-instructions")
			if rpcErr != nil {
				t.Fatalf("expected chat params, got rpc error: %#v", rpcErr)
			}
			message := strings.TrimSpace(shared.StringArg(chatParams, "message", ""))
			if message != prompt {
				t.Fatalf("expected bridge to preserve raw prompt without artifact instructions, got %q", message)
			}
		})
	}
}

func TestOpenClawChatSendParamsMaterializesInlineAttachments(t *testing.T) {
	workspace := t.TempDir()
	chatParams, rpcErr := openClawChatSendParams(map[string]any{
		"threadId":         "thread-attachments",
		"taskPrompt":       "inspect uploaded image",
		"workingDirectory": workspace,
		"attachments": []any{
			map[string]any{"name": "empty-placeholder.txt", "path": ""},
		},
		"inlineAttachments": []any{
			map[string]any{
				"name":     "prompt.png",
				"mimeType": "image/png",
				"content":  base64.StdEncoding.EncodeToString([]byte("image-bytes")),
			},
		},
	}, "turn-inline-attachments")
	if rpcErr != nil {
		t.Fatalf("expected chat params, got rpc error: %#v", rpcErr)
	}

	attachments := shared.ListArg(chatParams, "attachments")
	if len(attachments) != 1 {
		t.Fatalf("expected one materialized attachment, got %#v", attachments)
	}
	attachment := shared.AsMap(attachments[0])
	if got := shared.StringArg(attachment, "name", ""); got != "prompt.png" {
		t.Fatalf("expected materialized attachment name, got %#v", attachment)
	}
	path := shared.StringArg(attachment, "path", "")
	if path == "" {
		t.Fatalf("expected materialized attachment path, got %#v", attachment)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected materialized file to exist: %v", err)
	}
	if string(content) != "image-bytes" {
		t.Fatalf("expected materialized content, got %q", string(content))
	}
	if got := shared.StringArg(chatParams, "message", ""); !strings.Contains(got, path) {
		t.Fatalf("expected message to include materialized attachment path, got %q", got)
	}
	if _, ok := chatParams["inlineAttachments"]; ok {
		t.Fatalf("chat.send params must not forward raw inlineAttachments, got %#v", chatParams)
	}
}

func TestOpenClawChatSendParamsMaterializesInlineAttachmentsInRemoteHint(t *testing.T) {
	remoteWorkspace := t.TempDir()
	chatParams, rpcErr := openClawChatSendParams(map[string]any{
		"threadId":                   "thread-remote-attachments",
		"taskPrompt":                 "inspect uploaded file",
		"workingDirectory":           "/Users/local/.xworkmate/threads/thread-remote-attachments",
		"remoteWorkingDirectoryHint": remoteWorkspace,
		"inlineAttachments": []any{
			map[string]any{
				"name":     "note.txt",
				"mimeType": "text/plain",
				"content":  base64.StdEncoding.EncodeToString([]byte("note body")),
			},
		},
	}, "turn-remote-attachments")
	if rpcErr != nil {
		t.Fatalf("expected chat params, got rpc error: %#v", rpcErr)
	}

	attachments := shared.ListArg(chatParams, "attachments")
	if len(attachments) != 1 {
		t.Fatalf("expected one materialized attachment, got %#v", attachments)
	}
	path := shared.StringArg(shared.AsMap(attachments[0]), "path", "")
	if !strings.HasPrefix(path, remoteWorkspace) {
		t.Fatalf("expected attachment under remote workspace %q, got %q", remoteWorkspace, path)
	}
	if strings.Contains(path, "/Users/local/") {
		t.Fatalf("attachment path must not use desktop local workspace, got %q", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected materialized file to exist: %v", err)
	}
	if string(content) != "note body" {
		t.Fatalf("expected materialized content, got %q", string(content))
	}
}

func TestOpenClawChatSendParamsMapsOwnerScopedWorkspaceToWritableRoot(t *testing.T) {
	writableRoot := t.TempDir()
	t.Setenv("OPENCLAW_WRITABLE_WORKSPACE_ROOT", writableRoot)
	ownerWorkspace := "/owners/local/device/demo/threads/draft-1"
	params := withOpenClawWritableWorkspace(map[string]any{
		"sessionId":                  "draft-1",
		"threadId":                   "draft-1",
		"taskPrompt":                 "write into currentTaskWorkspace: " + ownerWorkspace,
		"workingDirectory":           ownerWorkspace,
		"remoteWorkingDirectoryHint": ownerWorkspace,
		"inlineAttachments": []any{
			map[string]any{
				"name":     "note.txt",
				"mimeType": "text/plain",
				"content":  base64.StdEncoding.EncodeToString([]byte("note body")),
			},
		},
	}, "draft-1")

	chatParams, rpcErr := openClawChatSendParams(params, "turn-owner-workspace")
	if rpcErr != nil {
		t.Fatalf("expected chat params, got rpc error: %#v", rpcErr)
	}
	writableWorkspace := filepath.Join(writableRoot, "draft-1")
	if got := shared.StringArg(params, "workingDirectory", ""); got != writableWorkspace {
		t.Fatalf("expected writable working directory %q, got %q", writableWorkspace, got)
	}
	if got := shared.StringArg(params, "remoteWorkingDirectoryHint", ""); got != writableWorkspace {
		t.Fatalf("expected writable remote hint %q, got %q", writableWorkspace, got)
	}
	message := shared.StringArg(chatParams, "message", "")
	if strings.Contains(message, "/owners/") {
		t.Fatalf("message must not reference owner-scoped workspace, got %q", message)
	}
	if !strings.Contains(message, writableWorkspace) {
		t.Fatalf("message should reference writable workspace %q, got %q", writableWorkspace, message)
	}
	path := shared.StringArg(shared.AsMap(shared.ListArg(chatParams, "attachments")[0]), "path", "")
	if !strings.HasPrefix(path, writableWorkspace) {
		t.Fatalf("expected materialized attachment under writable workspace %q, got %q", writableWorkspace, path)
	}
}

func TestOpenClawWritableWorkspaceDoesNotDefaultToLegacyTaskArtifacts(t *testing.T) {
	t.Setenv("OPENCLAW_WRITABLE_WORKSPACE_ROOT", "")

	params := withOpenClawWritableWorkspace(map[string]any{
		"sessionId":                  "draft-1",
		"threadId":                   "draft-1",
		"taskPrompt":                 "write into /owners/local/device/demo/threads/draft-1",
		"workingDirectory":           "/owners/local/device/demo/threads/draft-1",
		"remoteWorkingDirectoryHint": "/owners/local/device/demo/threads/draft-1",
	}, "draft-1")

	if got := shared.StringArg(params, "workingDirectory", ""); strings.Contains(got, "task_artifacts") {
		t.Fatalf("must not synthesize legacy task_artifacts workspace, got %q", got)
	}
	if got := shared.StringArg(params, "remoteWorkingDirectoryHint", ""); strings.Contains(got, "task_artifacts") {
		t.Fatalf("must not synthesize legacy task_artifacts remote hint, got %q", got)
	}
}

func TestOpenClawArtifactWorkspaceDirIgnoresOwnerScopedRemoteHint(t *testing.T) {
	t.Setenv("OPENCLAW_WORKSPACE_DIR", "")

	got := openClawArtifactWorkspaceDir(map[string]any{
		"workingDirectory":           "/owners/local/device/demo/threads/draft-1",
		"remoteWorkingDirectoryHint": "/owners/local/device/demo/threads/draft-1",
	})

	if got != "~/.openclaw/workspace" {
		t.Fatalf("expected default OpenClaw workspace root, got %q", got)
	}
}

func TestOpenClawPreparedArtifactWorkspaceRewritesOwnerPromptToArtifactDirectory(t *testing.T) {
	ownerWorkspace := "/owners/local/device/demo/threads/draft-1"
	artifactDirectory := "/home/ubuntu/.openclaw/workspace/tasks/agent-main-draft-1/turn-1"

	params := withOpenClawPreparedArtifactWorkspace(map[string]any{
		"taskPrompt": "" +
			"TaskThread workspace context:\n" +
			"- remoteWorkspaceHint: " + ownerWorkspace + "\n" +
			"- currentTaskWorkspace: " + ownerWorkspace + "\n" +
			"User request:\nwrite a report",
		"workingDirectory":           ownerWorkspace,
		"remoteWorkingDirectoryHint": ownerWorkspace,
		"metadata": map[string]any{
			"xworkmateTaskArtifactContract": map[string]any{
				"currentTaskWorkspace": ownerWorkspace,
				"remoteWorkspaceHint":  ownerWorkspace,
			},
		},
	}, &openClawPreparedArtifactScope{
		ArtifactDirectory: artifactDirectory,
		ArtifactScope:     "tasks/agent-main-draft-1/turn-1",
	})

	if got := shared.StringArg(params, "workingDirectory", ""); got != artifactDirectory {
		t.Fatalf("expected workingDirectory rewritten to artifact directory, got %q", got)
	}
	if got := shared.StringArg(params, "remoteWorkingDirectoryHint", ""); got != artifactDirectory {
		t.Fatalf("expected remote hint rewritten to artifact directory, got %q", got)
	}
	message := shared.StringArg(params, "taskPrompt", "")
	if strings.Contains(message, ownerWorkspace) {
		t.Fatalf("prompt must not keep owner-scoped workspace, got %q", message)
	}
	if count := strings.Count(message, artifactDirectory); count != 2 {
		t.Fatalf("expected artifact directory to replace both workspace prompt references, count=%d message=%q", count, message)
	}
}

func TestExecuteSessionTaskGatewayRejectsOversizedInlineAttachmentBeforeChatSend(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	gateway.artifactWorkspaceRoot = t.TempDir()
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	oversized := make([]byte, openClawInlineAttachmentMaxFileBytes+1)
	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-oversized-attachment",
				"threadId":         "thread-oversized-attachment",
				"taskPrompt":       "inspect attachment",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
				"inlineAttachments": []any{
					map[string]any{
						"name":     "too-large.bin",
						"mimeType": "application/octet-stream",
						"content":  base64.StdEncoding.EncodeToString(oversized),
					},
				},
			},
		},
	})
	if rpcErr == nil {
		t.Fatalf("expected oversized attachment rpc error, got response: %#v", response)
	} else if !strings.Contains(rpcErr.Message, "OPENCLAW_ATTACHMENT_FILE_TOO_LARGE") {
		t.Fatalf("expected attachment size error, got %#v", rpcErr)
	}
	if gateway.ChatSendCount() != 0 {
		t.Fatalf("oversized attachment must not reach chat.send, got %d", gateway.ChatSendCount())
	}
}

func TestExecuteSessionTaskGatewayCollectsOpenClawEventArtifacts(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	gateway.artifactMode = "unknown"
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-event-artifact",
				"threadId":         "thread-openclaw-event-artifact",
				"taskPrompt":       "event artifact",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected gateway response, got rpc error: %#v", rpcErr)
	}
	artifacts, ok := response["artifacts"].([]map[string]any)
	if !ok {
		raw, ok := response["artifacts"].([]any)
		if !ok {
			t.Fatalf("expected artifacts payload, got %#v", response["artifacts"])
		}
		artifacts = make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			artifacts = append(artifacts, shared.AsMap(item))
		}
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected one event artifact, got %#v", artifacts)
	}
	if got := artifacts[0]["relativePath"]; got != "events/live.txt" {
		t.Fatalf("expected event artifact relative path, got %#v", artifacts[0])
	}
	if got := response["remoteWorkingDirectory"]; got != "/remote/openclaw/events" {
		t.Fatalf("expected remote working directory from event, got %#v", response)
	}
}

func TestExecuteSessionTaskGatewayAlwaysSyncsGatewayArtifactsAfterRun(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-artifact-missing",
				"threadId":         "thread-openclaw-artifact-missing",
				"taskPrompt":       "say pong",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":                "explicit",
					"explicitExecutionTarget":    "gateway",
					"preferredGatewayProviderId": "openclaw",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected gateway text response despite artifact export failure, got rpc error: %#v", rpcErr)
	}
	if got := response["output"]; got != "gateway pong" {
		t.Fatalf("expected gateway pong output, got %#v", response)
	}
	if gateway.ArtifactExportCount() != 0 {
		t.Fatalf("expected native task-registry lookup without Bridge artifact export sync, got %d", gateway.ArtifactExportCount())
	}
	if warnings := shared.ListArg(response, "artifactWarnings"); len(warnings) != 0 {
		t.Fatalf("expected no artifact warnings when gateway export succeeds empty, got %#v", warnings)
	}
}

func TestExtractArtifactPayloadsDoesNotScanRemoteDirectoryFallback(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	artifacts := extractArtifactPayloads(map[string]any{
		"remoteWorkingDirectory": root,
		"artifacts":              []any{},
	}, root)
	if len(artifacts) != 0 {
		t.Fatalf("expected no directory fallback artifacts, got %#v", artifacts)
	}
}

func TestExecuteSessionTaskDefaultsExplicitGatewayToOpenClaw(t *testing.T) {
	server := NewServer()

	_, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":  "session-gateway-missing-provider",
				"threadId":   "thread-gateway-missing-provider",
				"taskPrompt": "search latest news",
				"routing": map[string]any{
					"routingMode":             "explicit",
					"explicitExecutionTarget": "gateway",
				},
			},
		},
	})
	if rpcErr == nil {
		t.Fatal("expected gateway connectivity error")
	} else if rpcErr.Message == "GATEWAY_PROVIDER_REQUIRED" {
		t.Fatalf("expected openclaw default from routing result, got %#v", rpcErr)
	}
}

func TestExtractArtifactPayloadsPreservesDownloadURLOnlyArtifacts(t *testing.T) {
	artifacts := extractArtifactPayloads(map[string]any{
		"artifacts": []any{
			map[string]any{
				"name":        "reports/final.txt",
				"downloadURL": "https://xworkmate-bridge.svc.plus/artifacts/final.txt",
			},
			map[string]any{
				"download_url": "https://xworkmate-bridge.svc.plus/artifacts/from-url.md",
			},
		},
	}, "")

	if len(artifacts) != 2 {
		t.Fatalf("expected two artifacts, got %#v", artifacts)
	}
	if got := artifacts[0]["relativePath"]; got != "reports/final.txt" {
		t.Fatalf("expected name-derived path, got %#v", got)
	}
	if got := artifacts[0]["downloadUrl"]; got != "https://xworkmate-bridge.svc.plus/artifacts/final.txt" {
		t.Fatalf("expected normalized downloadUrl, got %#v", got)
	}
	if _, ok := artifacts[0]["downloadURL"]; ok {
		t.Fatalf("expected downloadURL alias to be removed: %#v", artifacts[0])
	}
	if got := artifacts[1]["relativePath"]; got != "from-url.md" {
		t.Fatalf("expected URL basename path, got %#v", got)
	}
	if got := artifacts[1]["contentType"]; got != "text/plain" {
		t.Fatalf("expected markdown content type, got %#v", got)
	}
}

func TestExtractArtifactPayloadsRejectsUnsafeDownloadURLArtifactNames(t *testing.T) {
	artifacts := extractArtifactPayloads(map[string]any{
		"artifacts": []any{
			map[string]any{
				"name":        "../secrets.txt",
				"downloadUrl": "https://xworkmate-bridge.svc.plus/artifacts/secrets.txt",
			},
		},
	}, "")

	if len(artifacts) != 0 {
		t.Fatalf("expected unsafe artifact to be dropped, got %#v", artifacts)
	}
}

type acpFakeOpenClawGateway struct {
	server                     *http.Server
	listener                   net.Listener
	connectCount               atomic.Int32
	chatSendCount              atomic.Int32
	agentWaitCount             atomic.Int32
	artifactPrepareCount       atomic.Int32
	artifactSnapshotCount      atomic.Int32
	artifactCount              atomic.Int32
	artifactReadCount          atomic.Int32
	artifactReadFailures       atomic.Int32
	closeNextChatSend          atomic.Bool
	alwaysCloseChatSend        atomic.Bool
	agentWaitDelayMs           atomic.Int64
	largeGatewayPayloadBytes   atomic.Int64
	emitAgentDelta             atomic.Bool
	lastConnectClient          atomic.Value
	lastChatSendParams         atomic.Value
	lastArtifactPrepareParams  atomic.Value
	lastArtifactSnapshotParams atomic.Value
	lastArtifactExportParams   atomic.Value
	lastAgentWaitParams        atomic.Value
	mu                         sync.Mutex
	methods                    []string
	runMessages                map[string]string
	artifactMode               string
	artifactWorkspaceRoot      string
	alternateRunID             string
	alternateSessionKey        string
	unsupportedSessionPrepare  atomic.Bool
}

func newAcpFakeOpenClawGateway(t *testing.T) *acpFakeOpenClawGateway {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake openclaw gateway: %v", err)
	}
	fake := &acpFakeOpenClawGateway{listener: listener, runMessages: map[string]string{}}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() {
			_ = conn.Close()
		}()
		_ = conn.WriteJSON(map[string]any{
			"type":  "event",
			"event": "connect.challenge",
			"payload": map[string]any{
				"nonce": "nonce-1",
			},
		})
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame map[string]any
			if err := json.Unmarshal(payload, &frame); err != nil {
				continue
			}
			if strings.TrimSpace(shared.StringArg(frame, "type", "")) != "req" {
				continue
			}
			id := frame["id"]
			method := strings.TrimSpace(shared.StringArg(frame, "method", ""))
			fake.recordMethod(method)
			switch method {
			case "connect":
				fake.connectCount.Add(1)
				params := shared.AsMap(frame["params"])
				device := shared.AsMap(params["device"])
				publicKey, err := base64.RawURLEncoding.DecodeString(
					strings.TrimSpace(shared.StringArg(device, "publicKey", "")),
				)
				sum := sha256.Sum256(publicKey)
				if err != nil || shared.StringArg(device, "id", "") != hex.EncodeToString(sum[:]) {
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   false,
						"error": map[string]any{
							"code":    "INVALID_REQUEST",
							"message": "device identity mismatch",
							"details": map[string]any{
								"code": "DEVICE_AUTH_DEVICE_ID_MISMATCH",
							},
						},
					})
					return
				}
				if got, want := shared.StringArg(shared.AsMap(params["auth"]), "token", ""), os.Getenv("BRIDGE_AUTH_TOKEN"); got != want {
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   false,
						"error": map[string]any{
							"code":    "INVALID_REQUEST",
							"message": "unauthorized: gateway token mismatch",
						},
					})
					return
				}
				fake.lastConnectClient.Store(shared.AsMap(params["client"]))
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   true,
					"payload": map[string]any{
						"server": map[string]any{"host": "127.0.0.1"},
						"snapshot": map[string]any{
							"sessionDefaults": map[string]any{"mainSessionKey": "main"},
						},
						"auth": map[string]any{
							"role":        "operator",
							"scopes":      []string{"operator.read", "operator.write"},
							"deviceToken": "device-token-1",
						},
					},
				})
			case "chat.send":
				fake.chatSendCount.Add(1)
				if fake.alwaysCloseChatSend.Load() || fake.closeNextChatSend.Swap(false) {
					_ = conn.Close()
					return
				}
				params := shared.AsMap(frame["params"])
				fake.lastChatSendParams.Store(params)
				if strings.TrimSpace(shared.StringArg(params, "message", "")) == "fail" {
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   false,
						"error": map[string]any{
							"code":    "OPENCLAW_CHAT_FAILED",
							"message": "openclaw chat failed",
						},
					})
					continue
				}
				runID := strings.TrimSpace(shared.StringArg(params, "idempotencyKey", "fake-run"))
				if strings.TrimSpace(fake.alternateRunID) != "" {
					runID = strings.TrimSpace(fake.alternateRunID)
				}
				sessionKey := strings.TrimSpace(shared.StringArg(params, "sessionKey", ""))
				if strings.TrimSpace(fake.alternateSessionKey) != "" {
					sessionKey = strings.TrimSpace(fake.alternateSessionKey)
				}
				message := strings.TrimSpace(shared.StringArg(params, "message", ""))
				fake.recordRunMessage(runID, message)
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   true,
					"payload": map[string]any{
						"runId":      runID,
						"sessionKey": sessionKey,
						"status":     "started",
					},
				})
			case "xworkmate.session.prepare":
				fake.artifactPrepareCount.Add(1)
				params := shared.AsMap(frame["params"])
				fake.lastArtifactPrepareParams.Store(params)
				if fake.unsupportedSessionPrepare.Load() {
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   false,
						"error": map[string]any{
							"code":    "INVALID_REQUEST",
							"message": "unknown method: xworkmate.session.prepare",
						},
					})
					continue
				}
				runID := strings.TrimSpace(shared.StringArg(params, "runId", "fake-run"))
				sessionKey := strings.TrimSpace(shared.StringArg(params, "openclawSessionKey", "main"))
				artifactScope := "tasks/" + sessionKey + "/" + runID
				workspaceRoot := "/remote/openclaw/workspace"
				if strings.TrimSpace(fake.artifactWorkspaceRoot) != "" {
					workspaceRoot = strings.TrimSpace(fake.artifactWorkspaceRoot)
				}
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   true,
					"payload": map[string]any{
						"runId":                     runID,
						"sessionKey":                sessionKey,
						"openclawSessionKey":        sessionKey,
						"appThreadKey":              strings.TrimSpace(shared.StringArg(params, "appThreadKey", "")),
						"mapping":                   map[string]any{"schemaVersion": 1, "appThreadKey": strings.TrimSpace(shared.StringArg(params, "appThreadKey", "")), "openclawSessionKey": sessionKey, "expectedArtifactDirs": shared.ListArg(params, "expectedArtifactDirs")},
						"expectedArtifactDirs":      shared.ListArg(params, "expectedArtifactDirs"),
						"remoteWorkingDirectory":    workspaceRoot,
						"remoteWorkspaceRefKind":    "remotePath",
						"artifactScope":             artifactScope,
						"scopeKind":                 "task",
						"artifactDirectory":         filepath.Join(workspaceRoot, filepath.FromSlash(artifactScope)),
						"relativeArtifactDirectory": artifactScope,
						"warnings":                  []any{},
					},
				})
			case "agent.wait":
				fake.agentWaitCount.Add(1)
				if delayMs := fake.agentWaitDelayMs.Load(); delayMs > 0 {
					time.Sleep(time.Duration(delayMs) * time.Millisecond)
				}
				params := shared.AsMap(frame["params"])
				fake.lastAgentWaitParams.Store(params)
				runID := strings.TrimSpace(shared.StringArg(params, "runId", "fake-run"))
				switch fake.runMessage(runID) {
				case "wait-error":
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   false,
						"error": map[string]any{
							"code":    "OPENCLAW_WAIT_FAILED",
							"message": "openclaw wait failed",
						},
					})
					continue
				case "wait-timeout":
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   false,
						"error": map[string]any{
							"code":    "TIMEOUT",
							"message": "openclaw wait timeout",
						},
					})
					continue
				case "wait-running":
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   true,
						"payload": map[string]any{
							"runId":    runID,
							"status":   "running",
							"terminal": false,
						},
					})
					continue
				case "completed-empty":
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   true,
						"payload": map[string]any{
							"runId":  runID,
							"status": "completed",
						},
					})
					continue
				}
				message := "gateway pong"
				if strings.Contains(fake.runMessage(runID), "hallucinate-files") {
					message = "文件已就绪，点击直接下载👇 三个格式一键收取："
				}
				if strings.Contains(fake.runMessage(runID), "declare output artifact path") {
					message = "I've saved the requested file at `/remote/openclaw/workspace/tasks/draft_1780110089528457-2/" + runID + "/AI_Agent_News_June_2_2026.md`"
				}
				if strings.Contains(fake.runMessage(runID), "agent failed before reply") {
					message = "Agent failed before reply: No available auth profile for nvidia"
				}
				emitChatEvent := !strings.Contains(fake.runMessage(runID), "silent-turn")
				if payloadBytes := fake.largeGatewayPayloadBytes.Load(); payloadBytes > 0 {
					_ = conn.WriteJSON(map[string]any{
						"type":  "event",
						"event": "health",
						"seq":   1,
						"payload": map[string]any{
							"status": "ok",
							"blob":   strings.Repeat("x", int(payloadBytes)),
						},
					})
				}
				if fake.emitAgentDelta.Load() {
					_ = conn.WriteJSON(map[string]any{
						"type":  "event",
						"event": "agent",
						"seq":   2,
						"payload": map[string]any{
							"runId":        runID,
							"sessionKey":   "main",
							"stream":       "assistant",
							"data":         map[string]any{"text": "streamed delta"},
							"largeIgnored": strings.Repeat("y", 1024),
						},
					})
				}
				if emitChatEvent {
					_ = conn.WriteJSON(map[string]any{
						"type":  "event",
						"event": "chat",
						"seq":   3,
						"payload": map[string]any{
							"runId": runID,
							"state": "final",
							"message": map[string]any{
								"role":    "assistant",
								"content": message,
							},
						},
					})
				}
				if strings.Contains(fake.runMessage(runID), "event artifact") {
					_ = conn.WriteJSON(map[string]any{
						"type":  "event",
						"event": "chat",
						"seq":   2,
						"payload": map[string]any{
							"runId":                  runID,
							"state":                  "final",
							"remoteWorkingDirectory": "/remote/openclaw/events",
							"remoteWorkspaceRefKind": "remotePath",
							"artifacts": []any{
								map[string]any{
									"relativePath": "events/live.txt",
									"contentType":  "text/plain",
									"encoding":     "base64",
									"content":      "bGl2ZSBldmVudA==",
								},
							},
						},
					})
				}
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   true,
					"payload": map[string]any{
						"runId":  runID,
						"status": "ok",
					},
				})
			case "xworkmate.tasks.get":
				params := shared.AsMap(frame["params"])
				runID := strings.TrimSpace(shared.StringArg(params, "runId", ""))
				sessionKey := strings.TrimSpace(shared.StringArg(params, "openclawSessionKey", ""))
				appThreadKey := strings.TrimSpace(shared.StringArg(params, "appThreadKey", ""))
				if runID == "" || sessionKey == "" {
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   true,
						"payload": map[string]any{
							"ok":      false,
							"code":    "invalid_lookup",
							"status":  "not_found",
							"message": "openclawSessionKey and runId required",
						},
					})
					continue
				}
				status := "completed"
				taskStatus := "succeeded"
				success := true
				message := "gateway pong"
				code := ""
				errorMessage := ""
				runMessage := fake.runMessage(runID)
				lowerRunMessage := strings.ToLower(runMessage)
				switch {
				case strings.Contains(runMessage, "wait-error"):
					status = "failed"
					taskStatus = "failed"
					success = false
					message = "openclaw wait failed"
					code = "OPENCLAW_WAIT_FAILED"
					errorMessage = "openclaw wait failed"
				case strings.Contains(runMessage, "wait-timeout"), strings.Contains(runMessage, "wait-running"):
					status = "running"
					taskStatus = "running"
					message = ""
				case strings.Contains(runMessage, "completed-empty"):
					status = "failed"
					taskStatus = "failed"
					success = false
					message = openClawNoDisplayableText
					code = "OPENCLAW_NO_DISPLAYABLE_OUTPUT"
					errorMessage = openClawNoDisplayableText
				case strings.Contains(runMessage, "agent failed before reply"):
					status = "failed"
					taskStatus = "failed"
					success = false
					message = "Agent failed before reply: No available auth profile for nvidia"
					code = "OPENCLAW_AGENT_FAILED_BEFORE_REPLY"
					errorMessage = message
				case strings.Contains(runMessage, "hallucinate-files"):
					message = "文件已就绪，点击直接下载👇 三个格式一键收取："
				}
				artifactScope := "tasks/" + sessionKey + "/" + runID
				artifacts := []any{}
				hallucinatedFiles := strings.Contains(runMessage, "hallucinate-files")
				downloadURL := func(relativePath string) string {
					query := url.Values{}
					query.Set("sessionKey", sessionKey)
					query.Set("runId", runID)
					query.Set("artifactScope", artifactScope)
					query.Set("relativePath", relativePath)
					query.Set("sig", "fake-signature")
					return openClawArtifactDownloadPath + "?" + query.Encode()
				}
				if strings.Contains(runMessage, "make artifact") {
					artifacts = append(artifacts, map[string]any{
						"relativePath":  "reports/final.md",
						"label":         "final.md",
						"contentType":   "text/markdown",
						"sizeBytes":     12,
						"sha256":        "fake-sha256",
						"artifactScope": artifactScope,
						"scopeKind":     "task",
						"downloadUrl":   downloadURL("reports/final.md"),
					})
				}
				if strings.Contains(runMessage, "make pdf artifact") {
					artifacts = append(artifacts, map[string]any{
						"relativePath":  "exports/final.pdf",
						"label":         "final.pdf",
						"contentType":   "application/pdf",
						"sizeBytes":     12,
						"sha256":        "fake-sha256",
						"artifactScope": artifactScope,
						"scopeKind":     "task",
						"downloadUrl":   downloadURL("exports/final.pdf"),
					})
				}
				if strings.Contains(runMessage, "make partial artifact") {
					artifacts = append(artifacts, map[string]any{
						"relativePath":  "chapters/intro.md",
						"label":         "intro.md",
						"contentType":   "text/markdown",
						"sizeBytes":     12,
						"sha256":        "fake-sha256",
						"artifactScope": artifactScope,
						"scopeKind":     "task",
						"downloadUrl":   downloadURL("chapters/intro.md"),
					})
				}
				if !hallucinatedFiles && (strings.Contains(runMessage, "7张") || strings.Contains(runMessage, "图片") || strings.Contains(lowerRunMessage, "image")) {
					artifacts = append(artifacts, map[string]any{
						"relativePath":  "artifacts/media/browser/series-01.png",
						"label":         "series-01.png",
						"contentType":   "image/png",
						"sizeBytes":     12,
						"sha256":        "fake-sha256",
						"artifactScope": artifactScope,
						"scopeKind":     "task",
						"downloadUrl":   downloadURL("artifacts/media/browser/series-01.png"),
					})
				}
				if !hallucinatedFiles && !strings.Contains(runMessage, "make pdf artifact") && strings.Contains(lowerRunMessage, "pdf") {
					artifacts = append(artifacts, map[string]any{
						"relativePath":  "artifacts/tmp-openclaw/final.pdf",
						"label":         "final.pdf",
						"contentType":   "application/pdf",
						"sizeBytes":     12,
						"sha256":        "fake-sha256",
						"artifactScope": artifactScope,
						"scopeKind":     "task",
						"downloadUrl":   downloadURL("artifacts/tmp-openclaw/final.pdf"),
					})
				}
				if !hallucinatedFiles && (strings.Contains(runMessage, "视频") || strings.Contains(lowerRunMessage, "video")) {
					artifacts = append(artifacts, map[string]any{
						"relativePath":  "artifacts/tmp-openclaw/final.mp4",
						"label":         "final.mp4",
						"contentType":   "video/mp4",
						"sizeBytes":     12,
						"sha256":        "fake-sha256",
						"artifactScope": artifactScope,
						"scopeKind":     "task",
						"downloadUrl":   downloadURL("artifacts/tmp-openclaw/final.mp4"),
					})
				}
				if strings.Contains(runMessage, "event artifact") {
					artifacts = append(artifacts, map[string]any{
						"relativePath": "events/live.txt",
						"label":        "live.txt",
						"contentType":  "text/plain",
						"sizeBytes":    12,
						"sha256":       "fake-sha256",
						"downloadUrl":  downloadURL("events/live.txt"),
					})
				}
				if strings.TrimSpace(fake.artifactWorkspaceRoot) != "" {
					artifacts = appendArtifactList(artifacts, fake.exportFilesystemArtifacts(artifactScope))
				}
				remoteWorkingDirectory := "/remote/openclaw/workspace"
				if strings.TrimSpace(fake.artifactWorkspaceRoot) != "" {
					remoteWorkingDirectory = strings.TrimSpace(fake.artifactWorkspaceRoot)
				}
				if strings.Contains(runMessage, "event artifact") {
					remoteWorkingDirectory = "/remote/openclaw/events"
				}
				payload := map[string]any{
					"success":                   success,
					"status":                    status,
					"taskStatus":                taskStatus,
					"mode":                      "gateway-chat",
					"resolvedGatewayProviderId": "openclaw",
					"runId":                     runID,
					"taskId":                    runID,
					"appThreadKey":              appThreadKey,
					"openclawSessionKey":        sessionKey,
					"message":                   message,
					"output":                    message,
					"summary":                   message,
					"remoteWorkingDirectory":    remoteWorkingDirectory,
					"remoteWorkspaceRefKind":    "remotePath",
					"task": map[string]any{
						"taskId": runID,
						"runId":  runID,
						"status": taskStatus,
					},
					"warnings": []any{},
				}
				if code != "" {
					payload["code"] = code
				}
				if errorMessage != "" {
					payload["error"] = errorMessage
				}
				if len(artifacts) > 0 {
					payload["artifacts"] = artifacts
				}
				_ = conn.WriteJSON(map[string]any{
					"type":    "res",
					"id":      id,
					"ok":      true,
					"payload": payload,
				})
			case "xworkmate.artifacts.collect-and-snapshot":
				fake.artifactSnapshotCount.Add(1)
				params := shared.AsMap(frame["params"])
				fake.lastArtifactSnapshotParams.Store(params)
				runID := strings.TrimSpace(shared.StringArg(params, "runId", "fake-run"))
				sessionKey := strings.TrimSpace(shared.StringArg(params, "openclawSessionKey", ""))
				artifactScope := strings.TrimSpace(shared.StringArg(params, "artifactScope", ""))
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   true,
					"payload": map[string]any{
						"runId":                  runID,
						"sessionKey":             sessionKey,
						"remoteWorkingDirectory": "/remote/openclaw/workspace",
						"remoteWorkspaceRefKind": "remotePath",
						"artifactScope":          artifactScope,
						"scopeKind":              "task",
						"copiedFiles":            []any{},
						"warnings":               []any{},
					},
				})
			case "xworkmate.artifacts.export":
				fake.artifactCount.Add(1)
				if fake.artifactMode == "unknown" {
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   false,
						"error": map[string]any{
							"code":    "UNKNOWN_METHOD",
							"message": "unknown method: xworkmate.artifacts.export",
						},
					})
					continue
				}
				params := shared.AsMap(frame["params"])
				fake.lastArtifactExportParams.Store(params)
				runID := strings.TrimSpace(shared.StringArg(params, "runId", "fake-run"))
				sessionKey := strings.TrimSpace(shared.StringArg(params, "openclawSessionKey", ""))
				artifactScope := strings.TrimSpace(shared.StringArg(params, "artifactScope", ""))
				payload := map[string]any{
					"runId":                  runID,
					"sessionKey":             sessionKey,
					"remoteWorkingDirectory": "/remote/openclaw/workspace",
					"remoteWorkspaceRefKind": "remotePath",
					"scopeKind":              "workspace",
					"artifacts":              []any{},
					"warnings":               []any{},
				}
				if artifactScope != "" {
					payload["artifactScope"] = artifactScope
					payload["scopeKind"] = "task"
				}
				filesystemArtifacts := []any{}
				if strings.TrimSpace(fake.artifactWorkspaceRoot) != "" && artifactScope != "" {
					payload["remoteWorkingDirectory"] = strings.TrimSpace(fake.artifactWorkspaceRoot)
					if sessionKey != "" {
						filesystemArtifacts = fake.exportFilesystemArtifacts(artifactScope)
					}
				}
				if strings.Contains(fake.runMessage(runID), "make artifact") {
					payload["artifacts"] = []any{
						map[string]any{
							"relativePath":  "reports/final.md",
							"label":         "final.md",
							"contentType":   "text/markdown",
							"sizeBytes":     12,
							"sha256":        "fake-sha256",
							"artifactScope": artifactScope,
							"scopeKind":     "task",
							"encoding":      "base64",
							"content":       "ZmluYWwgcmVwb3J0",
						},
					}
				}
				if strings.Contains(fake.runMessage(runID), "make pdf artifact") {
					payload["artifacts"] = []any{
						map[string]any{
							"relativePath":  "exports/final.pdf",
							"label":         "final.pdf",
							"contentType":   "application/pdf",
							"sizeBytes":     12,
							"sha256":        "fake-sha256",
							"artifactScope": artifactScope,
							"scopeKind":     "task",
						},
					}
				}
				if strings.Contains(fake.runMessage(runID), "make partial artifact") {
					payload["artifacts"] = []any{
						map[string]any{
							"relativePath":  "chapters/intro.md",
							"label":         "intro.md",
							"contentType":   "text/markdown",
							"sizeBytes":     12,
							"sha256":        "fake-sha256",
							"artifactScope": artifactScope,
							"scopeKind":     "task",
						},
					}
				}
				runMessage := fake.runMessage(runID)
				lowerRunMessage := strings.ToLower(runMessage)
				hallucinatedFiles := strings.Contains(runMessage, "hallucinate-files")
				if !hallucinatedFiles && (strings.Contains(runMessage, "7张") || strings.Contains(runMessage, "图片") || strings.Contains(lowerRunMessage, "image")) {
					payload["artifacts"] = appendArtifactList(payload["artifacts"], []any{map[string]any{
						"relativePath":  "artifacts/media/browser/series-01.png",
						"label":         "series-01.png",
						"contentType":   "image/png",
						"sizeBytes":     12,
						"sha256":        "fake-sha256",
						"artifactScope": artifactScope,
						"scopeKind":     "task",
					}})
				}
				if !hallucinatedFiles && !strings.Contains(runMessage, "make pdf artifact") && strings.Contains(lowerRunMessage, "pdf") {
					payload["artifacts"] = appendArtifactList(payload["artifacts"], []any{map[string]any{
						"relativePath":  "artifacts/tmp-openclaw/final.pdf",
						"label":         "final.pdf",
						"contentType":   "application/pdf",
						"sizeBytes":     12,
						"sha256":        "fake-sha256",
						"artifactScope": artifactScope,
						"scopeKind":     "task",
					}})
				}
				if !hallucinatedFiles && (strings.Contains(runMessage, "视频") || strings.Contains(lowerRunMessage, "video")) {
					payload["artifacts"] = appendArtifactList(payload["artifacts"], []any{map[string]any{
						"relativePath":  "artifacts/tmp-openclaw/final.mp4",
						"label":         "final.mp4",
						"contentType":   "video/mp4",
						"sizeBytes":     12,
						"sha256":        "fake-sha256",
						"artifactScope": artifactScope,
						"scopeKind":     "task",
					}})
				}
				if len(filesystemArtifacts) > 0 {
					payload["artifacts"] = appendArtifactList(payload["artifacts"], filesystemArtifacts)
				}
				_ = conn.WriteJSON(map[string]any{
					"type":    "res",
					"id":      id,
					"ok":      true,
					"payload": payload,
				})
			case "xworkmate.artifacts.read":
				fake.artifactReadCount.Add(1)
				params := shared.AsMap(frame["params"])
				relativePath := strings.TrimSpace(shared.StringArg(params, "relativePath", ""))
				artifactScope := strings.TrimSpace(shared.StringArg(params, "artifactScope", ""))
				if fake.consumeArtifactReadFailure() {
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   false,
						"error": map[string]any{
							"code":    "OPENCLAW_ARTIFACT_READ_FAILED",
							"message": "openclaw artifact read failed",
						},
					})
					continue
				}
				if relativePath != "reports/final.md" {
					_ = conn.WriteJSON(map[string]any{
						"type": "res",
						"id":   id,
						"ok":   false,
						"error": map[string]any{
							"code":    "ARTIFACT_NOT_FOUND",
							"message": "artifact not found",
						},
					})
					continue
				}
				content := []byte("final report")
				sum := sha256.Sum256(content)
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   true,
					"payload": map[string]any{
						"runId":                  strings.TrimSpace(shared.StringArg(params, "runId", "")),
						"sessionKey":             strings.TrimSpace(shared.StringArg(params, "openclawSessionKey", "")),
						"remoteWorkingDirectory": "/remote/openclaw/workspace",
						"remoteWorkspaceRefKind": "remotePath",
						"artifactScope":          artifactScope,
						"scopeKind":              "task",
						"artifacts": []any{
							map[string]any{
								"relativePath":  "reports/final.md",
								"label":         "final.md",
								"contentType":   "text/markdown",
								"sizeBytes":     len(content),
								"sha256":        hex.EncodeToString(sum[:]),
								"artifactScope": artifactScope,
								"scopeKind":     "task",
								"encoding":      "base64",
								"content":       base64.StdEncoding.EncodeToString(content),
							},
						},
					},
				})
			case "skills.status":
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   true,
					"payload": map[string]any{
						"workspaceDir":     "/remote/openclaw/workspace",
						"managedSkillsDir": "/remote/openclaw/skills",
						"skills": []any{
							map[string]any{
								"name":        "it-infra-continuous-png",
								"description": "Generate infrastructure PNGs.",
								"source":      "openclaw-workspace",
								"skillKey":    "it-infra-continuous-png",
								"eligible":    true,
								"disabled":    false,
								"missing": map[string]any{
									"bins":   []any{},
									"env":    []any{},
									"config": []any{},
								},
							},
							map[string]any{
								"name":                 "legacy-disabled",
								"description":          "Disabled test skill.",
								"source":               "agents-skills-personal",
								"skillKey":             "legacy-disabled",
								"eligible":             false,
								"disabled":             true,
								"blockedByAgentFilter": true,
								"missing": map[string]any{
									"bins":   []any{"legacy-cli"},
									"env":    []any{},
									"config": []any{},
								},
							},
						},
					},
				})
			case "chat.run":
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   false,
					"error": map[string]any{
						"code":    "UNKNOWN_METHOD",
						"message": "unknown method: chat.run",
					},
				})
			case "session.start":
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   false,
					"error": map[string]any{
						"code":    "UNKNOWN_METHOD",
						"message": "unknown method: session.start",
					},
				})
			default:
				_ = conn.WriteJSON(map[string]any{
					"type":    "res",
					"id":      id,
					"ok":      true,
					"payload": map[string]any{},
				})
			}
		}
	})
	fake.server = &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() {
		_ = fake.server.Serve(listener)
	}()
	return fake
}

func (f *acpFakeOpenClawGateway) URL() string {
	return "ws://" + f.listener.Addr().String() + "/"
}

func (f *acpFakeOpenClawGateway) recordMethod(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods = append(f.methods, method)
}

func (f *acpFakeOpenClawGateway) Methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

func (f *acpFakeOpenClawGateway) recordRunMessage(runID, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runMessages[runID] = message
}

func (f *acpFakeOpenClawGateway) runMessage(runID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runMessages[runID]
}

func (f *acpFakeOpenClawGateway) ConnectCount() int {
	return int(f.connectCount.Load())
}

func (f *acpFakeOpenClawGateway) ChatSendCount() int {
	return int(f.chatSendCount.Load())
}

func (f *acpFakeOpenClawGateway) LastChatSendParams() map[string]any {
	params, _ := f.lastChatSendParams.Load().(map[string]any)
	return params
}

func (f *acpFakeOpenClawGateway) AgentWaitCount() int {
	return int(f.agentWaitCount.Load())
}

func (f *acpFakeOpenClawGateway) LastAgentWaitParams() map[string]any {
	params, _ := f.lastAgentWaitParams.Load().(map[string]any)
	return params
}

func (f *acpFakeOpenClawGateway) ArtifactPrepareCount() int {
	return int(f.artifactPrepareCount.Load())
}

func (f *acpFakeOpenClawGateway) exportFilesystemArtifacts(artifactScope string) []any {
	root := strings.TrimSpace(f.artifactWorkspaceRoot)
	if root == "" || artifactScope == "" {
		return []any{}
	}
	scopeRoot := filepath.Join(root, filepath.FromSlash(artifactScope))
	entries := make([]any, 0)
	_ = filepath.WalkDir(scopeRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry == nil || entry.IsDir() {
			return nil
		}
		relativePath, relErr := filepath.Rel(scopeRoot, path)
		if relErr != nil || strings.HasPrefix(relativePath, "..") {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		entries = append(entries, map[string]any{
			"relativePath":  filepath.ToSlash(relativePath),
			"label":         filepath.Base(path),
			"contentType":   artifactContentType(filepath.ToSlash(relativePath)),
			"sizeBytes":     info.Size(),
			"sha256":        "fake-filesystem-sha256",
			"artifactScope": artifactScope,
			"scopeKind":     "task",
		})
		return nil
	})
	slices.SortFunc(entries, func(left any, right any) int {
		return strings.Compare(
			shared.StringArg(shared.AsMap(left), "relativePath", ""),
			shared.StringArg(shared.AsMap(right), "relativePath", ""),
		)
	})
	return entries
}

func (f *acpFakeOpenClawGateway) LastArtifactPrepareParams() map[string]any {
	params, _ := f.lastArtifactPrepareParams.Load().(map[string]any)
	return params
}

func (f *acpFakeOpenClawGateway) ArtifactSnapshotCount() int {
	return int(f.artifactSnapshotCount.Load())
}

func (f *acpFakeOpenClawGateway) LastArtifactSnapshotParams() map[string]any {
	params, _ := f.lastArtifactSnapshotParams.Load().(map[string]any)
	return params
}

func (f *acpFakeOpenClawGateway) ArtifactExportCount() int {
	return int(f.artifactCount.Load())
}

func (f *acpFakeOpenClawGateway) ArtifactReadCount() int {
	return int(f.artifactReadCount.Load())
}

func (f *acpFakeOpenClawGateway) FailNextArtifactReads(count int) {
	f.artifactReadFailures.Store(int32(count))
}

func (f *acpFakeOpenClawGateway) consumeArtifactReadFailure() bool {
	for {
		remaining := f.artifactReadFailures.Load()
		if remaining <= 0 {
			return false
		}
		if f.artifactReadFailures.CompareAndSwap(remaining, remaining-1) {
			return true
		}
	}
}

func (f *acpFakeOpenClawGateway) LastArtifactExportParams() map[string]any {
	value := f.lastArtifactExportParams.Load()
	if value == nil {
		return nil
	}
	return shared.AsMap(value)
}

func (f *acpFakeOpenClawGateway) LastConnectClient() map[string]any {
	value := f.lastConnectClient.Load()
	if value == nil {
		return nil
	}
	return value.(map[string]any)
}

func (f *acpFakeOpenClawGateway) Close() {
	_ = f.server.Close()
}

func sameMethods(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func sameAnyStringSlice(got []any, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if fmt.Sprint(got[index]) != want[index] {
			return false
		}
	}
	return true
}

func waitForCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for condition")
}

func TestExecuteSessionTaskAutoRoutingUsesBridgeProductionProviderOrder(t *testing.T) {
	workspaceDir := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	server := NewServer()
	geminiProvider := newExternalSingleAgentProvider(t, "gemini", "gemini-output")
	defer geminiProvider.Close()
	codexProvider := newExternalSingleAgentProvider(t, "codex", "codex-output")
	defer codexProvider.Close()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID: "gemini",
		Label:      "Gemini",
		Endpoint:   geminiProvider.URL,
		Enabled:    true,
	})
	setTestBridgeProvider(server, syncedProvider{
		ProviderID: "codex",
		Label:      "Codex",
		Endpoint:   codexProvider.URL,
		Enabled:    true,
	})

	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-auto-order",
				"threadId":         "thread-auto-order",
				"taskPrompt":       "create a powerpoint deck for launch",
				"workingDirectory": workspaceDir,
				"routing": map[string]any{
					"routingMode": "auto",
					"availableSkills": []any{
						map[string]any{
							"id":          "pptx",
							"label":       "PPTX",
							"description": "slides",
							"installed":   true,
						},
					},
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected success, got rpc error: %v", rpcErr)
	}
	if got := response["resolvedProviderId"]; got != "codex" {
		t.Fatalf("expected resolved provider codex from built-in bridge order, got %#v", response)
	}
}

func TestExecuteSessionTaskKeepsRemoteWorkspaceHintOutOfLocalCWD(t *testing.T) {
	workspaceDir := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	server := NewServer()
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acp/rpc" {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = r.Body.Close() }()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		method := strings.TrimSpace(shared.StringArg(request, "method", ""))
		result := map[string]any{
			"success":                true,
			"output":                 "hello",
			"summary":                "hello",
			"message":                "hello",
			"remoteWorkingDirectory": "/owners/local/user/demo/threads/main",
			"remoteWorkspaceRefKind": "remotePath",
			"artifacts": []map[string]any{
				{
					"relativePath": "notes/hello.txt",
					"content":      "hello artifact",
					"contentType":  "text/plain",
				},
			},
		}
		if method == "thread/start" {
			result = map[string]any{"id": "codex-thread-1"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result":  result,
		})
	}))
	defer providerServer.Close()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID: "codex",
		Label:      "Codex",
		Endpoint:   providerServer.URL,
		Enabled:    true,
	})

	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":                  "session-remote-hint",
				"threadId":                   "thread-remote-hint",
				"taskPrompt":                 "say hello",
				"workingDirectory":           workspaceDir,
				"remoteWorkingDirectoryHint": "/owners/local/user/demo/threads/main",
				"routing": map[string]any{
					"routingMode":             "explicit",
					"explicitExecutionTarget": "singleAgent",
					"explicitProviderId":      "codex",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected success, got rpc error: %v", rpcErr)
	}
	if got := response["remoteWorkingDirectory"]; got != "/owners/local/user/demo/threads/main" {
		t.Fatalf("expected remote working directory in response, got %#v", response)
	}
	if got := response["remoteWorkspaceRefKind"]; got != "remotePath" {
		t.Fatalf("expected remote workspace kind in response, got %#v", response)
	}
	if _, ok := response["artifacts"].([]map[string]any); !ok {
		if _, ok := response["artifacts"].([]any); !ok {
			t.Fatalf("expected artifacts payload, got %#v", response["artifacts"])
		}
	}
	sess := server.sessions["session-remote-hint"]
	if sess == nil {
		t.Fatal("expected session state to be retained")
	} else if sess.control.RequestedWorkingDir != workspaceDir {
		t.Fatalf("expected local requested cwd %q, got %q", workspaceDir, sess.control.RequestedWorkingDir)
	}
	if sess.control.RemoteWorkingDirHint != "/owners/local/user/demo/threads/main" {
		t.Fatalf("expected remote hint retained, got %#v", sess.control)
	}
	if sess.task.Kind != TaskKindSingleAgent || sess.task.State != TaskStateCompleted {
		t.Fatalf("expected completed single-agent task, got %#v", sess.task)
	}
	if sess.artifacts.RemoteWorkingDirectory != "/owners/local/user/demo/threads/main" {
		t.Fatalf("expected artifact record to keep remote directory, got %#v", sess.artifacts)
	}
}

func TestExecuteSessionTaskRequiresRouting(t *testing.T) {
	server := NewServer()
	_, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			ID:     "request-1",
			Method: "session.start",
			Params: map[string]any{
				"sessionId":  "session-missing-routing",
				"threadId":   "thread-missing-routing",
				"taskPrompt": "hello",
			},
		},
	})
	if rpcErr == nil {
		t.Fatalf("expected routing-required error")
	} else if rpcErr.Message != "ROUTING_REQUIRED" {
		t.Fatalf("expected ROUTING_REQUIRED, got %#v", rpcErr)
	}
}

func TestExecuteSessionMessageMissingProviderStateReturnsContinuationUnavailable(t *testing.T) {
	server := NewServer()
	providerServer := newExternalSingleAgentProvider(t, "codex", "done")
	defer providerServer.Close()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID: "codex",
		Label:      "Codex",
		Endpoint:   providerServer.URL,
		Enabled:    true,
	})

	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			ID:     "request-continue",
			Method: "session.message",
			Params: map[string]any{
				"sessionId":        "session-without-provider-state",
				"threadId":         "thread-without-provider-state",
				"taskPrompt":       "continue",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":             "explicit",
					"explicitExecutionTarget": "singleAgent",
					"explicitProviderId":      "codex",
				},
			},
		},
	})
	if rpcErr == nil {
		t.Fatalf("expected continuation unavailable error, got response %#v", response)
	} else if rpcErr.Code != -32002 || !strings.Contains(rpcErr.Message, "SESSION_CONTINUATION_UNAVAILABLE") {
		t.Fatalf("expected structured continuation error, got %#v", rpcErr)
	}
	data := shared.AsMap(rpcErr.Data)
	if got := shared.StringArg(data, "code", ""); got != "SESSION_CONTINUATION_UNAVAILABLE" {
		t.Fatalf("expected continuation detail code, got %#v", rpcErr.Data)
	}
	if got := shared.StringArg(data, "sessionId", ""); got != "session-without-provider-state" {
		t.Fatalf("expected session id in error data, got %#v", rpcErr.Data)
	}
	if got := shared.StringArg(data, "threadId", ""); got != "thread-without-provider-state" {
		t.Fatalf("expected thread id in error data, got %#v", rpcErr.Data)
	}
	if got := shared.StringArg(data, "providerId", ""); got != "codex" {
		t.Fatalf("expected provider id in error data, got %#v", rpcErr.Data)
	}
}

func TestExecuteSessionTaskComplexRequestNoLongerPromotesToMultiAgent(t *testing.T) {
	workspaceDir := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Params: map[string]any{
				"sessionId":        "session-complex",
				"threadId":         "thread-complex",
				"taskPrompt":       "collect latest news and summarize it into a report for review",
				"workingDirectory": workspaceDir,
				"routing": map[string]any{
					"routingMode": "auto",
				},
			},
		},
	})
	if rpcErr == nil {
		t.Fatalf("expected gateway-not-connected error, got response %#v", response)
	} else if strings.Contains(rpcErr.Message, "multi-agent") {
		t.Fatalf("expected no multi-agent path, got rpc error: %v", rpcErr)
	}
}

func TestHandleRoutingResolveAllowsSkillInstallRetry(t *testing.T) {
	tempDir := t.TempDir()
	finder := filepath.Join(tempDir, "find-skills.sh")
	installer := filepath.Join(tempDir, "install-skills.sh")
	if err := os.WriteFile(
		finder,
		[]byte("#!/bin/sh\nprintf '%s' '{\"candidates\":[{\"id\":\"video-translator\",\"label\":\"video-translator\",\"description\":\"translate video\",\"installed\":false}]}'\n"),
		0o755,
	); err != nil {
		t.Fatalf("write finder: %v", err)
	}
	if err := os.WriteFile(
		installer,
		[]byte("#!/bin/sh\nprintf '%s' '{\"candidates\":[{\"id\":\"video-translator\",\"label\":\"video-translator\",\"description\":\"translate video\",\"installed\":true}]}'\n"),
		0o755,
	); err != nil {
		t.Fatalf("write installer: %v", err)
	}
	t.Setenv("ACP_FIND_SKILLS_BIN", finder)
	t.Setenv("ACP_INSTALL_SKILL_BIN", installer)

	result := handleRoutingResolve(map[string]any{
		"taskPrompt":       "translate and dub this video with subtitles",
		"workingDirectory": "/tmp/workspace",
		"routing": map[string]any{
			"routingMode":       "auto",
			"allowSkillInstall": true,
			"availableSkills": []any{
				map[string]any{
					"id":          "docx",
					"label":       "docx",
					"description": "docs",
					"installed":   true,
				},
			},
		},
	})

	if got := result["skillResolutionSource"]; got != "find_skills" {
		t.Fatalf("expected find_skills source, got %#v", got)
	}
	if got := result["needsSkillInstall"]; got != true {
		t.Fatalf("expected first pass to request install approval, got %#v", got)
	}
	requestID, _ := result["skillInstallRequestId"].(string)
	if strings.TrimSpace(requestID) == "" {
		t.Fatalf("expected install request id, got %#v", result)
	}

	retried := handleRoutingResolve(map[string]any{
		"taskPrompt":       "translate and dub this video with subtitles",
		"workingDirectory": "/tmp/workspace",
		"routing": map[string]any{
			"routingMode":       "auto",
			"allowSkillInstall": true,
			"installApproval": map[string]any{
				"requestId":         requestID,
				"approvedSkillKeys": []any{"video-translator"},
			},
			"availableSkills": []any{
				map[string]any{
					"id":          "docx",
					"label":       "docx",
					"description": "docs",
					"installed":   true,
				},
			},
		},
	})

	if got := retried["needsSkillInstall"]; got != false {
		t.Fatalf("expected install retry to clear needsSkillInstall, got %#v", got)
	}
	resolvedSkills, _ := retried["resolvedSkills"].([]string)
	if len(resolvedSkills) != 1 || resolvedSkills[0] != "video-translator" {
		t.Fatalf("expected installed skill to resolve, got %#v", retried["resolvedSkills"])
	}
}
