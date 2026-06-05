package acp

import (
	"strings"
	"time"

	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

const (
	openClawShortTaskMinutes   = 10
	openClawLongTaskMinutes    = 30
	openClawComplexTaskMinutes = 60
)

type OpenClawTaskRecord struct {
	SessionID            string
	ThreadID             string
	TurnID               string
	RunID                string
	SessionKey           string
	GatewayProviderID    string
	TaskLoadClass        string
	RuntimeBudgetMinutes int
	StartedAt            time.Time
	DeadlineAt           time.Time
	ProgressStage        string
	ProgressMessage      string
	PreparedArtifact     *openClawPreparedArtifactScope
	ResolvedModel        string
	ResolvedSkills       []string
}

func openClawTaskRuntimePolicy(params map[string]any, chatParams map[string]any, contract openClawArtifactContract) (string, int) {
	message := strings.TrimSpace(shared.StringArg(chatParams, "message", ""))
	if message == "" {
		message = openClawCurrentTurnMessage(params)
	}
	lower := strings.ToLower(message)
	taskLoadClass := strings.TrimSpace(contract.TaskLoadClass)
	if taskLoadClass == "" {
		taskLoadClass = strings.TrimSpace(shared.StringArg(params, "taskLoadClass", ""))
	}
	metadataClass := strings.TrimSpace(shared.StringArg(shared.AsMap(params["metadata"]), "taskLoadClass", ""))
	if taskLoadClass == "" {
		taskLoadClass = metadataClass
	}
	switch taskLoadClass {
	case "short_task":
		return "short_task", openClawShortTaskMinutes
	case "long_task":
		return "long_task", openClawLongTaskMinutes
	case "complex_chain_task", "complex_long_chain_task":
		return "complex_chain_task", openClawComplexTaskMinutes
	}
	if metadataClass == "complex_long_chain_task" || contract.ComplexLongChain || openClawMessageContainsAny(lower, []string{
		"复杂链路", "多章节", "每章", "拆章节", "汇总排版", "gpt images", "images2", "image generation", "视频", "渲染", "hyperframes", "remotion", "ffmpeg",
	}) {
		return "complex_chain_task", openClawComplexTaskMinutes
	}
	if openClawMessageContainsAny(lower, []string{
		"生成文件", "同步生成文件", "产物", "附件", "pdf", "docx", "ppt", "pptx", "markdown", ".md", "png", "jpg", "jpeg", "mp4",
	}) || len(shared.ListArg(params, "attachments"))+len(shared.ListArg(params, "inlineAttachments")) >= 2 {
		return "long_task", openClawLongTaskMinutes
	}
	return "short_task", openClawShortTaskMinutes
}

func openClawRunningTaskResult(record *OpenClawTaskRecord) map[string]any {
	if record == nil {
		return map[string]any{"success": true, "status": "running", "mode": router.ExecutionTargetGatewayChat}
	}
	result := map[string]any{
		"success":                   true,
		"status":                    string(TaskStateRunning),
		"turnId":                    record.TurnID,
		"runId":                     record.RunID,
		"sessionId":                 record.SessionID,
		"threadId":                  record.ThreadID,
		"appThreadKey":              record.ThreadID,
		"openclawSessionKey":        record.SessionKey,
		"mode":                      router.ExecutionTargetGatewayChat,
		"resolvedGatewayProviderId": record.GatewayProviderID,
		"taskLoadClass":             record.TaskLoadClass,
		"runtimeBudgetMinutes":      record.RuntimeBudgetMinutes,
		"startedAt":                 record.StartedAt.UTC().Format(time.RFC3339Nano),
		"deadlineAt":                record.DeadlineAt.UTC().Format(time.RFC3339Nano),
		"progress":                  openClawTaskProgress(record),
	}
	if record.PreparedArtifact != nil {
		applyOpenClawPreparedArtifactToResult(result, record.PreparedArtifact)
	}
	return result
}

func openClawTaskProgress(record *OpenClawTaskRecord) map[string]any {
	now := time.Now()
	stage := strings.TrimSpace(record.ProgressStage)
	if stage == "" {
		stage = "running"
	}
	message := strings.TrimSpace(record.ProgressMessage)
	if message == "" {
		message = "OpenClaw task is running"
	}
	return map[string]any{
		"stage":     stage,
		"message":   message,
		"elapsedMs": maxInt64(0, now.Sub(record.StartedAt).Milliseconds()),
		"budgetMs":  (time.Duration(record.RuntimeBudgetMinutes) * time.Minute).Milliseconds(),
		"terminal":  false,
	}
}

func maxInt64(a int64, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
