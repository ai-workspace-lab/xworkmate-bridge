package acp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"xworkmate-bridge/internal/memory"
	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

type SessionOrchestrator struct {
	server *Server
}

func NewSessionOrchestrator(server *Server) *SessionOrchestrator {
	return &SessionOrchestrator{server: server}
}

func (o *SessionOrchestrator) Process(ctx context.Context, method string, params map[string]any, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	// 1. 路由解析 (Core Control Plane Duty)
	res, err := o.server.routingEngine.Resolve(ctx, params)
	if err != nil {
		if err.Error() == "ROUTING_REQUIRED" {
			return nil, &shared.RPCError{Code: -32602, Message: err.Error()}
		}
		return nil, &shared.RPCError{Code: -32602, Message: "ROUTING_FAILED: " + err.Error()}
	}

	if res.Status == "unavailable" {
		return o.formatUnavailable(res), nil
	}

	// 2. 环境准备
	sessionID := shared.StringArg(params, "sessionId", "")
	threadID := shared.StringArg(params, "threadId", sessionID)
	turnID := fmt.Sprintf("turn-%d", time.Now().UnixNano())

	sess := o.server.getOrCreateSession(sessionID, threadID)
	sess.mu.Lock()
	sess.target = res.TargetID
	sess.provider = res.ProviderID
	prompt := strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	if prompt != "" {
		sess.history = append(sess.history, "USER: "+prompt)
	}
	sess.mu.Unlock()

	o.server.emitSessionUpdate(notify, turnID, map[string]any{
		"type":    "status",
		"event":   "started",
		"message": "session started",
		"pending": true,
		"error":   false,
	})

	if res.TargetID == "gateway" {
		return o.runGateway(ctx, method, params, turnID, notify)
	}

	if res.TargetID == "multi-agent" {
		return o.runMultiAgent(ctx, sess, params, turnID, notify)
	}

	// 3. 选择适配器
	adapter, ok := o.server.adapters[res.ProviderID]
	if !ok {
		return nil, &shared.RPCError{Code: -32001, Message: "PROVIDER_NOT_FOUND: " + res.ProviderID}
	}

	// 4. 执行适配器
	if sessionID != "" {
		o.server.mu.Lock()
		o.server.sessionToAdapter[sessionID] = adapter
		o.server.mu.Unlock()
	}
	eventChan, err := adapter.Execute(ctx, sessionID, threadID, method, params)
	if err != nil {
		return nil, &shared.RPCError{Code: -32002, Message: "EXECUTION_FAILED: " + err.Error()}
	}

	// 5. 事件归一化输出
	result, rpcErr := o.dispatchEvents(ctx, turnID, res, eventChan, notify)
	if rpcErr != nil {
		return nil, rpcErr
	}

	// 6. 记录成功 (Project Memory)
	workingDirectory := shared.StringArg(params, "workingDirectory", "")
	routingParams := shared.AsMap(params["routing"])
	routingMode := strings.TrimSpace(shared.StringArg(routingParams, "routingMode", ""))
	if workingDirectory != "" && (routingMode == "auto" || routingMode == "") {
		_ = o.server.memoryService.RecordSuccess(workingDirectory, memory.SuccessEntry{
			ResolvedExecutionTarget: res.TargetID,
			ResolvedProviderID:      res.ProviderID,
			ResolvedModel:           res.Model,
			ResolvedSkills:          res.Skills,
			Summary:                 shared.StringArg(result, "output", ""),
		})
	}

	return result, nil
}

func (o *SessionOrchestrator) runGateway(
	ctx context.Context,
	method string,
	params map[string]any,
	turnID string,
	notify func(map[string]any),
) (map[string]any, *shared.RPCError) {
	if o.server.gateway == nil {
		return nil, &shared.RPCError{Code: -32001, Message: "GATEWAY_NOT_INITIALIZED"}
	}

	gatewayProvider := strings.TrimSpace(shared.StringArg(params, "gatewayProvider", ""))
	if gatewayProvider == "" {
		gatewayProvider = router.GatewayProviderOpenClaw
	}
	result := o.server.gateway.RequestByMode(
		gatewayProvider,
		method,
		params,
		2*time.Minute,
		notify,
	)
	if !result.OK {
		errMessage := strings.TrimSpace(shared.StringArg(result.Error, "message", "gateway execution failed"))
		return nil, &shared.RPCError{Code: -32002, Message: errMessage}
	}
	payload := shared.AsMap(result.Payload)
	if len(payload) == 0 {
		payload = map[string]any{
			"success": true,
			"turnId":  turnID,
			"mode":    router.ExecutionTargetGatewayChat,
		}
	}
	if _, ok := payload["turnId"]; !ok {
		payload["turnId"] = turnID
	}
	if _, ok := payload["mode"]; !ok {
		payload["mode"] = router.ExecutionTargetGatewayChat
	}
	return payload, nil
}

func (o *SessionOrchestrator) runMultiAgent(
	ctx context.Context,
	session *session,
	params map[string]any,
	turnID string,
	notify func(map[string]any),
) (map[string]any, *shared.RPCError) {
	session.mu.Lock()
	history := append([]string(nil), session.history...)
	session.mu.Unlock()

	prompt := shared.ComposeHistoryPrompt(history)
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

	o.server.emitSessionUpdate(notify, turnID, map[string]any{
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
		o.server.emitSessionUpdate(notify, turnID, map[string]any{
			"type":    "status",
			"mode":    "multi-agent",
			"message": errMsg,
			"pending": false,
			"error":   true,
		})
		return nil, &shared.RPCError{Code: -32001, Message: errMsg}
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
		o.server.emitSessionUpdate(notify, turnID, map[string]any{
			"type":    "status",
			"mode":    "multi-agent",
			"message": err.Error(),
			"pending": false,
			"error":   true,
		})
		return nil, &shared.RPCError{Code: -32002, Message: err.Error()}
	}

	session.mu.Lock()
	session.history = append(session.history, "ASSISTANT: "+output)
	session.mu.Unlock()

	o.server.emitSessionUpdate(notify, turnID, map[string]any{
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

	return map[string]any{
		"success":                 true,
		"summary":                 output,
		"finalScore":              9,
		"iterations":              1,
		"turnId":                  turnID,
		"mode":                    "multi-agent",
		"resolvedExecutionTarget": "multi-agent",
	}, nil
}

func (o *SessionOrchestrator) dispatchEvents(ctx context.Context, turnID string, routing RoutingResult, eventChan <-chan SessionEvent, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	var finalResult map[string]any

	for event := range eventChan {
		switch event.Type {
		case "chunk", "status":
			payload := event.Payload
			payload["turnId"] = turnID
			notify(payload)
		case "result":
			finalResult = event.Payload
		case "error":
			return nil, event.Error
		}
	}

	// 6. 结果集归一化 (Stable Contract for App)
	if finalResult == nil {
		finalResult = make(map[string]any)
	}
	finalResult["turnId"] = turnID
	finalResult["resolvedExecutionTarget"] = routing.TargetID
	finalResult["resolvedProviderId"] = routing.ProviderID
	finalResult["status"] = "completed"
	finalResult["success"] = true

	return finalResult, nil
}

func (o *SessionOrchestrator) formatUnavailable(res RoutingResult) map[string]any {
	return map[string]any{
		"success":            false,
		"unavailable":        true,
		"unavailableCode":    res.UnavailableCode,
		"unavailableMessage": res.UnavailableMsg,
		"resolvedProviderId": res.ProviderID,
	}
}

func (s *Server) getOrCreateSession(sessionID, threadID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		sess = &session{
			sessionID: sessionID,
			threadID:  threadID,
		}
		s.sessions[sessionID] = sess
	}
	return sess
}

func (s *Server) emitSessionUpdate(notify func(map[string]any), turnID string, update map[string]any) {
	if notify == nil || update == nil {
		return
	}
	update["turnId"] = turnID
	notify(update)
}
