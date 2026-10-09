package rolepolicy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func strPtr(value string) *string { return &value }

func verified(value any) Fact {
	return Fact{Value: value, State: StateVerified, Source: "test", CheckedAt: testNow.Add(-time.Hour), ValidUntil: testNow.Add(24 * time.Hour)}
}

func engineerPolicy() *Policy {
	return &Policy{
		Version: "test-1",
		Enabled: true,
		Budget:  Budget{Mode: BudgetModeGatewayQuota},
		Limits:  Limits{ContextReserveTokens: 16000, CatalogMaxAgeSeconds: 600},
		Connections: map[string]Connection{
			"ai-internal": {BaseURL: "https://gateway.example/v1", TokenEnv: "TEST_GATEWAY_TOKEN", AllowedDataClasses: []string{"unclassified", "internal"}},
		},
		Executors: map[string]Executor{
			"opencode": {
				Kind:                ExecutorKindAgent,
				ProviderID:          "opencode",
				Connection:          "ai-internal",
				ModelOptionTemplate: "ai-internal/{model_id}",
				Capabilities: map[string]Fact{
					"workspace_edit":   verified(true),
					"test_runner":      verified(true),
					"permission_relay": verified(true),
				},
			},
			"deepseek-harness": {
				Kind:                ExecutorKindAgent,
				ProviderID:          "deepseek-harness",
				Connection:          "ai-internal",
				ModelOptionTemplate: `["ai-internal","{model_id}"]`,
				Capabilities: map[string]Fact{
					"workspace_edit":   verified(true),
					"test_runner":      verified(true),
					"permission_relay": verified(true),
				},
			},
		},
		Models: map[string]ModelEntry{
			"gpt-6.1-sol": {DisplayName: "GPT 6.1 Sol", Bindings: map[string]ModelBinding{
				"ai-internal": {GatewayModelID: strPtr("gpt-6.1-sol"), Capabilities: map[string]Fact{"tool_calling": verified(true)}, ContextWindow: verified(float64(400000))},
			}},
			"claude-opus-5-5": {DisplayName: "Claude Opus 5.5", Bindings: map[string]ModelBinding{
				"ai-internal": {GatewayModelID: nil},
			}},
		},
		Roles: map[string]RoleProfile{
			RoleEngineer: {
				Enabled:                      true,
				Preferences:                  []string{"gpt-6.1-sol", "claude-opus-5-5"},
				Executors:                    []string{"opencode", "deepseek-harness"},
				RequiredModelCapabilities:    []string{"tool_calling"},
				RequiredExecutorCapabilities: []string{"workspace_edit", "test_runner", "permission_relay"},
				RequiresWorkspace:            true,
				Tools:                        []string{"read", "edit", "bash"},
			},
			RoleResearcher: {Enabled: false},
		},
	}
}

func liveCatalog(ids ...string) map[string]CatalogSnapshot {
	return map[string]CatalogSnapshot{"ai-internal": {ConnectionID: "ai-internal", FetchedAt: testNow.Add(-time.Minute), ModelIDs: ids}}
}

func engineerRequest() Request {
	return Request{
		Role:               RoleEngineer,
		WorkingDirectory:   "/srv/workspace/demo",
		PromptBytes:        2000,
		AvailableProviders: []string{"opencode", "deepseek-harness"},
		ExecutorKinds:      []string{ExecutorKindAgent, ExecutorKindGateway},
		Now:                testNow,
	}
}

func TestSelectEngineerPicksFirstPreferenceThatPassesGates(t *testing.T) {
	decision := Select(engineerPolicy(), liveCatalog("gpt-6.1-sol", "other"), engineerRequest())
	if !decision.Selected {
		t.Fatalf("expected selection, got %+v", decision)
	}
	if decision.Executor != "opencode" || decision.ModelID != "gpt-6.1-sol" || decision.ModelOption != "ai-internal/gpt-6.1-sol" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if decision.RoleMode != ModeAuto || decision.PolicyVersion != "test-1" {
		t.Fatalf("missing mode/version: %+v", decision)
	}
}

func TestSelectNewCatalogIDWithoutEvidenceIsNotSelected(t *testing.T) {
	policy := engineerPolicy()
	policy.Roles[RoleEngineer] = withPreferences(policy.Roles[RoleEngineer], "claude-opus-5-5")
	decision := Select(policy, liveCatalog("gpt-6.1-sol", "claude-opus-5-5"), engineerRequest())
	if decision.Selected || decision.Code != CodeNoEligibleCandidate {
		t.Fatalf("expected rejection, got %+v", decision)
	}
	assertExcluded(t, decision, "claude-opus-5-5", ExclusionPendingMapping)
}

func TestSelectRejectsModelMissingFromLiveCatalog(t *testing.T) {
	decision := Select(engineerPolicy(), liveCatalog("something-else"), engineerRequest())
	if decision.Selected {
		t.Fatalf("expected rejection, got %+v", decision)
	}
	assertExcluded(t, decision, "gpt-6.1-sol", ExclusionNotInLiveCatalog)
}

func TestSelectDoesNotNormalizeModelIDPunctuation(t *testing.T) {
	decision := Select(engineerPolicy(), liveCatalog("gpt-6-1-sol", "GPT-6.1-SOL"), engineerRequest())
	if decision.Selected {
		t.Fatalf("punctuation/case variants must not match: %+v", decision)
	}
}

func TestSelectRejectsStaleOrFailedCatalog(t *testing.T) {
	stale := liveCatalog("gpt-6.1-sol")
	snapshot := stale["ai-internal"]
	snapshot.FetchedAt = testNow.Add(-time.Hour)
	stale["ai-internal"] = snapshot
	assertExcluded(t, Select(engineerPolicy(), stale, engineerRequest()), "gpt-6.1-sol", ExclusionCatalogStale)

	failed := map[string]CatalogSnapshot{"ai-internal": {ConnectionID: "ai-internal", FetchedAt: testNow, Err: "GET returned 401"}}
	assertExcluded(t, Select(engineerPolicy(), failed, engineerRequest()), "gpt-6.1-sol", ExclusionCatalogUnavailable)
}

func TestSelectEngineerWithoutWorkspaceIsRejected(t *testing.T) {
	req := engineerRequest()
	req.WorkingDirectory = ""
	decision := Select(engineerPolicy(), liveCatalog("gpt-6.1-sol"), req)
	if decision.Code != CodeWorkspaceUnauthorized {
		t.Fatalf("expected workspace rejection, got %+v", decision)
	}
}

func TestSelectExecutorCapabilityMustBeVerified(t *testing.T) {
	policy := engineerPolicy()
	for id, exec := range policy.Executors {
		exec.Capabilities = map[string]Fact{
			"workspace_edit":   verified(true),
			"test_runner":      {Value: true, State: StateDeclared},
			"permission_relay": verified(true),
		}
		policy.Executors[id] = exec
	}
	decision := Select(policy, liveCatalog("gpt-6.1-sol"), engineerRequest())
	if decision.Selected {
		t.Fatalf("declared capability must not pass: %+v", decision)
	}
	assertExcluded(t, decision, "gpt-6.1-sol", ExclusionExecutorCapability)
}

func TestSelectExpiredEvidenceFallsBackToUnknown(t *testing.T) {
	policy := engineerPolicy()
	model := policy.Models["gpt-6.1-sol"]
	binding := model.Bindings["ai-internal"]
	binding.Capabilities = map[string]Fact{"tool_calling": {Value: true, State: StateVerified, ValidUntil: testNow.Add(-time.Second)}}
	model.Bindings["ai-internal"] = binding
	policy.Models["gpt-6.1-sol"] = model
	decision := Select(policy, liveCatalog("gpt-6.1-sol"), engineerRequest())
	assertExcluded(t, decision, "gpt-6.1-sol", ExclusionCapabilityUnverified)
}

func TestSelectContextGate(t *testing.T) {
	req := engineerRequest()
	req.PromptBytes = 2 * 400000
	assertExcluded(t, Select(engineerPolicy(), liveCatalog("gpt-6.1-sol"), req), "gpt-6.1-sol", ExclusionContextInsufficient)

	policy := engineerPolicy()
	model := policy.Models["gpt-6.1-sol"]
	binding := model.Bindings["ai-internal"]
	binding.ContextWindow = Fact{State: StateUnknown}
	model.Bindings["ai-internal"] = binding
	policy.Models["gpt-6.1-sol"] = model
	assertExcluded(t, Select(policy, liveCatalog("gpt-6.1-sol"), engineerRequest()), "gpt-6.1-sol", ExclusionContextUnknown)
}

func TestSelectEgressGate(t *testing.T) {
	req := engineerRequest()
	req.DataClass = "confidential"
	assertExcluded(t, Select(engineerPolicy(), liveCatalog("gpt-6.1-sol"), req), "gpt-6.1-sol", ExclusionEgressNotAllowed)
}

func TestSelectExecutorUnavailableMovesToNextExecutor(t *testing.T) {
	req := engineerRequest()
	req.AvailableProviders = []string{"deepseek-harness"}
	decision := Select(engineerPolicy(), liveCatalog("gpt-6.1-sol"), req)
	if !decision.Selected || decision.Executor != "deepseek-harness" || decision.ModelOption != `["ai-internal","gpt-6.1-sol"]` {
		t.Fatalf("expected deepseek-harness selection, got %+v", decision)
	}
	assertExcluded(t, decision, "gpt-6.1-sol", ExclusionExecutorUnavailable)
}

func TestSelectBudgetDisabledRefusesDispatch(t *testing.T) {
	policy := engineerPolicy()
	policy.Budget.Mode = BudgetModeDisabled
	decision := Select(policy, liveCatalog("gpt-6.1-sol"), engineerRequest())
	if decision.Code != CodeBudgetUnconfigured {
		t.Fatalf("expected budget rejection, got %+v", decision)
	}
}

func TestSelectRoleGates(t *testing.T) {
	policy := engineerPolicy()
	req := engineerRequest()
	req.Role = RoleResearcher
	if decision := Select(policy, liveCatalog("gpt-6.1-sol"), req); decision.Code != CodeRoleNotEnabled {
		t.Fatalf("expected role_not_enabled, got %+v", decision)
	}
	req.Role = "wizard"
	if decision := Select(policy, liveCatalog("gpt-6.1-sol"), req); decision.Code != CodeRoleUnknown {
		t.Fatalf("expected role_unknown, got %+v", decision)
	}
	req.Role = ""
	decision := Select(policy, liveCatalog("gpt-6.1-sol"), req)
	if !decision.Selected || decision.Role != RoleEngineer {
		t.Fatalf("auto with one enabled role should pick it, got %+v", decision)
	}
	req.RoleMode = ModeManual
	req.ManualModel = "gpt-6.1-sol"
	if decision := Select(policy, liveCatalog("gpt-6.1-sol"), req); decision.Code != CodeRoleRequired {
		t.Fatalf("manual mode must name a role, got %+v", decision)
	}
	policy.Enabled = false
	if decision := Select(policy, liveCatalog("gpt-6.1-sol"), engineerRequest()); decision.Code != CodePolicyDisabled {
		t.Fatalf("expected policy_disabled, got %+v", decision)
	}
}

func TestSelectManualModelGoesThroughSameGates(t *testing.T) {
	req := engineerRequest()
	req.RoleMode = ModeManual
	req.ManualModel = "claude-opus-5-5"
	decision := Select(engineerPolicy(), liveCatalog("gpt-6.1-sol"), req)
	if decision.Selected {
		t.Fatalf("manual pick of unmapped model must be rejected: %+v", decision)
	}
	assertExcluded(t, decision, "claude-opus-5-5", ExclusionPendingMapping)

	req.ManualModel = "not-registered"
	if decision := Select(engineerPolicy(), liveCatalog("gpt-6.1-sol"), req); decision.Code != CodeModelUnknown {
		t.Fatalf("expected model_unknown, got %+v", decision)
	}
}

func TestSelectSpecialistRequiresSpecialty(t *testing.T) {
	policy := engineerPolicy()
	policy.Roles[RoleSpecialist] = RoleProfile{Enabled: true, RequiresSpecialty: true}
	req := engineerRequest()
	req.Role = RoleSpecialist
	if decision := Select(policy, liveCatalog("gpt-6.1-sol"), req); decision.Code != CodeSpecialtyRequired {
		t.Fatalf("expected specialty_required, got %+v", decision)
	}
}

func TestExamplePolicyLoadsAndStaysDisabled(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "example", "role-router-policy.example.json")
	policy, err := Load(path)
	if err != nil {
		t.Fatalf("load example policy: %v", err)
	}
	if policy.Enabled || policy.Budget.Mode != BudgetModeDisabled {
		t.Fatalf("example policy must ship disabled: enabled=%v budget=%s", policy.Enabled, policy.Budget.Mode)
	}
	for key, model := range policy.Models {
		for conn, binding := range model.Bindings {
			if binding.GatewayModelID != nil {
				t.Fatalf("example must not claim an observed gateway id: %s on %s", key, conn)
			}
		}
	}
	decision := Select(policy, nil, engineerRequest())
	if decision.Code != CodePolicyDisabled {
		t.Fatalf("expected policy_disabled for example, got %+v", decision)
	}
}

func TestExamplePolicyMatchesRoleModesDesign(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	policy, err := Load(filepath.Join(filepath.Dir(file), "..", "..", "example", "role-router-policy.example.json"))
	if err != nil {
		t.Fatalf("load example policy: %v", err)
	}
	want := map[string]struct {
		modes     string
		effort    EffortRange
		executors string
	}{
		RoleEngineer:   {"code", EffortRange{Start: "high", Min: "medium", Max: "max"}, "opencode,deepseek-harness,openclaw"},
		RoleArchitect:  {"work,code", EffortRange{Start: "max", Min: "high", Max: "max"}, "openclaw"},
		RoleResearcher: {"work,code", EffortRange{Start: "high", Min: "medium", Max: "max"}, "openclaw"},
		RoleSpecialist: {"work,code", EffortRange{Start: "high", Min: "high", Max: "max"}, "openclaw"},
	}
	for roleID, w := range want {
		role := policy.Roles[roleID]
		if got := strings.Join(role.ProductModes, ","); got != w.modes {
			t.Fatalf("%s product modes: want %s, got %s", roleID, w.modes, got)
		}
		if role.Effort == nil || *role.Effort != w.effort {
			t.Fatalf("%s effort: want %+v, got %+v", roleID, w.effort, role.Effort)
		}
		if got := strings.Join(role.Executors, ","); got != w.executors {
			t.Fatalf("%s executors: want %s, got %s", roleID, w.executors, got)
		}
	}
	if policy.Executors["openclaw"].Kind != ExecutorKindGateway || policy.Executors["opencode"].Kind != ExecutorKindAgent {
		t.Fatalf("unexpected executor kinds: %+v", policy.Executors)
	}
}

func TestParseRejectsBrokenReferences(t *testing.T) {
	cases := map[string]string{
		"unknown executor": `{"version":"v","budget":{"mode":"disabled"},"limits":{"context_reserve_tokens":1,"catalog_max_age_seconds":1},"roles":{"engineer":{"executors":["missing"]}}}`,
		"unknown model":    `{"version":"v","budget":{"mode":"disabled"},"limits":{"context_reserve_tokens":1,"catalog_max_age_seconds":1},"roles":{"engineer":{"preferences":["missing"]}}}`,
		"bad budget":       `{"version":"v","budget":{"mode":"hard"},"limits":{"context_reserve_tokens":1,"catalog_max_age_seconds":1}}`,
		"unknown role":     `{"version":"v","budget":{"mode":"disabled"},"limits":{"context_reserve_tokens":1,"catalog_max_age_seconds":1},"roles":{"boss":{}}}`,
		"no base url":      `{"version":"v","budget":{"mode":"disabled"},"limits":{"context_reserve_tokens":1,"catalog_max_age_seconds":1},"connections":{"c":{"token_env":"T"}}}`,
		"two base urls":    `{"version":"v","budget":{"mode":"disabled"},"limits":{"context_reserve_tokens":1,"catalog_max_age_seconds":1},"connections":{"c":{"base_url":"https://gateway.example/v1","base_url_env":"U","token_env":"T"}}}`,
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestFetchCatalogKeepsExactIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6.1-sol"},{"id":"Grok4.7"},{"id":""}]}`))
	}))
	defer server.Close()
	t.Setenv("TEST_GATEWAY_TOKEN", "secret-token")
	conn := Connection{BaseURL: server.URL + "/v1/", TokenEnv: "TEST_GATEWAY_TOKEN"}
	snapshot := FetchCatalog(context.Background(), server.Client(), "ai-internal", conn, testNow)
	if snapshot.Err != "" || strings.Join(snapshot.ModelIDs, ",") != "gpt-6.1-sol,Grok4.7" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}

	t.Setenv("TEST_GATEWAY_TOKEN", "wrong")
	if snapshot := FetchCatalog(context.Background(), server.Client(), "ai-internal", conn, testNow); snapshot.Err == "" {
		t.Fatalf("expected error on 401")
	}
	if err := os.Unsetenv("TEST_GATEWAY_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if snapshot := FetchCatalog(context.Background(), server.Client(), "ai-internal", conn, testNow); !strings.Contains(snapshot.Err, "TEST_GATEWAY_TOKEN") {
		t.Fatalf("expected missing token error, got %+v", snapshot)
	}
}

func TestFetchCatalogReadsBaseURLFromEnv(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6.1-sol"}]}`))
	}))
	defer server.Close()
	t.Setenv("TEST_GATEWAY_TOKEN", "secret-token")
	conn := Connection{BaseURLEnv: "TEST_GATEWAY_BASE_URL", TokenEnv: "TEST_GATEWAY_TOKEN"}
	if snapshot := FetchCatalog(context.Background(), server.Client(), "ai-internal", conn, testNow); !strings.Contains(snapshot.Err, "TEST_GATEWAY_BASE_URL") {
		t.Fatalf("expected missing base url error, got %+v", snapshot)
	}
	t.Setenv("TEST_GATEWAY_BASE_URL", server.URL+"/v1")
	snapshot := FetchCatalog(context.Background(), server.Client(), "ai-internal", conn, testNow)
	if snapshot.Err != "" || strings.Join(snapshot.ModelIDs, ",") != "gpt-6.1-sol" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func withPreferences(role RoleProfile, prefs ...string) RoleProfile {
	role.Preferences = prefs
	return role
}

func assertExcluded(t *testing.T, decision Decision, model, code string) {
	t.Helper()
	for _, exclusion := range decision.Excluded {
		if exclusion.Model == model && exclusion.Code == code {
			return
		}
	}
	t.Fatalf("expected %s excluded with %s, got %+v", model, code, decision.Excluded)
}
