package acp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"xworkmate-bridge/internal/dispatch"
	"xworkmate-bridge/internal/mounts"
	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

func (s *Server) handleRequest(
	request shared.RPCRequest,
	notify func(map[string]any),
) (map[string]any, *shared.RPCError) {
	method := strings.TrimSpace(request.Method)
	switch method {
	case "health":
		return map[string]any{"status": "ok", "version": "0.7.0"}, nil
	case "acp.capabilities":
		providerCatalog := s.availableProviderCatalog()
		gatewayProviders := availableGatewayProviderCatalog()
		singleAgent := len(providerCatalog) > 0
		availableExecutionTargets := availableExecutionTargets(
			providerCatalog,
			gatewayProviders,
		)
		multiAgent := shared.BoolArg(
			shared.EnvOrDefault("ACP_MULTI_AGENT_ENABLED", "true"),
			true,
		)
		result := map[string]any{
			"singleAgent":               singleAgent,
			"multiAgent":                multiAgent,
			"availableExecutionTargets": availableExecutionTargets,
			"providerCatalog":           providerCatalog,
			"gatewayProviders":          gatewayProviders,
			"capabilities": map[string]any{
				"single_agent":              singleAgent,
				"multi_agent":               multiAgent,
				"availableExecutionTargets": availableExecutionTargets,
				"providerCatalog":           providerCatalog,
				"gatewayProviders":          gatewayProviders,
			},
		}
		return result, nil
	case "session.start", "session.message":
		params := request.Params
		sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
		if sessionID == "" {
			return nil, &shared.RPCError{
				Code:    -32602,
				Message: "sessionId is required",
			}
		}
		threadID := strings.TrimSpace(
			shared.StringArg(params, "threadId", sessionID),
		)
		if threadID == "" {
			threadID = sessionID
		}
		if method == "session.start" {
			s.resetSession(sessionID, threadID)
		}
		result, rpcErr := s.enqueue(threadID, task{
			req:    request,
			notify: notify,
			done:   make(chan taskResult, 1),
		})
		if rpcErr != nil {
			return nil, rpcErr
		}
		return result, nil
	case "session.cancel":
		params := request.Params
		sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
		if sessionID == "" {
			return nil, &shared.RPCError{
				Code:    -32602,
				Message: "sessionId is required",
			}
		}
		cancelled := s.cancelSession(sessionID)
		return map[string]any{"accepted": true, "cancelled": cancelled}, nil
	case "session.close":
		params := request.Params
		sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
		if sessionID == "" {
			return nil, &shared.RPCError{
				Code:    -32602,
				Message: "sessionId is required",
			}
		}
		closed := s.closeSession(sessionID)
		return map[string]any{"accepted": true, "closed": closed}, nil
	case "xworkmate.dispatch.resolve":
		return handleDispatchResolve(request.Params), nil
	case "xworkmate.routing.resolve":
		result, _ := resolveRoutingMetadataWithProviders(
			request.Params,
			s.availableProviders(),
		)
		return mergeRoutingResponse(map[string]any{"ok": true}, result), nil
	case "xworkmate.provider.probe":
		providerID := strings.TrimSpace(shared.StringArg(request.Params, "providerId", ""))
		if providerID == "" {
			return nil, &shared.RPCError{
				Code:    -32602,
				Message: "providerId is required",
			}
		}
		provider, ok := s.syncedProviderByID(providerID)
		if !ok {
			return map[string]any{
				"success":    false,
				"providerId": providerID,
				"error":      "provider is not advertised by the bridge",
			}, nil
		}
		result, err := s.probeExternalProvider(context.Background(), provider, request.Params)
		if err != nil {
			return map[string]any{
				"success":    false,
				"providerId": providerID,
				"error":      err.Error(),
			}, nil
		}
		return map[string]any{
			"success":      true,
			"providerId":   providerID,
			"probeMethod":  "acp.capabilities",
			"capabilities": result,
		}, nil
	case "xworkmate.mounts.reconcile":
		return handleMountReconcile(request.Params), nil
	case "xworkmate.gateway.connect":
		return handleGatewayConnect(s, request.Params, notify), nil
	case "xworkmate.gateway.request":
		return handleGatewayRequest(s, request.Params, notify), nil
	case "xworkmate.gateway.disconnect":
		return handleGatewayDisconnect(s, request.Params, notify), nil
	default:
		return nil, &shared.RPCError{
			Code:    -32601,
			Message: fmt.Sprintf("unknown method: %s", method),
		}
	}
}

func (s *Server) executeSessionTask(task task) (map[string]any, *shared.RPCError) {
	params := task.req.Params
	resolvedRouting, hasResolvedRouting := resolveRoutingMetadataWithProviders(
		params,
		s.availableProviders(),
	)
	if !hasResolvedRouting {
		return nil, &shared.RPCError{
			Code:    -32602,
			Message: "ROUTING_REQUIRED",
		}
	}

	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	threadID := strings.TrimSpace(shared.StringArg(params, "threadId", sessionID))
	if resolvedRouting.Unavailable {
		response := mergeRoutingResponse(map[string]any{
			"success":            false,
			"error":              resolvedRouting.UnavailableMessage,
			"unavailable":        true,
			"unavailableCode":    resolvedRouting.UnavailableCode,
			"unavailableMessage": resolvedRouting.UnavailableMessage,
		}, resolvedRouting)
		return response, nil
	}
	executionParams := buildResolvedExecutionParams(params, resolvedRouting)
	mode := strings.TrimSpace(shared.StringArg(executionParams, "mode", "single-agent"))
	provider := strings.TrimSpace(shared.StringArg(executionParams, "provider", ""))

	session := s.getOrCreateSession(sessionID, threadID)
	session.mode = mode
	if provider != "" {
		session.provider = provider
	}

	prompt := strings.TrimSpace(shared.StringArg(executionParams, "taskPrompt", ""))
	if prompt != "" {
		session.history = append(session.history, "USER: "+prompt)
	}
	turnID := fmt.Sprintf("turn-%d", time.Now().UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	s.setSessionCancel(sessionID, cancel)
	defer s.clearSessionCancel(sessionID)

	notify := task.notify
	s.emitSessionUpdate(session, notify, turnID, map[string]any{
		"type":    "status",
		"event":   "started",
		"message": "session started",
		"pending": true,
		"error":   false,
	})

	if mode == router.ExecutionTargetGatewayChat || mode == router.ExecutionTargetGateway {
		result := s.runGateway(
			ctx,
			task.req.Method,
			session,
			executionParams,
			turnID,
			notify,
		)
		if result.err != nil {
			return nil, result.err
		}
		result.response = mergeRoutingResponse(result.response, resolvedRouting)
		return result.response, nil
	}

	if mode == "multi-agent" {
		result := s.runMultiAgent(ctx, session, executionParams, turnID, notify)
		if result.err != nil {
			return nil, result.err
		}
		result.response = mergeRoutingResponse(result.response, resolvedRouting)
		if err := recordRoutingSuccess(params, resolvedRouting, result.response); err != nil {
			return nil, &shared.RPCError{Code: -32001, Message: err.Error()}
		}
		return result.response, nil
	}

	result := s.runSingleAgent(
		ctx,
		task.req.Method,
		session,
		executionParams,
		turnID,
		notify,
	)
	if result.err != nil {
		return nil, result.err
	}
	result.response = mergeRoutingResponse(result.response, resolvedRouting)
	if err := recordRoutingSuccess(params, resolvedRouting, result.response); err != nil {
		return nil, &shared.RPCError{Code: -32001, Message: err.Error()}
	}
	return result.response, nil
}

func (s *Server) runSingleAgent(
	ctx context.Context,
	method string,
	session *session,
	params map[string]any,
	turnID string,
	notify func(map[string]any),
) taskResult {
	provider := session.provider
	if provider == "" {
		provider = strings.TrimSpace(shared.StringArg(params, "provider", "codex"))
	}
	workingDirectory := strings.TrimSpace(
		shared.StringArg(params, "workingDirectory", ""),
	)
	_, effectiveWorkingDirectory := shared.NormalizeProviderWorkingDirectory(
		provider,
		workingDirectory,
	)

	if syncedProvider, ok := s.syncedProviderByID(provider); ok {
		response, err := s.runSingleAgentViaExternalProvider(
		ctx,
		syncedProvider,
		method,
		params,
		notify,
		)
		if err == nil {
		result := shared.AsMap(response["result"])
		if len(result) == 0 {
			result = response
		}

			if _, exists := result["provider"]; !exists {
				result["provider"] = provider
			}
			if _, exists := result["mode"]; !exists {
				result["mode"] = "single-agent"
			}
			if _, exists := result["turnId"]; !exists {
				result["turnId"] = turnID
			}
			if _, exists := result["effectiveWorkingDirectory"]; !exists && effectiveWorkingDirectory != "" {
				result["effectiveWorkingDirectory"] = effectiveWorkingDirectory
			}
			return taskResult{response: enrichSingleAgentResultArtifacts(result, params)}
		}
		s.emitSessionUpdate(session, notify, turnID, map[string]any{
			"type":    "status",
			"event":   "completed",
			"message": err.Error(),
			"pending": false,
			"error":   true,
		})
		return taskResult{
			response: map[string]any{
				"success":  false,
				"error":    err.Error(),
				"turnId":   turnID,
				"mode":     "single-agent",
				"provider": provider,
			},
		}
	}

	s.emitSessionUpdate(session, notify, turnID, map[string]any{
		"type":    "status",
		"event":   "completed",
		"message": "provider is not advertised by the bridge",
		"pending": false,
		"error":   true,
	})
	return taskResult{
		response: map[string]any{
			"success":  false,
			"error":    "provider is not advertised by the bridge",
			"turnId":   turnID,
			"mode":     "single-agent",
			"provider": provider,
		},
	}
}

func (s *Server) runMultiAgent(
	ctx context.Context,
	session *session,
	params map[string]any,
	turnID string,
	notify func(map[string]any),
) taskResult {
	prompt := shared.ComposeHistoryPrompt(session.history)
	if prompt == "" {
		prompt = strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	}
	prompt = shared.AugmentPromptWithAttachments(prompt, params)

	baseURL := shared.NormalizeBaseURL(
		shared.StringArg(params, "aiGatewayBaseUrl", os.Getenv("AI_GATEWAY_BASE_URL")),
	)
	apiKey := strings.TrimSpace(shared.StringArg(params, "aiGatewayApiKey", os.Getenv("AI_GATEWAY_API_KEY")))
	model := strings.TrimSpace(
		shared.StringArg(
			params,
			"model",
			shared.EnvOrDefault("ACP_MULTI_AGENT_MODEL", "gpt-4o"),
		),
	)
	if model == "" {
		model = "gpt-4o"
	}

	s.emitSessionUpdate(session, notify, turnID, map[string]any{
		"type":      "step",
		"mode":      "multi-agent",
		"title":     "Planner",
		"message":   "Preparing multi-agent run",
		"pending":   false,
		"error":     false,
		"role":      "architect",
		"iteration": 1,
		"score":     0,
	})

	if apiKey == "" {
		errMsg := "aiGatewayApiKey is required for multi-agent mode"
		s.emitSessionUpdate(session, notify, turnID, map[string]any{
			"type":    "status",
			"mode":    "multi-agent",
			"message": errMsg,
			"pending": false,
			"error":   true,
		})
		return taskResult{
			response: map[string]any{
				"success": false,
				"error":   errMsg,
				"turnId":  turnID,
				"mode":    "multi-agent",
			},
		}
	}

	messages := []map[string]string{
		{
			"role":    "system",
			"content": "You are a multi-agent coordinator. Return concise actionable output.",
		},
		{"role": "user", "content": prompt},
	}
	output, err := shared.CallOpenAICompatibleCtx(
		ctx,
		baseURL,
		apiKey,
		model,
		messages,
	)
	if err != nil {
		s.emitSessionUpdate(session, notify, turnID, map[string]any{
			"type":    "status",
			"mode":    "multi-agent",
			"message": err.Error(),
			"pending": false,
			"error":   true,
		})
		return taskResult{
			response: map[string]any{
				"success": false,
				"error":   err.Error(),
				"turnId":  turnID,
				"mode":    "multi-agent",
			},
		}
	}

	s.emitSessionUpdate(session, notify, turnID, map[string]any{
		"type":      "step",
		"mode":      "multi-agent",
		"title":     "Reviewer",
		"message":   output,
		"pending":   false,
		"error":     false,
		"role":      "tester",
		"iteration": 1,
		"score":     9,
	})

	return taskResult{
		response: map[string]any{
			"success":    true,
			"summary":    output,
			"finalScore": 9,
			"iterations": 1,
			"turnId":     turnID,
			"mode":       "multi-agent",
		},
	}
}

func handleDispatchResolve(params map[string]any) map[string]any {
	providers := parseDispatchProviders(params["providers"])
	requiredCapabilities := parseStringSlice(params["requiredCapabilities"])
	preferredProviderID := strings.TrimSpace(
		shared.StringArg(params, "preferredProviderId", ""),
	)
	request := dispatch.Request{
		Providers:            providers,
		PreferredProviderID:  preferredProviderID,
		RequiredCapabilities: requiredCapabilities,
	}
	if nodeState := parseDispatchNodeState(params["nodeState"]); nodeState != nil {
		request.NodeState = nodeState
	}
	if nodeInfo := parseDispatchNodeInfo(params["nodeInfo"]); nodeInfo != nil {
		request.NodeInfo = nodeInfo
	}
	return dispatch.ResultMap(dispatch.Resolve(request))
}

func parseDispatchProviders(raw any) []dispatch.Provider {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	providers := make([]dispatch.Provider, 0, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := strings.TrimSpace(shared.StringArg(entry, "id", ""))
		if id == "" {
			continue
		}
		providers = append(providers, dispatch.Provider{
			ID:           id,
			Name:         strings.TrimSpace(shared.StringArg(entry, "name", "")),
			DefaultArgs:  parseStringSlice(entry["defaultArgs"]),
			Capabilities: parseStringSlice(entry["capabilities"]),
		})
	}
	return providers
}

func parseDispatchNodeState(raw any) *dispatch.NodeState {
	entry, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return &dispatch.NodeState{
		SelectedAgentID: strings.TrimSpace(
			shared.StringArg(entry, "selectedAgentId", ""),
		),
		GatewayConnected: shared.BoolArg(
			fmt.Sprint(entry["gatewayConnected"]),
			false,
		),
		ExecutionTarget: strings.TrimSpace(
			shared.StringArg(entry, "executionTarget", ""),
		),
		RuntimeMode:   strings.TrimSpace(shared.StringArg(entry, "runtimeMode", "")),
		BridgeEnabled: shared.BoolArg(fmt.Sprint(entry["bridgeEnabled"]), false),
		BridgeState:   strings.TrimSpace(shared.StringArg(entry, "bridgeState", "")),
		ResolvedCodexCLIPath: strings.TrimSpace(
			shared.StringArg(entry, "resolvedCodexCliPath", ""),
		),
		ConfiguredCodexCLIPath: strings.TrimSpace(
			shared.StringArg(entry, "configuredCodexCliPath", ""),
		),
	}
}

func parseDispatchNodeInfo(raw any) *dispatch.NodeInfo {
	entry, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return &dispatch.NodeInfo{
		ID:      strings.TrimSpace(shared.StringArg(entry, "id", "")),
		Name:    strings.TrimSpace(shared.StringArg(entry, "name", "")),
		Version: strings.TrimSpace(shared.StringArg(entry, "version", "")),
	}
}

func parseStringSlice(raw any) []string {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(list))
	for _, item := range list {
		value := strings.TrimSpace(fmt.Sprint(item))
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return values
}

func handleMountReconcile(params map[string]any) map[string]any {
	config := parseMountConfig(params["config"])
	request := mounts.Request{
		Config:                 config,
		AIGatewayURL:           strings.TrimSpace(shared.StringArg(params, "aiGatewayUrl", "")),
		ConfiguredCodexCLIPath: strings.TrimSpace(shared.StringArg(params, "configuredCodexCliPath", "")),
		CodexHome:              strings.TrimSpace(shared.StringArg(params, "codexHome", "")),
		OpencodeHome:           strings.TrimSpace(shared.StringArg(params, "opencodeHome", "")),
		OpenClawHome:           strings.TrimSpace(shared.StringArg(params, "openclawHome", "")),
		Aris:                   parseMountArisInput(params["aris"]),
	}
	return mounts.ResultMap(mounts.Reconcile(request))
}

func parseMountConfig(raw any) mounts.Config {
	entry, ok := raw.(map[string]any)
	if !ok {
		return mounts.Config{}
	}
	managedMCPServers := parseMountManagedServers(entry["managedMcpServers"])
	return mounts.Config{
		AutoSync:          shared.BoolArg(fmt.Sprint(entry["autoSync"]), false),
		UsesAris:          shared.BoolArg(fmt.Sprint(entry["usesAris"]), false),
		ManagedMCPServers: managedMCPServers,
	}
}

func parseMountManagedServers(raw any) []mounts.ManagedMCPServer {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	servers := make([]mounts.ManagedMCPServer, 0, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := strings.TrimSpace(shared.StringArg(entry, "id", ""))
		if id == "" {
			continue
		}
		servers = append(servers, mounts.ManagedMCPServer{
			ID:        id,
			Name:      strings.TrimSpace(shared.StringArg(entry, "name", "")),
			Transport: strings.TrimSpace(shared.StringArg(entry, "transport", "")),
			Command:   strings.TrimSpace(shared.StringArg(entry, "command", "")),
			URL:       strings.TrimSpace(shared.StringArg(entry, "url", "")),
			Args:      parseStringSlice(entry["args"]),
			Enabled:   shared.BoolArg(fmt.Sprint(entry["enabled"]), true),
		})
	}
	return servers
}

func parseMountArisInput(raw any) mounts.ArisInput {
	entry, ok := raw.(map[string]any)
	if !ok {
		return mounts.ArisInput{}
	}
	return mounts.ArisInput{
		Available:         shared.BoolArg(fmt.Sprint(entry["available"]), false),
		BundleVersion:     strings.TrimSpace(shared.StringArg(entry, "bundleVersion", "")),
		LLMChatServerPath: strings.TrimSpace(shared.StringArg(entry, "llmChatServerPath", "")),
		SkillCount:        shared.IntArg(fmt.Sprint(entry["skillCount"]), 0),
		BridgeAvailable:   shared.BoolArg(fmt.Sprint(entry["bridgeAvailable"]), false),
		Error:             strings.TrimSpace(shared.StringArg(entry, "error", "")),
	}
}
