package acp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	if gateway.SessionStartCount() != 1 {
		t.Fatalf("expected one session.start request, got %d", gateway.SessionStartCount())
	}
	client := gateway.LastConnectClient()
	if got := client["id"]; got != "openclaw-macos" {
		t.Fatalf("expected OpenClaw-compatible client id, got %#v", client)
	}
	if got := strings.TrimSpace(shared.StringArg(client, "modelIdentifier", "")); got == "" {
		t.Fatalf("expected non-empty modelIdentifier, got %#v", client)
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
	server            *http.Server
	listener          net.Listener
	connectCount      atomic.Int32
	sessionStartCount atomic.Int32
	lastConnectClient atomic.Value
}

func newAcpFakeOpenClawGateway(t *testing.T) *acpFakeOpenClawGateway {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake openclaw gateway: %v", err)
	}
	fake := &acpFakeOpenClawGateway{listener: listener}
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
			switch strings.TrimSpace(shared.StringArg(frame, "method", "")) {
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
			case "session.start":
				fake.sessionStartCount.Add(1)
				_ = conn.WriteJSON(map[string]any{
					"type": "res",
					"id":   id,
					"ok":   true,
					"payload": map[string]any{
						"success": true,
						"output":  "gateway pong",
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

func (f *acpFakeOpenClawGateway) ConnectCount() int {
	return int(f.connectCount.Load())
}

func (f *acpFakeOpenClawGateway) SessionStartCount() int {
	return int(f.sessionStartCount.Load())
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
