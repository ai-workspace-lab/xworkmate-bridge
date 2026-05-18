package acp

import (
	"context"
	"fmt"
	"strings"
	"time"
	"xworkmate-bridge/internal/shared"
)

func (s *Server) handleRequest(request shared.RPCRequest, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	method := strings.TrimSpace(request.Method)
	ctx := context.Background()

	switch method {
	case "health":
		return map[string]any{"status": "ok", "version": "0.7.0", "role": "acp-control-plane"}, nil

	case "acp.capabilities":
		return s.catalog.Get(), nil

	case "session.start", "session.message":
		return s.orchestrator.Process(ctx, method, request.Params, notify)

	case "session.cancel":
		sessionID := shared.StringArg(request.Params, "sessionId", "")
		s.cancelSession(ctx, sessionID)
		return map[string]any{"accepted": true}, nil

	case "session.close":
		sessionID := shared.StringArg(request.Params, "sessionId", "")
		s.closeSession(ctx, sessionID)
		return map[string]any{"accepted": true}, nil

	case "xworkmate.routing.resolve":
		res, err := s.routingEngine.Resolve(ctx, request.Params)
		if err != nil {
			return nil, &shared.RPCError{Code: -32602, Message: err.Error()}
		}
		return map[string]any{
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
		}, nil

	case "xworkmate.gateway.connect", "xworkmate.gateway.request", "xworkmate.gateway.disconnect":
		// Gateway 语义由专门的 Gateway 组件通过 Adapter 处理
		return s.handleGatewayMethod(ctx, method, request.Params, notify)

	case "xworkmate.jobs.submit", "xworkmate.jobs.get", "xworkmate.jobs.list", "xworkmate.jobs.stats":
		return s.handleJobMethod(ctx, method, request.Params, notify)

	case "xworkmate.tools.invoke":
		return s.invokeOpenClawTool(ctx, request.Params)

	default:
		return nil, &shared.RPCError{
			Code:    -32601,
			Message: fmt.Sprintf("unknown method: %s", method),
		}
	}
}

func (s *Server) cancelSession(ctx context.Context, sessionID string) {
	s.mu.RLock()
	sess, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if ok && sess != nil && sess.compat != nil {
		sess.mu.Lock()
		sess.task.State = TaskStateCancelled
		sess.task.UpdatedAt = time.Now()
		sess.mu.Unlock()
		_ = sess.compat.CancelSession(ctx, sessionID)
	}
}

func (s *Server) closeSession(ctx context.Context, sessionID string) {
	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	delete(s.sessions, sessionID)
	s.mu.Unlock()
	if ok && sess != nil && sess.compat != nil {
		sess.mu.Lock()
		sess.task.State = TaskStateCancelled
		sess.task.UpdatedAt = time.Now()
		sess.mu.Unlock()
		_ = sess.compat.CloseSession(ctx, sessionID)
	}
}
