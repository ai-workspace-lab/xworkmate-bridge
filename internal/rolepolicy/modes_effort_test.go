package rolepolicy

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// modesPolicy extends the engineer policy with the OpenClaw gateway
// executor and the four product-mode roles of the role-modes design.
func modesPolicy() *Policy {
	policy := engineerPolicy()
	policy.Limits.EffortRules = &EffortRules{LargeContextTokens: 32000, LargeContextAttachments: 3, ShortFollowUpChars: 200}
	policy.Connections["xworkmate-central"] = Connection{BaseURL: "https://central.example/v1", TokenEnv: "TEST_CENTRAL_TOKEN", AllowedDataClasses: []string{"unclassified"}}
	policy.Executors["openclaw"] = Executor{
		Kind:                ExecutorKindGateway,
		ProviderID:          "openclaw",
		Connection:          "xworkmate-central",
		ModelOptionTemplate: "xworkmate/{model_id}",
	}
	for key, model := range policy.Models {
		model.Bindings["xworkmate-central"] = ModelBinding{
			GatewayModelID: strPtr("central-" + key),
			Capabilities:   map[string]Fact{"tool_calling": verified(true)},
			ContextWindow:  verified(float64(400000)),
		}
		policy.Models[key] = model
	}
	engineer := policy.Roles[RoleEngineer]
	engineer.Executors = []string{"opencode", "deepseek-harness", "openclaw"}
	engineer.RequiresWorkspace = false
	engineer.ProductModes = []string{ProductModeCode}
	engineer.Effort = &EffortRange{Start: "high", Min: "medium", Max: "max"}
	policy.Roles[RoleEngineer] = engineer
	gatewayRole := func(start, low string) RoleProfile {
		return RoleProfile{
			Enabled:                   true,
			Preferences:               []string{"gpt-6.1-sol"},
			Executors:                 []string{"openclaw"},
			RequiredModelCapabilities: []string{"tool_calling"},
			ProductModes:              []string{ProductModeWork, ProductModeCode},
			Effort:                    &EffortRange{Start: start, Min: low, Max: "max"},
		}
	}
	policy.Roles[RoleArchitect] = gatewayRole("max", "high")
	policy.Roles[RoleResearcher] = gatewayRole("high", "medium")
	specialist := gatewayRole("high", "high")
	specialist.RequiresSpecialty = true
	policy.Roles[RoleSpecialist] = specialist
	return policy
}

func modesCatalogs() map[string]CatalogSnapshot {
	catalogs := liveCatalog("gpt-6.1-sol")
	catalogs["xworkmate-central"] = CatalogSnapshot{
		ConnectionID: "xworkmate-central",
		FetchedAt:    testNow.Add(-time.Minute),
		ModelIDs:     []string{"central-gpt-6.1-sol", "central-claude-opus-5-5"},
	}
	return catalogs
}

func modesRequest(role, productMode string) Request {
	req := engineerRequest()
	req.Role = role
	req.ProductMode = productMode
	req.AvailableProviders = []string{"opencode", "deepseek-harness", "openclaw"}
	if role == RoleSpecialist {
		req.Specialty = "tax law"
	}
	return req
}

func TestSelectEnforcesProductModeTable(t *testing.T) {
	cases := []struct {
		role, mode string
		allowed    bool
	}{
		{RoleEngineer, ProductModeChat, false},
		{RoleEngineer, ProductModeWork, false},
		{RoleEngineer, ProductModeCode, true},
		{RoleArchitect, ProductModeChat, false},
		{RoleArchitect, ProductModeWork, true},
		{RoleResearcher, ProductModeCode, true},
		{RoleSpecialist, ProductModeWork, true},
		// Clients that send no product mode keep the pre-#273 behaviour.
		{RoleEngineer, "", true},
	}
	for _, c := range cases {
		decision := Select(modesPolicy(), modesCatalogs(), modesRequest(c.role, c.mode))
		if c.allowed && !decision.Selected {
			t.Fatalf("%s in %q should be allowed, got %+v", c.role, c.mode, decision)
		}
		if !c.allowed && decision.Code != CodeRoleNotAllowedForMode {
			t.Fatalf("%s in %q should be rejected with %s, got %+v", c.role, c.mode, CodeRoleNotAllowedForMode, decision)
		}
	}
}

func TestSelectEngineerWithWorkspacePrefersAgentExecutor(t *testing.T) {
	decision := Select(modesPolicy(), modesCatalogs(), modesRequest(RoleEngineer, ProductModeCode))
	if !decision.Selected || decision.Executor != "opencode" || decision.ExecutorKind != ExecutorKindAgent {
		t.Fatalf("expected opencode agent executor, got %+v", decision)
	}
}

func TestSelectEngineerWithoutWorkspaceUsesGateway(t *testing.T) {
	req := modesRequest(RoleEngineer, ProductModeCode)
	req.WorkingDirectory = ""
	decision := Select(modesPolicy(), modesCatalogs(), req)
	if !decision.Selected || decision.Executor != "openclaw" || decision.ExecutorKind != ExecutorKindGateway {
		t.Fatalf("expected openclaw gateway executor, got %+v", decision)
	}
	if decision.ModelOption != "xworkmate/central-gpt-6.1-sol" {
		t.Fatalf("unexpected gateway model option: %+v", decision)
	}
	if !hasExclusion(decision, "gpt-6.1-sol", ExclusionWorkspaceRequired) {
		t.Fatalf("expected agent executors excluded with %s, got %+v", ExclusionWorkspaceRequired, decision.Excluded)
	}
}

func TestSelectExecutorOrderOutranksModelOrder(t *testing.T) {
	// The first-preferred model is only in the gateway catalog; the agent
	// executor still wins with the second model.
	policy := modesPolicy()
	sol := policy.Models["gpt-6.1-sol"]
	sol.Bindings["ai-internal"] = ModelBinding{GatewayModelID: nil}
	policy.Models["gpt-6.1-sol"] = sol
	opus := policy.Models["claude-opus-5-5"]
	opus.Bindings["ai-internal"] = ModelBinding{GatewayModelID: strPtr("claude-opus-5-5"), Capabilities: map[string]Fact{"tool_calling": verified(true)}, ContextWindow: verified(float64(400000))}
	policy.Models["claude-opus-5-5"] = opus
	catalogs := modesCatalogs()
	catalogs["ai-internal"] = CatalogSnapshot{ConnectionID: "ai-internal", FetchedAt: testNow.Add(-time.Minute), ModelIDs: []string{"claude-opus-5-5"}}

	decision := Select(policy, catalogs, modesRequest(RoleEngineer, ProductModeCode))
	if !decision.Selected || decision.Executor != "opencode" || decision.ModelKey != "claude-opus-5-5" {
		t.Fatalf("expected opencode with claude-opus-5-5, got %+v", decision)
	}
}

func TestSelectSkipsExecutorKindsTheCallerCannotDispatch(t *testing.T) {
	req := modesRequest(RoleArchitect, ProductModeWork)
	req.ExecutorKinds = []string{ExecutorKindAgent}
	decision := Select(modesPolicy(), modesCatalogs(), req)
	if decision.Selected || !hasExclusion(decision, "gpt-6.1-sol", ExclusionExecutorKind) {
		t.Fatalf("expected gateway executor excluded with %s, got %+v", ExclusionExecutorKind, decision)
	}
}

func TestSelectAgentCapabilitiesDoNotGateTheGateway(t *testing.T) {
	policy := modesPolicy()
	engineer := policy.Roles[RoleEngineer]
	engineer.RequiredExecutorCapabilities = []string{"workspace_edit", "test_runner", "permission_relay"}
	policy.Roles[RoleEngineer] = engineer
	req := modesRequest(RoleEngineer, ProductModeCode)
	req.WorkingDirectory = ""
	decision := Select(policy, modesCatalogs(), req)
	if !decision.Selected || decision.Executor != "openclaw" {
		t.Fatalf("gateway executor declares no agent capabilities and should still be selected, got %+v", decision)
	}
}

func TestSelectReportsResolvedEffort(t *testing.T) {
	req := modesRequest(RoleEngineer, ProductModeCode)
	req.PromptBytes = 2 * 40000
	decision := Select(modesPolicy(), modesCatalogs(), req)
	if !decision.Selected || decision.Effort == nil || decision.Effort.Level != "max" {
		t.Fatalf("expected engineer effort max for a large context, got %+v", decision.Effort)
	}
}

func TestResolveEffortRules(t *testing.T) {
	rules := EffortRules{LargeContextTokens: 32000, LargeContextAttachments: 3, ShortFollowUpChars: 200}
	engineer := EffortRange{Start: "high", Min: "medium", Max: "max"}
	architect := EffortRange{Start: "max", Min: "high", Max: "max"}
	specialist := EffortRange{Start: "high", Min: "high", Max: "max"}
	cases := []struct {
		name    string
		r       EffortRange
		signals EffortSignals
		want    string
	}{
		{"start level", engineer, EffortSignals{}, "high"},
		{"large context by tokens", engineer, EffortSignals{PromptTokens: 32001}, "max"},
		{"large context by attachments", engineer, EffortSignals{Attachments: 3}, "max"},
		{"short follow-up", engineer, EffortSignals{FollowUp: true, PromptChars: 40}, "medium"},
		{"long follow-up keeps start", engineer, EffortSignals{FollowUp: true, PromptChars: 200}, "high"},
		{"retry after failure", engineer, EffortSignals{PreviousTurnFailed: true}, "max"},
		{"architect short follow-up stays at its floor", architect, EffortSignals{FollowUp: true, PromptChars: 10}, "high"},
		{"architect large context clamps to max", architect, EffortSignals{PromptTokens: 50000}, "max"},
		{"specialist short follow-up clamps to min", specialist, EffortSignals{FollowUp: true, PromptChars: 10}, "high"},
		{"rules combine before clamping", engineer, EffortSignals{PromptTokens: 50000, FollowUp: true, PromptChars: 10, PreviousTurnFailed: true}, "max"},
	}
	for _, c := range cases {
		got := ResolveEffort(c.r, rules, c.signals)
		if got.Level != c.want {
			t.Fatalf("%s: want %s, got %+v", c.name, c.want, got)
		}
	}
	if got := ResolveEffort(specialist, rules, EffortSignals{FollowUp: true, PromptChars: 10}); len(got.Adjustments) != 2 || !strings.Contains(got.Adjustments[1], "clamped to min") {
		t.Fatalf("expected the clamp to be recorded, got %+v", got.Adjustments)
	}
}

func TestParseRejectsInvalidModesAndEffort(t *testing.T) {
	base := `{"version":"v","budget":{"mode":"disabled"},"limits":{"context_reserve_tokens":1,"catalog_max_age_seconds":1%s},
		"connections":{"c":{"base_url":"https://gateway.example/v1","token_env":"T"}},
		"executors":{"e":{"kind":"%s","provider_id":"p","connection":"c","model_option_template":"{model_id}"}},
		"roles":{"architect":%s}}`
	rules := `,"effort_rules":{"large_context_tokens":32000,"large_context_attachments":3,"short_follow_up_chars":200}`
	cases := map[string][3]string{
		"unknown executor kind":     {rules, "worker", `{"executors":["e"]}`},
		"chat is not a role mode":   {rules, "gateway", `{"executors":["e"],"product_modes":["chat"]}`},
		"effort level unknown":      {rules, "gateway", `{"executors":["e"],"effort":{"start":"ultra","min":"high","max":"max"}}`},
		"effort start below min":    {rules, "gateway", `{"executors":["e"],"effort":{"start":"medium","min":"high","max":"max"}}`},
		"effort without rules":      {"", "gateway", `{"executors":["e"],"effort":{"start":"max","min":"high","max":"max"}}`},
		"effort rules not positive": {`,"effort_rules":{"large_context_tokens":0,"large_context_attachments":3,"short_follow_up_chars":200}`, "gateway", `{"executors":["e"],"effort":{"start":"max","min":"high","max":"max"}}`},
	}
	for name, c := range cases {
		raw := fmt.Sprintf(base, c[0], c[1], c[2])
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	valid := fmt.Sprintf(base, rules, "gateway", `{"executors":["e"],"product_modes":["work","code"],"effort":{"start":"max","min":"high","max":"max"}}`)
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
}

func hasExclusion(decision Decision, model, code string) bool {
	for _, exclusion := range decision.Excluded {
		if exclusion.Model == model && exclusion.Code == code {
			return true
		}
	}
	return false
}
