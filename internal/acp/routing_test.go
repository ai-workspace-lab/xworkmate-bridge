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
	return s.handleRequest(t.req, t.notify)
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
	}
	if rpcErr.Message == "GATEWAY_PROVIDER_REQUIRED" {
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
	if gateway.AgentWaitCount() != 1 {
		t.Fatalf("expected one OpenClaw agent.wait request, got %d", gateway.AgentWaitCount())
	}
	waitParams := gateway.LastAgentWaitParams()
	timeoutMs, ok := waitParams["timeoutMs"].(float64)
	if !ok {
		t.Fatalf("expected numeric OpenClaw agent.wait timeoutMs, got %#v", waitParams)
	}
	if got := int64(timeoutMs); got != openClawAgentWaitTimeout.Milliseconds() {
		t.Fatalf("expected OpenClaw agent.wait timeoutMs %d, got %#v", openClawAgentWaitTimeout.Milliseconds(), waitParams)
	}
	if got := int64(timeoutMs); got <= 120000 {
		t.Fatalf("expected OpenClaw agent.wait timeout to exceed the previous 120s cap, got %#v", waitParams)
	}
	if gateway.ArtifactExportCount() != 1 {
		t.Fatalf("expected one OpenClaw artifact export request, got %d", gateway.ArtifactExportCount())
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "chat.send", "agent.wait", "xworkmate.artifacts.export"}) {
		t.Fatalf("expected connect, chat.send, agent.wait, then artifact export, got %#v", got)
	}
	client := gateway.LastConnectClient()
	if got := client["id"]; got != "openclaw-macos" {
		t.Fatalf("expected OpenClaw-compatible client id, got %#v", client)
	}
	if got := strings.TrimSpace(shared.StringArg(client, "modelIdentifier", "")); got == "" {
		t.Fatalf("expected non-empty modelIdentifier, got %#v", client)
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
	if gateway.AgentWaitCount() != 1 {
		t.Fatalf("expected one OpenClaw agent.wait request, got %d", gateway.AgentWaitCount())
	}
	if gateway.ArtifactExportCount() != 1 {
		t.Fatalf("expected one OpenClaw artifact export request, got %d", gateway.ArtifactExportCount())
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "chat.send", "agent.wait", "xworkmate.artifacts.export"}) {
		t.Fatalf("expected connect, chat.send, agent.wait, then artifact export, got %#v", got)
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
	}
	if rpcErr.Code != -32002 || !strings.Contains(rpcErr.Message, "openclaw chat failed") {
		t.Fatalf("expected surfaced chat.send failure, got %#v", rpcErr)
	}
	if got := gateway.Methods(); len(got) != 2 || got[0] != "connect" || got[1] != "chat.send" {
		t.Fatalf("expected connect then chat.send, got %#v", got)
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
	}
	if rpcErr.Code != -32002 || !strings.Contains(rpcErr.Message, "OPENCLAW_GATEWAY_SOCKET_CLOSED") {
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
	if rpcErr == nil {
		t.Fatalf("expected OpenClaw agent.wait error, got response: %#v", response)
	}
	if rpcErr.Code != -32002 || !strings.Contains(rpcErr.Message, "openclaw wait failed") {
		t.Fatalf("expected surfaced agent.wait failure, got %#v", rpcErr)
	}
	if got := gateway.Methods(); len(got) != 3 || got[0] != "connect" || got[1] != "chat.send" || got[2] != "agent.wait" {
		t.Fatalf("expected connect, chat.send, then agent.wait, got %#v", got)
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
	if gateway.ArtifactExportCount() != 1 {
		t.Fatalf("expected one OpenClaw artifact export request, got %d", gateway.ArtifactExportCount())
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
	if got := parsedDownloadURL.Query().Get("sessionKey"); got != "thread-openclaw-artifact" {
		t.Fatalf("expected thread sessionKey in downloadUrl, got %q", got)
	}
	if got := parsedDownloadURL.Query().Get("relativePath"); got != "reports/final.md" {
		t.Fatalf("expected artifact relativePath in downloadUrl, got %q", got)
	}
	artifactScope := parsedDownloadURL.Query().Get("artifactScope")
	if !strings.HasPrefix(artifactScope, "tasks/") {
		t.Fatalf("expected artifact scope in downloadUrl, got %q", artifactScope)
	}
	if parsedDownloadURL.Query().Get("sig") == "" {
		t.Fatalf("expected signed downloadUrl, got %q", downloadURL)
	}
	exportParams := gateway.LastArtifactExportParams()
	if got := strings.TrimSpace(shared.StringArg(exportParams, "maxInlineBytes", "")); got != "0" {
		t.Fatalf("expected OpenClaw artifact export to disable inline content, got %#v", exportParams)
	}
	if got := shared.BoolArg(shared.StringArg(exportParams, "includeContent", ""), true); got {
		t.Fatalf("expected OpenClaw artifact export to omit content, got %#v", exportParams)
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.artifacts.prepare", "chat.send", "agent.wait", "xworkmate.artifacts.export"}) {
		t.Fatalf("expected connect, artifact prepare, chat.send, agent.wait, then artifact export, got %#v", got)
	}
}

func TestExecuteSessionTaskGatewayDoesNotExportStaleWorkspaceArtifactsWhenScopedDirectoryEmpty(t *testing.T) {
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
		t.Fatalf("expected artifact guard response, got rpc error: %#v", rpcErr)
	}
	if got := response["success"]; got != false {
		t.Fatalf("expected artifact_missing failure, got %#v", response)
	}
	if got := response["status"]; got != "artifact_missing" {
		t.Fatalf("expected artifact_missing status, got %#v", response)
	}
	exportParams := gateway.LastArtifactExportParams()
	if got := strings.TrimSpace(shared.StringArg(exportParams, "artifactScope", "")); !strings.HasPrefix(got, "tasks/thread-openclaw-latest-artifact/") {
		t.Fatalf("expected scoped artifact export params, got %#v", exportParams)
	}
	if _, ok := exportParams["latestIfEmpty"]; ok {
		t.Fatalf("expected no latestIfEmpty fallback export param, got %#v", exportParams)
	}
	if got := strings.TrimSpace(shared.StringArg(exportParams, "maxInlineBytes", "")); got != "0" {
		t.Fatalf("expected latest workspace export to disable inline content, got %#v", exportParams)
	}
	if got := shared.BoolArg(shared.StringArg(exportParams, "includeContent", ""), true); got {
		t.Fatalf("expected latest workspace export to omit content, got %#v", exportParams)
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.artifacts.prepare", "chat.send", "agent.wait", "xworkmate.artifacts.export"}) {
		t.Fatalf("expected connect, artifact prepare, chat.send, agent.wait, then artifact export, got %#v", got)
	}
}

func TestExecuteSessionMessageGatewayRejectsClaimedArtifactsWithoutScopedFiles(t *testing.T) {
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
		t.Fatalf("expected claimed artifact guard response, got rpc error: %#v", rpcErr)
	}
	if got := response["success"]; got != false {
		t.Fatalf("expected claimed artifact_missing failure, got %#v", response)
	}
	if got := response["status"]; got != "artifact_missing" {
		t.Fatalf("expected artifact_missing status, got %#v", response)
	}
	exportParams := gateway.LastArtifactExportParams()
	if _, ok := exportParams["latestIfEmpty"]; ok {
		t.Fatalf("expected no latestIfEmpty fallback export param, got %#v", exportParams)
	}
	if _, ok := exportParams["latestTaskScopeIfEmpty"]; ok {
		t.Fatalf("expected no latestTaskScopeIfEmpty fallback export param, got %#v", exportParams)
	}
	if _, ok := exportParams["artifactScope"]; ok {
		t.Fatalf("expected no new prepared artifact scope for claimed follow-up, got %#v", exportParams)
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "chat.send", "agent.wait", "xworkmate.artifacts.export"}) {
		t.Fatalf("expected connect, chat.send, agent.wait, then artifact export, got %#v", got)
	}
}

func TestFilterOpenClawArtifactPayloadByOutputKeepsMentionedFiles(t *testing.T) {
	payload := map[string]any{
		"remoteWorkingDirectory": "/remote/openclaw/workspace",
		"artifacts": []any{
			map[string]any{"relativePath": "k8s-networking.pdf"},
			map[string]any{"relativePath": "k8s-networking.docx"},
			map[string]any{"relativePath": "generate_all.py"},
		},
	}
	filtered := filterOpenClawArtifactPayloadByOutput(
		"文件已经生成好了：k8s-networking.pdf, k8s-networking.docx",
		payload,
	)
	artifacts := shared.ListArg(filtered, "artifacts")
	if len(artifacts) != 2 {
		t.Fatalf("expected only mentioned artifacts, got %#v", artifacts)
	}
	if got := shared.StringArg(shared.AsMap(artifacts[0]), "relativePath", ""); got != "k8s-networking.pdf" {
		t.Fatalf("expected pdf artifact first, got %#v", artifacts)
	}
	if got := shared.StringArg(shared.AsMap(artifacts[1]), "relativePath", ""); got != "k8s-networking.docx" {
		t.Fatalf("expected docx artifact second, got %#v", artifacts)
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

func TestOpenClawChatSendParamsAddsArtifactDeliveryInstructions(t *testing.T) {
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
			}, "turn-artifact-instructions", &openClawPreparedArtifactScope{
				ArtifactScope:     "tasks/thread-artifact-instructions/turn-artifact-instructions",
				ArtifactDirectory: "/remote/openclaw/workspace/tasks/thread-artifact-instructions/turn-artifact-instructions",
				ScopeKind:         "task",
			})
			if rpcErr != nil {
				t.Fatalf("expected chat params, got rpc error: %#v", rpcErr)
			}
			message := strings.TrimSpace(shared.StringArg(chatParams, "message", ""))
			if !strings.Contains(message, prompt) {
				t.Fatalf("expected original prompt to be preserved, got %q", message)
			}
			if !strings.Contains(message, "Create the requested files as real files") {
				t.Fatalf("expected artifact delivery instructions, got %q", message)
			}
			if !strings.Contains(message, "/remote/openclaw/workspace/tasks/thread-artifact-instructions/turn-artifact-instructions") {
				t.Fatalf("expected scoped artifact directory instruction, got %q", message)
			}
			if !strings.Contains(message, "Do not claim that files are ready") {
				t.Fatalf("expected anti-hallucination download instruction, got %q", message)
			}
		})
	}
}

func TestOpenClawArtifactDeliveryRequiredScansNestedParams(t *testing.T) {
	params := map[string]any{
		"request": map[string]any{
			"params": map[string]any{
				"taskPrompt": "请在当前任务制品目录中真实生成一个文件",
			},
		},
	}
	if !openClawArtifactDeliveryRequired(params) {
		t.Fatal("expected nested artifact delivery prompt to be detected")
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

func TestExecuteSessionTaskGatewayKeepsTextWhenArtifactExportUnavailable(t *testing.T) {
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
	if gateway.ArtifactExportCount() != 1 {
		t.Fatalf("expected one OpenClaw artifact export request, got %d", gateway.ArtifactExportCount())
	}
	warnings := response["artifactWarnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(fmt.Sprint(warnings[0]), "unknown method") {
		t.Fatalf("expected artifact warning for unknown method, got %#v", response["artifactWarnings"])
	}
}

func TestExecuteSessionTaskGatewayRejectsMissingOpenClawFilesForDeliveryRequest(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-openclaw-missing-files",
				"threadId":         "thread-openclaw-missing-files",
				"taskPrompt":       "输出 PPT PDF docx 文件 hallucinate-files",
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
		t.Fatalf("expected bridge response, got rpc error: %#v", rpcErr)
	}
	if success, _ := response["success"].(bool); success {
		t.Fatalf("expected missing artifact delivery to be marked unsuccessful, got %#v", response)
	}
	output := strings.TrimSpace(shared.StringArg(response, "output", ""))
	if strings.Contains(output, "点击直接下载") || strings.Contains(output, "文件已就绪") {
		t.Fatalf("expected hallucinated download text to be replaced, got %q", output)
	}
	if !strings.Contains(output, "未检测到 OpenClaw 本轮导出的实际文件") {
		t.Fatalf("expected explicit missing artifact message, got %q", output)
	}
	if _, ok := response["artifacts"]; ok {
		t.Fatalf("expected no artifacts when export returned none, got %#v", response["artifacts"])
	}
	warnings := response["artifactWarnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(fmt.Sprint(warnings[0]), "returned no files") {
		t.Fatalf("expected missing artifact warning, got %#v", response["artifactWarnings"])
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
	}
	if rpcErr.Message == "GATEWAY_PROVIDER_REQUIRED" {
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
	server                   *http.Server
	listener                 net.Listener
	connectCount             atomic.Int32
	chatSendCount            atomic.Int32
	agentWaitCount           atomic.Int32
	artifactCount            atomic.Int32
	artifactReadCount        atomic.Int32
	artifactReadFailures     atomic.Int32
	closeNextChatSend        atomic.Bool
	alwaysCloseChatSend      atomic.Bool
	agentWaitDelayMs         atomic.Int64
	largeGatewayPayloadBytes atomic.Int64
	emitAgentDelta           atomic.Bool
	lastConnectClient        atomic.Value
	lastArtifactExportParams atomic.Value
	lastAgentWaitParams      atomic.Value
	mu                       sync.Mutex
	methods                  []string
	runMessages              map[string]string
	artifactMode             string
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
				fake.recordRunMessage(runID, strings.TrimSpace(shared.StringArg(params, "message", "")))
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   true,
					"payload": map[string]any{
						"runId":  runID,
						"status": "started",
					},
				})
			case "xworkmate.artifacts.prepare":
				params := shared.AsMap(frame["params"])
				runID := strings.TrimSpace(shared.StringArg(params, "runId", "fake-run"))
				sessionKey := strings.TrimSpace(shared.StringArg(params, "sessionKey", "main"))
				artifactScope := "tasks/" + sessionKey + "/" + runID
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   true,
					"payload": map[string]any{
						"runId":                     runID,
						"sessionKey":                sessionKey,
						"remoteWorkingDirectory":    "/remote/openclaw/workspace",
						"remoteWorkspaceRefKind":    "remotePath",
						"artifactScope":             artifactScope,
						"scopeKind":                 "task",
						"artifactDirectory":         "/remote/openclaw/workspace/" + artifactScope,
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
				}
				message := "gateway pong"
				if strings.Contains(fake.runMessage(runID), "hallucinate-files") {
					message = "文件已就绪，点击直接下载👇 三个格式一键收取："
				}
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
				artifactScope := strings.TrimSpace(shared.StringArg(params, "artifactScope", ""))
				payload := map[string]any{
					"runId":                  runID,
					"sessionKey":             strings.TrimSpace(shared.StringArg(params, "sessionKey", "")),
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
						"sessionKey":             strings.TrimSpace(shared.StringArg(params, "sessionKey", "")),
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

func (f *acpFakeOpenClawGateway) AgentWaitCount() int {
	return int(f.agentWaitCount.Load())
}

func (f *acpFakeOpenClawGateway) LastAgentWaitParams() map[string]any {
	params, _ := f.lastAgentWaitParams.Load().(map[string]any)
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
	}
	if sess.control.RequestedWorkingDir != workspaceDir {
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
	}
	if rpcErr.Message != "ROUTING_REQUIRED" {
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
	}
	if rpcErr.Code != -32002 || !strings.Contains(rpcErr.Message, "SESSION_CONTINUATION_UNAVAILABLE") {
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
	}
	if strings.Contains(rpcErr.Message, "multi-agent") {
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
