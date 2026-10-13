package acp

import (
	"context"
	"errors"
	"strings"
	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
	"xworkmate-bridge/internal/skills"
)

// unavailableRoleRoutingMoved answers ACP turns that still ask for role routing.
// AI Workspace v2.1 moved role selection and dispatch to the DSH orchestrator,
// with Accounts as the task state; Bridge only forwards.
const unavailableRoleRoutingMoved = "ROLE_ROUTING_MOVED"

func roleRoutingRequested(routingParams map[string]any) bool {
	return strings.TrimSpace(shared.StringArg(routingParams, "role", "")) != "" ||
		strings.TrimSpace(shared.StringArg(routingParams, "roleMode", "")) != ""
}

type DefaultRoutingEngine struct {
	server *Server
}

func (e *DefaultRoutingEngine) Resolve(ctx context.Context, params map[string]any) (RoutingResult, error) {
	routingParams := shared.AsMap(params["routing"])
	if len(routingParams) == 0 {
		routingParams = map[string]any{}
	}
	if roleRoutingRequested(routingParams) {
		return RoutingResult{
			Status:                "unavailable",
			UnavailableCode:       unavailableRoleRoutingMoved,
			UnavailableMsg:        "role tasks are created through the Accounts session API (POST /api/v1/sessions/{sessionId}/messages with routing) and run by the DSH orchestrator",
			SkillResolutionSource: "none",
		}, nil
	}
	routingMode := strings.TrimSpace(shared.StringArg(routingParams, "routingMode", "implicit"))
	explicitExecutionTarget := strings.TrimSpace(shared.StringArg(routingParams, "explicitExecutionTarget", ""))
	if explicitExecutionTarget == "" {
		explicitExecutionTarget = strings.TrimSpace(shared.StringArg(params, "executionTarget", ""))
	}
	if explicitExecutionTarget != "" {
		routingMode = router.RoutingModeExplicit
	}
	preferredGatewayProviderID := strings.TrimSpace(shared.StringArg(routingParams, "preferredGatewayProviderId", ""))
	if preferredGatewayProviderID == "" {
		preferredGatewayProviderID = strings.TrimSpace(shared.StringArg(params, "gatewayProviderId", ""))
	}
	if preferredGatewayProviderID == "" {
		preferredGatewayProviderID = strings.TrimSpace(shared.StringArg(params, "gatewayProvider", ""))
	}
	if len(routingParams) == 0 && explicitExecutionTarget == "" && preferredGatewayProviderID == "" {
		return RoutingResult{}, errors.New("ROUTING_REQUIRED")
	}

	installApproval := shared.AsMap(routingParams["installApproval"])
	resolver := router.NewResolver()
	res := resolver.Resolve(router.Request{
		Prompt:                     strings.TrimSpace(shared.StringArg(params, "taskPrompt", "")),
		WorkingDirectory:           strings.TrimSpace(shared.StringArg(params, "workingDirectory", "")),
		RoutingMode:                routingMode,
		PreferredGatewayProviderID: preferredGatewayProviderID,
		ExplicitExecutionTarget:    explicitExecutionTarget,
		ExplicitProviderID:         strings.TrimSpace(shared.StringArg(routingParams, "explicitProviderId", "")),
		ExplicitModel:              strings.TrimSpace(shared.StringArg(routingParams, "explicitModel", "")),
		ExplicitSkills:             parseGatewayRuntimeStringSlice(routingParams["explicitSkills"]),
		AvailableProviders:         e.server.getAvailableProviderIDs(),
		AvailableSkills:            parseSkillsCandidates(shared.ListArg(routingParams, "availableSkills")),
		AllowSkillInstall:          parseBool(routingParams["allowSkillInstall"]),
		AIGatewayBaseURL:           strings.TrimSpace(shared.StringArg(params, "aiGatewayBaseUrl", "")),
		AIGatewayAPIKey:            strings.TrimSpace(shared.StringArg(params, "aiGatewayApiKey", "")),
		InstallApproval: skills.InstallApproval{
			RequestID:         strings.TrimSpace(shared.StringArg(installApproval, "requestId", "")),
			ApprovedSkillKeys: parseGatewayRuntimeStringSlice(installApproval["approvedSkillKeys"]),
		},
	})

	return RoutingResult{
		TargetID:              res.ResolvedExecutionTarget,
		ProviderID:            res.ResolvedProviderID,
		GatewayProviderID:     res.ResolvedGatewayProviderID,
		Model:                 res.ResolvedModel,
		Skills:                res.ResolvedSkills,
		Status:                oif(res.Unavailable, "unavailable", "available"),
		UnavailableCode:       res.UnavailableCode,
		UnavailableMsg:        res.UnavailableMessage,
		SkillResolutionSource: res.SkillResolutionSource,
		NeedsSkillInstall:     res.NeedsSkillInstall,
		SkillInstallRequestID: res.SkillInstallRequestID,
	}, nil
}

func oif(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
