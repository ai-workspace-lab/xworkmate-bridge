package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
