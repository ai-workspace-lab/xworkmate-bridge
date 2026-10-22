package acp

import (
	"regexp"
	"strings"
	"xworkmate-bridge/internal/shared"
)

var centralProductModel = regexp.MustCompile(`^xworkmate/[A-Za-z0-9_.:/-]+$`)

// Product semantics are forwarded to the Gateway. The bridge neither chooses
// a worker process nor owns a second task/scheduler state machine.
func openClawProductCapability(params map[string]any) (map[string]any, *shared.RPCError) {
	raw, exists := shared.AsMap(params["metadata"])["xworkmateProductCapability"]
	if !exists {
		return nil, nil
	}
	value, ok := raw.(map[string]any)
	invalid := func() (map[string]any, *shared.RPCError) {
		return nil, &shared.RPCError{Code: -32602, Message: "invalid xworkmateProductCapability v1 contract"}
	}
	if !ok || !validProductSchemaVersion(value["schemaVersion"]) {
		return invalid()
	}
	for key := range value {
		if key != "schemaVersion" && key != "mode" && key != "model" {
			return invalid()
		}
	}
	mode, ok := value["mode"].(string)
	if !ok || (mode != "chat" && mode != "work" && mode != "code") {
		return invalid()
	}
	result := map[string]any{"schemaVersion": 1, "mode": mode}
	if rawModel, exists := value["model"]; exists {
		model, ok := rawModel.(string)
		if !ok || !centralProductModel.MatchString(model) || len(model) > 256 || strings.TrimSpace(model) == "" || strings.ContainsAny(model, "\r\n\x00") {
			return invalid()
		}
		result["model"] = strings.TrimSpace(model)
	}
	return result, nil
}

func productCapabilityReceipt(capability map[string]any) string {
	switch capability["mode"] {
	case "work":
		return "XWorkmate product mode: Work. For document/long-task execution use the run-scoped xworkmate_worker tool with engine=dsh-acp. If the worker is unavailable or denies permission, report that failure and wait for authorized configuration. Keep generated artifacts in this task scope. Do not claim execution without a worker result."
	case "code":
		return "XWorkmate product mode: Code. Use the run-scoped xworkmate_worker tool with engine=opencode-v2 for the authorized repository/code task. Preserve scoped diff and actual test results. If the worker is unavailable or denies permission, report that failure; do not bypass it with a direct local Shell/provider."
	default:
		return ""
	}
}

func validProductSchemaVersion(value any) bool {
	switch v := value.(type) {
	case int:
		return v == 1
	case float64:
		return v == 1
	default:
		return false
	}
}
