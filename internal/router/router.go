package router

import (
	"os"
	"strings"

	"xworkmate-bridge/internal/memory"
	"xworkmate-bridge/internal/skills"
)

const (
	RoutingModeAuto     = "auto"
	RoutingModeExplicit = "explicit"

	ExecutionTargetSingleAgent = "single-agent"
	ExecutionTargetGateway     = "gateway"
	ExecutionTargetGatewayChat = "gateway-chat"

	GatewayProviderOpenClaw = "openclaw"
)

type Request struct {
	Prompt                     string
	WorkingDirectory           string
	RoutingMode                string
	PreferredGatewayProviderID string
	ExplicitExecutionTarget    string
	ExplicitProviderID         string
	ExplicitModel              string
	ExplicitSkills             []string
	AllowSkillInstall          bool
	InstallApproval            skills.InstallApproval
	AvailableSkills            []skills.Candidate
	AvailableProviders         []string
	AIGatewayBaseURL           string
	AIGatewayAPIKey            string
}

type Result struct {
	ResolvedExecutionTarget   string
	ResolvedProviderID        string
	ResolvedGatewayProviderID string
	ResolvedModel             string
	ResolvedSkills            []string
	SkillResolutionSource     string
	SkillCandidates           []skills.Candidate
	NeedsSkillInstall         bool
	SkillInstallRequestID     string
	MemorySources             []memory.Source
	Unavailable               bool
	UnavailableCode           string
	UnavailableMessage        string
}

type Resolver struct {
	SkillFinder    skills.Finder
	SkillInstaller skills.Installer
	MemoryService  memory.Service
	Classifier     Classifier
}

func NewResolver() Resolver {
	homeDir, _ := os.UserHomeDir()
	return Resolver{
		SkillFinder:    skills.NewDefaultFinder(),
		SkillInstaller: skills.NewDefaultInstaller(),
		MemoryService:  memory.NewService(homeDir),
		Classifier:     LLMClassifier{},
	}
}

func (r Resolver) Resolve(req Request) Result {
	mem := r.MemoryService.Load(req.WorkingDirectory)
	availableProviders := normalizeProviders(req.AvailableProviders)

	result := Result{
		ResolvedModel: strings.TrimSpace(req.ExplicitModel),
		MemorySources: mem.Sources,
	}

	result.ResolvedExecutionTarget, result.ResolvedGatewayProviderID = r.resolveExecution(req, mem.Preferences)
	result.ResolvedProviderID, result.Unavailable, result.UnavailableCode, result.UnavailableMessage = resolveProvider(
		req,
		mem.Preferences,
		availableProviders,
		result.ResolvedExecutionTarget,
	)
	if result.ResolvedModel == "" {
		result.ResolvedModel = strings.TrimSpace(mem.Preferences.PreferredModel)
	}

	skillRequest := skills.ResolveRequest{
		Prompt:            req.Prompt,
		ExplicitSkills:    req.ExplicitSkills,
		AvailableSkills:   req.AvailableSkills,
		AllowSkillInstall: req.AllowSkillInstall,
		InstallApproval:   req.InstallApproval,
	}
	skillResult := skills.Resolve(skillRequest, r.SkillFinder, r.SkillInstaller)
	result.ResolvedSkills = skillResult.ResolvedSkills
	result.SkillResolutionSource = skillResult.Source
	result.SkillCandidates = skillResult.Candidates
	result.NeedsSkillInstall = skillResult.NeedsInstall
	result.SkillInstallRequestID = skillResult.InstallRequestID

	if len(result.ResolvedSkills) == 0 && len(mem.Preferences.PreferredSkills) > 0 {
		result.ResolvedSkills = append([]string(nil), mem.Preferences.PreferredSkills...)
		if result.SkillResolutionSource == "" || result.SkillResolutionSource == "none" {
			result.SkillResolutionSource = "local_match"
		}
	}
	if result.SkillResolutionSource == "" {
		result.SkillResolutionSource = "none"
	}
	if result.ResolvedExecutionTarget == "" {
		if len(availableProviders) > 0 {
			result.ResolvedExecutionTarget = ExecutionTargetSingleAgent
		} else {
			result.ResolvedExecutionTarget = ExecutionTargetGateway
		}
	}
	if result.ResolvedExecutionTarget == ExecutionTargetGateway &&
		result.ResolvedGatewayProviderID == "" {
		result.ResolvedGatewayProviderID = resolveGatewayProvider(
			req.PreferredGatewayProviderID,
		)
	}
	return result
}

func (r Resolver) resolveExecution(req Request, prefs memory.Preferences) (string, string) {
	explicit := strings.TrimSpace(req.ExplicitExecutionTarget)
	if strings.EqualFold(strings.TrimSpace(req.RoutingMode), RoutingModeExplicit) && explicit != "" {
		return mapExplicitTarget(
			explicit,
			req.PreferredGatewayProviderID,
		)
	}

	prompt := normalize(req.Prompt)

	localTask := looksLocal(prompt)
	onlineTask := looksOnline(prompt)

	switch {
	case localTask:
		return ExecutionTargetSingleAgent, ""
	case onlineTask:
		return ExecutionTargetGateway, resolveGatewayProvider(
			req.PreferredGatewayProviderID,
		)
	}

	switch normalizeExecutionTarget(r.classify(req)) {
	case ExecutionTargetGateway:
		return ExecutionTargetGateway, resolveGatewayProvider(
			req.PreferredGatewayProviderID,
		)
	case ExecutionTargetSingleAgent:
		return ExecutionTargetSingleAgent, ""
	}

	switch normalizeExecutionTarget(strings.TrimSpace(prefs.PreferredRoute)) {
	case ExecutionTargetGateway:
		return ExecutionTargetGateway, resolveGatewayProvider(
			req.PreferredGatewayProviderID,
		)
	case ExecutionTargetSingleAgent:
		if len(normalizeProviders(req.AvailableProviders)) > 0 {
			return ExecutionTargetSingleAgent, ""
		}
	}
	if len(normalizeProviders(req.AvailableProviders)) > 0 {
		return ExecutionTargetSingleAgent, ""
	}
	return ExecutionTargetGateway, resolveGatewayProvider(
		req.PreferredGatewayProviderID,
	)
}

func (r Resolver) classify(req Request) string {
	if r.Classifier == nil {
		return ""
	}
	return normalizeExecutionTarget(r.Classifier.Classify(ClassificationRequest{
		Prompt:           req.Prompt,
		AIGatewayBaseURL: req.AIGatewayBaseURL,
		AIGatewayAPIKey:  req.AIGatewayAPIKey,
	}))
}

func mapExplicitTarget(
	value string,
	preferredGatewayProviderID string,
) (string, string) {
	switch strings.TrimSpace(value) {
	case "singleAgent", ExecutionTargetSingleAgent:
		return ExecutionTargetSingleAgent, ""
	case ExecutionTargetGateway:
		return ExecutionTargetGateway, resolveGatewayProvider(
			preferredGatewayProviderID,
		)
	default:
		return ExecutionTargetSingleAgent, ""
	}
}

func resolveGatewayProvider(preferredGatewayProviderID string) string {
	providerID := normalizeGatewayProvider(preferredGatewayProviderID)
	if providerID == "" {
		providerID = GatewayProviderOpenClaw
	}
	return providerID
}

func normalizeGatewayProvider(value string) string {
	switch normalize(value) {
	case GatewayProviderOpenClaw:
		return GatewayProviderOpenClaw
	default:
		return ""
	}
}

func resolveProvider(
	req Request,
	prefs memory.Preferences,
	availableProviders []string,
	executionTarget string,
) (string, bool, string, string) {
	if executionTarget != ExecutionTargetSingleAgent {
		preferredProvider := normalize(strings.TrimSpace(prefs.Provider))
		if containsProvider(availableProviders, preferredProvider) {
			return preferredProvider, false, "", ""
		}
		return "", false, "", ""
	}

	explicitProviderID := normalize(strings.TrimSpace(req.ExplicitProviderID))
	if explicitProviderID != "" {
		if containsProvider(availableProviders, explicitProviderID) {
			return explicitProviderID, false, "", ""
		}
		if len(availableProviders) == 1 {
			return availableProviders[0], false, "", ""
		}
		return "", true, "PROVIDER_UNAVAILABLE", "explicit provider is unavailable"
	}

	preferredProvider := normalize(strings.TrimSpace(prefs.Provider))
	if containsProvider(availableProviders, preferredProvider) {
		return preferredProvider, false, "", ""
	}
	if len(availableProviders) > 0 {
		return availableProviders[0], false, "", ""
	}
	return "", true, "PROVIDER_UNAVAILABLE", "no single-agent provider is available"
}

func normalizeProviders(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	unique := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		providerID := normalize(value)
		if providerID == "" {
			continue
		}
		if _, ok := unique[providerID]; ok {
			continue
		}
		unique[providerID] = struct{}{}
		normalized = append(normalized, providerID)
	}
	return normalized
}

func containsProvider(values []string, want string) bool {
	want = normalize(want)
	if want == "" {
		return false
	}
	for _, value := range values {
		if normalize(value) == want {
			return true
		}
	}
	return false
}

func looksLocal(prompt string) bool {
	return containsAny(prompt, []string{
		"ppt", "pptx", "powerpoint", "word", "docx", "excel", "xlsx", "pdf",
		"image-resizer", "resize image", "compress image", "crop image",
	})
}

func looksOnline(prompt string) bool {
	return containsAny(prompt, []string{
		"image-cog", "wan", "video-translator", "browser", "search", "news",
		"资讯采集", "跨浏览器", "文生图", "文生视频", "图生视频", "视频翻译",
		"translate video", "dub video", "subtitles",
	})
}

func containsAny(haystack string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, normalize(needle)) {
			return true
		}
	}
	return false
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeExecutionTarget(value string) string {
	switch normalize(value) {
	case ExecutionTargetGatewayChat:
		return ExecutionTargetGateway
	default:
		return normalize(value)
	}
}
