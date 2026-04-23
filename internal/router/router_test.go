package router

import (
	"testing"

	"xworkmate-bridge/internal/memory"
	"xworkmate-bridge/internal/skills"
)

type fakeClassifier string

func (f fakeClassifier) Classify(req ClassificationRequest) string {
	return string(f)
}

func TestResolveExplicitTargetOverridesAuto(t *testing.T) {
	resolver := Resolver{
		SkillFinder:    skills.StaticFinder{},
		SkillInstaller: nil,
		MemoryService:  memory.Service{},
	}

	result := resolver.Resolve(Request{
		Prompt:                  "search the web and summarize results",
		RoutingMode:             RoutingModeExplicit,
		ExplicitExecutionTarget: "singleAgent",
		ExplicitProviderID:      "codex",
		ExplicitModel:           "gpt-5.4",
		AvailableProviders:      []string{"codex"},
	})

	if result.ResolvedExecutionTarget != ExecutionTargetSingleAgent {
		t.Fatalf("expected explicit single-agent route, got %#v", result)
	}
	if result.ResolvedProviderID != "codex" || result.ResolvedModel != "gpt-5.4" {
		t.Fatalf("unexpected explicit provider/model: %#v", result)
	}
}

func TestResolveExplicitProviderRequiresAvailability(t *testing.T) {
	resolver := Resolver{
		SkillFinder:    skills.StaticFinder{},
		SkillInstaller: nil,
		MemoryService:  memory.Service{},
	}

	result := resolver.Resolve(Request{
		Prompt:                  "search the web and summarize results",
		RoutingMode:             RoutingModeExplicit,
		ExplicitExecutionTarget: "singleAgent",
		ExplicitProviderID:      "codex",
	})

	if !result.Unavailable {
		t.Fatalf("expected explicit provider to be unavailable without synced catalog, got %#v", result)
	}
	if result.UnavailableCode != "PROVIDER_UNAVAILABLE" {
		t.Fatalf("expected PROVIDER_UNAVAILABLE, got %#v", result)
	}
}

func TestResolveExplicitGatewayIgnoresExplicitProvider(t *testing.T) {
	resolver := Resolver{
		SkillFinder:    skills.StaticFinder{},
		SkillInstaller: nil,
		MemoryService:  memory.Service{},
	}

	result := resolver.Resolve(Request{
		Prompt:                     "search the web and summarize results",
		RoutingMode:                RoutingModeExplicit,
		ExplicitExecutionTarget:    ExecutionTargetGateway,
		ExplicitProviderID:         "codex",
		PreferredGatewayProviderID: GatewayProviderOpenClaw,
	})

	if result.Unavailable {
		t.Fatalf("expected explicit gateway route to ignore explicit provider, got %#v", result)
	}
	if result.ResolvedExecutionTarget != ExecutionTargetGateway {
		t.Fatalf("expected gateway route, got %#v", result)
	}
	if result.ResolvedGatewayProviderID != GatewayProviderOpenClaw {
		t.Fatalf("expected openclaw gateway provider, got %#v", result)
	}
	if result.ResolvedProviderID != "" {
		t.Fatalf("expected no single-agent provider on gateway route, got %#v", result)
	}
}

func TestResolveAutoLocalTaskToSingleAgent(t *testing.T) {
	resolver := Resolver{
		SkillFinder:    skills.StaticFinder{},
		SkillInstaller: nil,
		MemoryService:  memory.Service{},
	}

	result := resolver.Resolve(Request{
		Prompt: "create a PowerPoint deck from this outline",
	})

	if result.ResolvedExecutionTarget != ExecutionTargetSingleAgent {
		t.Fatalf("expected single-agent route, got %#v", result)
	}
}

func TestResolveAutoOnlineTaskToGateway(t *testing.T) {
	resolver := Resolver{
		SkillFinder:    skills.StaticFinder{},
		SkillInstaller: nil,
		MemoryService:  memory.Service{},
	}

	result := resolver.Resolve(Request{
		Prompt:                     "跨浏览器执行并搜索最新资讯",
		PreferredGatewayProviderID: GatewayProviderOpenClaw,
	})

	if result.ResolvedExecutionTarget != ExecutionTargetGateway {
		t.Fatalf("expected gateway route, got %#v", result)
	}
	if result.ResolvedGatewayProviderID != GatewayProviderOpenClaw {
		t.Fatalf("expected openclaw gateway provider, got %#v", result)
	}
}

func TestResolveComplexTaskNoLongerPromotesToMultiAgent(t *testing.T) {
	resolver := Resolver{
		SkillFinder:    skills.StaticFinder{},
		SkillInstaller: nil,
		MemoryService:  memory.Service{},
	}

	result := resolver.Resolve(Request{
		Prompt:              "analyze these files, review the output, and summarize multiple deliverables",
		AvailableProviders:  []string{"codex"},
	})

	if result.ResolvedExecutionTarget != ExecutionTargetSingleAgent {
		t.Fatalf("expected single-agent route after control-plane refocus, got %#v", result)
	}
}

func TestResolveUsesClassifierForBoundarySamples(t *testing.T) {
	resolver := Resolver{
		SkillFinder:    skills.StaticFinder{},
		SkillInstaller: nil,
		MemoryService:  memory.Service{},
		Classifier:     fakeClassifier(ExecutionTargetGateway),
	}

	result := resolver.Resolve(Request{
		Prompt:                     "help me handle this ambiguous request",
		PreferredGatewayProviderID: GatewayProviderOpenClaw,
	})

	if result.ResolvedExecutionTarget != ExecutionTargetGateway {
		t.Fatalf("expected classifier to resolve gateway route, got %#v", result)
	}
	if result.ResolvedGatewayProviderID != GatewayProviderOpenClaw {
		t.Fatalf("expected openclaw gateway provider, got %#v", result)
	}
}

func TestResolveGatewayProviderSelectsOpenClaw(t *testing.T) {
	resolver := Resolver{
		SkillFinder:    skills.StaticFinder{},
		SkillInstaller: nil,
		MemoryService:  memory.Service{},
	}

	result := resolver.Resolve(Request{
		Prompt:                     "search the web for latest news",
		PreferredGatewayProviderID: GatewayProviderOpenClaw,
	})

	if result.ResolvedExecutionTarget != ExecutionTargetGateway {
		t.Fatalf("expected gateway route, got %#v", result)
	}
	if result.ResolvedGatewayProviderID != GatewayProviderOpenClaw {
		t.Fatalf("expected openclaw gateway provider, got %#v", result)
	}
}
