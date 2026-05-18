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

	case "xworkmate.sessions.get":
		return s.handleSessionGet(request.Params), nil

	case "xworkmate.tools.invoke":
		return s.invokeOpenClawTool(ctx, request.Params)

	default:
		return nil, &shared.RPCError{
			Code:    -32601,
			Message: fmt.Sprintf("unknown method: %s", method),
		}
	}
}

func (s *Server) handleSessionGet(params map[string]any) map[string]any {
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	threadID := strings.TrimSpace(shared.StringArg(params, "threadId", ""))
	if sessionID == "" && threadID == "" {
		return map[string]any{"status": "not_found"}
	}
	s.mu.RLock()
	sess := s.sessions[sessionID]
	if sess == nil && threadID != "" {
		for _, candidate := range s.sessions {
			if candidate != nil && candidate.threadID == threadID {
				sess = candidate
				break
			}
		}
	}
	s.mu.RUnlock()
	if sess == nil {
		return map[string]any{
			"status":    "not_found",
			"sessionId": sessionID,
			"threadId":  threadID,
		}
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	payload := map[string]any{
		"status":    string(sess.task.State),
		"sessionId": sess.sessionID,
		"threadId":  sess.threadID,
		"task": map[string]any{
			"sessionId": sess.task.SessionID,
			"threadId":  sess.task.ThreadID,
			"turnId":    sess.task.TurnID,
			"provider":  sess.task.Provider,
			"target":    sess.task.Target,
			"state":     string(sess.task.State),
			"kind":      string(sess.task.Kind),
			"updatedAt": sess.task.UpdatedAt.UTC().Format(time.RFC3339Nano),
		},
	}
	if len(sess.lastResult) > 0 {
		payload["result"] = cloneMap(sess.lastResult)
	}
	if len(sess.artifacts.Artifacts) > 0 ||
		sess.artifacts.RemoteWorkingDirectory != "" ||
		sess.artifacts.RemoteWorkspaceRefKind != "" ||
		sess.artifacts.ResultSummary != "" {
		payload["artifacts"] = map[string]any{
			"items":                  cloneMapSlice(sess.artifacts.Artifacts),
			"remoteWorkingDirectory": sess.artifacts.RemoteWorkingDirectory,
			"remoteWorkspaceRefKind": sess.artifacts.RemoteWorkspaceRefKind,
			"resultSummary":          sess.artifacts.ResultSummary,
			"updatedAt":              sess.artifacts.UpdatedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	return payload
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
