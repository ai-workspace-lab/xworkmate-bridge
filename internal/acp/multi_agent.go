package acp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xworkmate-bridge/internal/shared"
)

const (
	multiAgentTargetID       = "multi-agent"
	defaultMultiAgentTimeout = 5 * time.Minute
)

type multiAgentPlan struct {
	Mode            string
	Steps           []multiAgentStep
	MaxTurns        int
	StopConditions  []string
	SharedWorkspace string
}

type multiAgentStep struct {
	Index      int
	ProviderID string
	Prompt     string
	OutputAs   string
	Timeout    time.Duration
}

type multiAgentStepResult struct {
	Index      int
	ProviderID string
	Status     string
	Output     string
	Error      string
	DurationMs int64
	Result     map[string]any
}

func isMultiAgentSessionRequest(params map[string]any) bool {
	if parseBool(params["multiAgent"]) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(shared.StringArg(params, "mode", "")), multiAgentTargetID) {
		return true
	}
	routing := shared.AsMap(params["routing"])
	if strings.TrimSpace(shared.StringArg(routing, "orchestrationMode", "")) != "" {
		return true
	}
	return false
}

func (o *SessionOrchestrator) ProcessMultiAgent(ctx context.Context, method string, params map[string]any, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	if method != "session.start" && method != "session.message" {
		return nil, &shared.RPCError{Code: -32601, Message: "MULTI_AGENT_METHOD_NOT_ALLOWED: " + method}
	}
	plan, rpcErr := o.parseMultiAgentPlan(params)
	if rpcErr != nil {
		return nil, rpcErr
	}

	sessionID := shared.StringArg(params, "sessionId", "")
	threadID := shared.StringArg(params, "threadId", sessionID)
	turnID := fmt.Sprintf("turn-%d", time.Now().UnixNano())
	sess := o.server.getOrCreateSession(sessionID, threadID)
	sess.mu.Lock()
	sess.target = multiAgentTargetID
	sess.provider = ""
	sess.mode = multiAgentTargetID
	sess.control.ControlPlaneSessionID = sessionID
	sess.control.ThreadID = threadID
	sess.control.RequestedWorkingDir = plan.SharedWorkspace
	sess.control.RemoteWorkingDirHint = strings.TrimSpace(shared.StringArg(params, "remoteWorkingDirectoryHint", ""))
	sess.control.UpdatedAt = time.Now()
	sess.task = QueuedTask{
		SessionID: sessionID,
		ThreadID:  threadID,
		TurnID:    turnID,
		Target:    multiAgentTargetID,
		State:     TaskStateRunning,
		Kind:      TaskKindMultiAgent,
		UpdatedAt: time.Now(),
	}
	if prompt := strings.TrimSpace(shared.StringArg(params, "taskPrompt", "")); prompt != "" {
		sess.history = append(sess.history, "USER: "+prompt)
	}
	sess.mu.Unlock()

	o.server.emitSessionUpdate(notify, turnID, map[string]any{
		"type":      "status",
		"event":     "multi_agent_started",
		"message":   "multi-agent orchestration started",
		"mode":      plan.Mode,
		"stepCount": len(plan.Steps),
		"pending":   true,
		"error":     false,
	})

	result := o.runMultiAgentPlan(ctx, method, params, plan, sessionID, threadID, turnID, notify)
	routing := RoutingResult{TargetID: multiAgentTargetID}
	normalized := o.normalizeResult(sess, result, routing, turnID, withMultiAgentWorkingDirectory(params, plan.SharedWorkspace))
	o.server.emitSessionUpdate(notify, turnID, map[string]any{
		"type":    "status",
		"event":   "multi_agent_completed",
		"message": strings.TrimSpace(shared.StringArg(normalized, "summary", "multi-agent orchestration completed")),
		"pending": false,
		"error":   !parseBool(normalized["success"]),
		"result":  normalized,
	})
	return normalized, nil
}

func (o *SessionOrchestrator) parseMultiAgentPlan(params map[string]any) (multiAgentPlan, *shared.RPCError) {
	routing := shared.AsMap(params["routing"])
	mode := strings.ToLower(strings.TrimSpace(shared.StringArg(routing, "orchestrationMode", "")))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(shared.StringArg(params, "orchestrationMode", "")))
	}
	if mode == "" {
		mode = "sequence"
	}
	switch mode {
	case "sequence", "parallel", "race", "conversation":
	default:
		return multiAgentPlan{}, &shared.RPCError{Code: -32602, Message: "MULTI_AGENT_INVALID_MODE: " + mode}
	}

	sharedWorkspace := strings.TrimSpace(shared.StringArg(routing, "sharedWorkspace", ""))
	if sharedWorkspace == "" {
		sharedWorkspace = strings.TrimSpace(shared.StringArg(params, "workingDirectory", ""))
	}
	if sharedWorkspace == "" {
		sharedWorkspace = filepath.Join(os.TempDir(), "xworkmate-bridge", "multi-agent", safeMultiAgentPathSegment(shared.StringArg(params, "sessionId", "session")))
	}
	if err := os.MkdirAll(sharedWorkspace, 0o755); err != nil {
		return multiAgentPlan{}, &shared.RPCError{Code: -32602, Message: "MULTI_AGENT_WORKSPACE_UNAVAILABLE: " + err.Error()}
	}

	steps := parseMultiAgentSteps(shared.ListArg(routing, "steps"), params, defaultMultiAgentTimeout)
	if len(steps) == 0 {
		steps = parseMultiAgentSteps(shared.ListArg(params, "steps"), params, defaultMultiAgentTimeout)
	}
	if mode == "conversation" && len(steps) == 0 {
		steps = parseConversationParticipants(shared.ListArg(routing, "participants"), params, defaultMultiAgentTimeout)
	}
	if len(steps) == 0 {
		return multiAgentPlan{}, &shared.RPCError{Code: -32602, Message: "MULTI_AGENT_STEPS_REQUIRED"}
	}
	for index := range steps {
		steps[index].Index = index
		if steps[index].ProviderID == "" {
			return multiAgentPlan{}, &shared.RPCError{Code: -32602, Message: "MULTI_AGENT_PROVIDER_REQUIRED"}
		}
		if steps[index].Prompt == "" {
			return multiAgentPlan{}, &shared.RPCError{Code: -32602, Message: "MULTI_AGENT_PROMPT_REQUIRED"}
		}
	}

	maxTurns := parsePositiveInt(routing["maxTurns"])
	if maxTurns == 0 {
		maxTurns = parsePositiveInt(params["maxTurns"])
	}
	if maxTurns == 0 {
		maxTurns = len(steps)
	}
	if mode == "conversation" && maxTurns < len(steps) {
		maxTurns = len(steps)
	}

	stopConditions := parseMultiAgentStringList(routing["stopConditions"])
	if len(stopConditions) == 0 {
		stopConditions = []string{"STATUS: DONE", "STATUS: CONSENSUS"}
	}

	return multiAgentPlan{
		Mode:            mode,
		Steps:           steps,
		MaxTurns:        maxTurns,
		StopConditions:  stopConditions,
		SharedWorkspace: sharedWorkspace,
	}, nil
}

func parseMultiAgentSteps(raw []any, params map[string]any, fallbackTimeout time.Duration) []multiAgentStep {
	steps := make([]multiAgentStep, 0, len(raw))
	defaultPrompt := strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	for _, item := range raw {
		stepParams := shared.AsMap(item)
		if len(stepParams) == 0 {
			continue
		}
		timeout := time.Duration(parsePositiveInt(stepParams["timeoutMs"])) * time.Millisecond
		if timeout <= 0 {
			timeout = time.Duration(parsePositiveInt(stepParams["timeoutSeconds"])) * time.Second
		}
		if timeout <= 0 {
			timeout = fallbackTimeout
		}
		prompt := strings.TrimSpace(firstNonEmptyString(stepParams, "prompt", "taskPrompt"))
		if prompt == "" {
			prompt = defaultPrompt
		}
		steps = append(steps, multiAgentStep{
			ProviderID: strings.TrimSpace(firstNonEmptyString(stepParams, "providerId", "provider", "agent")),
			Prompt:     prompt,
			OutputAs:   strings.TrimSpace(shared.StringArg(stepParams, "outputAs", "")),
			Timeout:    timeout,
		})
	}
	return steps
}

func parseConversationParticipants(raw []any, params map[string]any, fallbackTimeout time.Duration) []multiAgentStep {
	steps := make([]multiAgentStep, 0, len(raw))
	prompt := strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	for _, item := range raw {
		providerID := strings.TrimSpace(fmt.Sprint(item))
		if mapped := shared.AsMap(item); len(mapped) > 0 {
			providerID = strings.TrimSpace(firstNonEmptyString(mapped, "providerId", "provider", "agent"))
		}
		if providerID == "" || providerID == "<nil>" {
			continue
		}
		steps = append(steps, multiAgentStep{
			ProviderID: providerID,
			Prompt:     prompt,
			Timeout:    fallbackTimeout,
		})
	}
	return steps
}

func (o *SessionOrchestrator) runMultiAgentPlan(ctx context.Context, method string, params map[string]any, plan multiAgentPlan, sessionID string, threadID string, turnID string, notify func(map[string]any)) map[string]any {
	switch plan.Mode {
	case "parallel":
		return o.runMultiAgentParallel(ctx, method, params, plan, sessionID, threadID, turnID, notify)
	case "race":
		return o.runMultiAgentRace(ctx, method, params, plan, sessionID, threadID, turnID, notify)
	case "conversation":
		return o.runMultiAgentConversation(ctx, method, params, plan, sessionID, threadID, turnID, notify)
	default:
		return o.runMultiAgentSequence(ctx, method, params, plan, sessionID, threadID, turnID, notify)
	}
}

func (o *SessionOrchestrator) runMultiAgentSequence(ctx context.Context, method string, params map[string]any, plan multiAgentPlan, sessionID string, threadID string, turnID string, notify func(map[string]any)) map[string]any {
	values := map[string]string{"input": strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))}
	results := make([]map[string]any, 0, len(plan.Steps))
	var lastOutput string
	for _, step := range plan.Steps {
		step.Prompt = renderMultiAgentPrompt(step.Prompt, values)
		stepResult := o.runMultiAgentProviderStep(ctx, method, params, plan, step, sessionID, threadID, turnID, false, notify)
		results = append(results, stepResult.Map())
		if stepResult.Status != "completed" {
			return multiAgentResult(plan, results, "", stepResult.Error, false)
		}
		lastOutput = stepResult.Output
		values["previousOutput"] = stepResult.Output
		if step.OutputAs != "" {
			values[step.OutputAs] = stepResult.Output
		}
	}
	return multiAgentResult(plan, results, lastOutput, "", true)
}

func (o *SessionOrchestrator) runMultiAgentParallel(ctx context.Context, method string, params map[string]any, plan multiAgentPlan, sessionID string, threadID string, turnID string, notify func(map[string]any)) map[string]any {
	results := make([]multiAgentStepResult, len(plan.Steps))
	var wg sync.WaitGroup
	for _, step := range plan.Steps {
		wg.Add(1)
		go func(step multiAgentStep) {
			defer wg.Done()
			results[step.Index] = o.runMultiAgentProviderStep(ctx, method, params, plan, step, sessionID, threadID, turnID, false, notify)
		}(step)
	}
	wg.Wait()
	return multiAgentAggregateResult(plan, results)
}

func (o *SessionOrchestrator) runMultiAgentRace(ctx context.Context, method string, params map[string]any, plan multiAgentPlan, sessionID string, threadID string, turnID string, notify func(map[string]any)) map[string]any {
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	resultsCh := make(chan multiAgentStepResult, len(plan.Steps))
	for _, step := range plan.Steps {
		go func(step multiAgentStep) {
			resultsCh <- o.runMultiAgentProviderStep(raceCtx, method, params, plan, step, sessionID, threadID, turnID, false, notify)
		}(step)
	}
	results := make([]multiAgentStepResult, 0, len(plan.Steps))
	for range plan.Steps {
		stepResult := <-resultsCh
		results = append(results, stepResult)
		if stepResult.Status == "completed" {
			cancel()
			return multiAgentResult(plan, multiAgentStepResultMaps(results), stepResult.Output, "", true)
		}
	}
	return multiAgentResult(plan, multiAgentStepResultMaps(results), "", "all agents failed", false)
}

func (o *SessionOrchestrator) runMultiAgentConversation(ctx context.Context, method string, params map[string]any, plan multiAgentPlan, sessionID string, threadID string, turnID string, notify func(map[string]any)) map[string]any {
	results := make([]map[string]any, 0, plan.MaxTurns)
	started := make(map[string]bool)
	lastAgent := ""
	lastOutput := ""
	topic := strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	for turn := 0; turn < plan.MaxTurns; turn++ {
		step := plan.Steps[turn%len(plan.Steps)]
		if lastOutput == "" {
			step.Prompt = fmt.Sprintf("Topic:\n%s\n\nShared workspace: %s\n\nRespond as %s.", topic, plan.SharedWorkspace, step.ProviderID)
		} else {
			step.Prompt = fmt.Sprintf("[%s]: %s\n\nContinue the discussion as %s. Shared workspace: %s", lastAgent, lastOutput, step.ProviderID, plan.SharedWorkspace)
		}
		useSend := started[step.ProviderID]
		stepResult := o.runMultiAgentProviderStep(ctx, method, params, plan, step, sessionID, threadID, turnID, useSend, notify)
		started[step.ProviderID] = true
		entry := stepResult.Map()
		entry["conversationTurn"] = turn + 1
		results = append(results, entry)
		if stepResult.Status != "completed" {
			return multiAgentResult(plan, results, "", stepResult.Error, false)
		}
		lastAgent = step.ProviderID
		lastOutput = stepResult.Output
		if multiAgentConversationShouldStop(lastOutput, plan.StopConditions) {
			result := multiAgentResult(plan, results, lastOutput, "", true)
			result["stopReason"] = "stop_condition"
			return result
		}
	}
	result := multiAgentResult(plan, results, lastOutput, "", true)
	result["stopReason"] = "max_turns"
	return result
}

func (o *SessionOrchestrator) runMultiAgentProviderStep(ctx context.Context, method string, params map[string]any, plan multiAgentPlan, step multiAgentStep, sessionID string, threadID string, turnID string, useSend bool, notify func(map[string]any)) multiAgentStepResult {
	startedAt := time.Now()
	o.server.emitSessionUpdate(notify, turnID, map[string]any{
		"type":       "status",
		"event":      "multi_agent_step_started",
		"providerId": step.ProviderID,
		"stepIndex":  step.Index,
		"pending":    true,
		"error":      false,
	})
	compat, ok := o.server.providers[step.ProviderID]
	if !ok {
		return failedMultiAgentStep(step, startedAt, "provider unavailable")
	}

	stepCtx := ctx
	cancel := func() {}
	if step.Timeout > 0 {
		stepCtx, cancel = context.WithTimeout(ctx, step.Timeout)
	}
	defer cancel()

	stepSessionID := fmt.Sprintf("%s/%s/%d/%s", sessionID, turnID, step.Index, step.ProviderID)
	stepParams := multiAgentStepParams(params, step, plan.SharedWorkspace)
	sink := func(update map[string]any) {
		o.server.emitSessionUpdate(notify, turnID, map[string]any{
			"type":           "delta",
			"event":          "multi_agent_step_update",
			"providerId":     step.ProviderID,
			"stepIndex":      step.Index,
			"providerUpdate": update,
			"pending":        true,
			"error":          false,
		})
	}

	var result map[string]any
	var err error
	if useSend {
		result, err = compat.SendMessage(stepCtx, stepSessionID, threadID, stepParams, sink)
		if _, ok := asSessionContinuationUnavailableError(err); ok {
			result, err = compat.StartSession(stepCtx, stepSessionID, threadID, stepParams, sink)
		}
	} else {
		result, err = compat.StartSession(stepCtx, stepSessionID, threadID, stepParams, sink)
	}
	stepResult := multiAgentStepResult{
		Index:      step.Index,
		ProviderID: step.ProviderID,
		Status:     "completed",
		Output:     strings.TrimSpace(multiAgentOutputFromResult(result)),
		DurationMs: time.Since(startedAt).Milliseconds(),
		Result:     result,
	}
	if err != nil {
		stepResult.Status = "failed"
		stepResult.Error = err.Error()
	} else if result != nil {
		if value, ok := result["success"]; ok && !parseBool(value) {
			stepResult.Status = "failed"
			stepResult.Error = firstNonEmptyString(result, "error", "message", "unavailableMessage")
		}
	}
	if stepResult.Status == "completed" && stepResult.Output == "" {
		stepResult.Status = "failed"
		stepResult.Error = "provider returned no displayable output"
	}
	if stepResult.Error == "" && stepResult.Status == "failed" {
		stepResult.Error = "provider execution failed"
	}
	o.server.emitSessionUpdate(notify, turnID, map[string]any{
		"type":       "status",
		"event":      "multi_agent_step_completed",
		"providerId": step.ProviderID,
		"stepIndex":  step.Index,
		"status":     stepResult.Status,
		"message":    stepResult.Output,
		"pending":    false,
		"error":      stepResult.Status != "completed",
	})
	return stepResult
}

func multiAgentStepParams(params map[string]any, step multiAgentStep, sharedWorkspace string) map[string]any {
	next := make(map[string]any, len(params)+2)
	for key, value := range params {
		next[key] = value
	}
	delete(next, "multiAgent")
	delete(next, "mode")
	next["taskPrompt"] = step.Prompt
	next["workingDirectory"] = sharedWorkspace
	next["routing"] = map[string]any{
		"routingMode":             "explicit",
		"explicitExecutionTarget": "singleAgent",
		"explicitProviderId":      step.ProviderID,
	}
	return next
}

func multiAgentOutputFromResult(result map[string]any) string {
	if result == nil {
		return ""
	}
	return firstNonEmptyString(result, "output", "summary", "message", "text")
}

func failedMultiAgentStep(step multiAgentStep, startedAt time.Time, message string) multiAgentStepResult {
	return multiAgentStepResult{
		Index:      step.Index,
		ProviderID: step.ProviderID,
		Status:     "failed",
		Error:      message,
		DurationMs: time.Since(startedAt).Milliseconds(),
	}
}

func (r multiAgentStepResult) Map() map[string]any {
	result := map[string]any{
		"index":      r.Index,
		"providerId": r.ProviderID,
		"status":     r.Status,
		"durationMs": r.DurationMs,
	}
	if r.Output != "" {
		result["output"] = r.Output
	}
	if r.Error != "" {
		result["error"] = r.Error
	}
	if r.Result != nil {
		result["result"] = r.Result
	}
	return result
}

func multiAgentAggregateResult(plan multiAgentPlan, results []multiAgentStepResult) map[string]any {
	mapped := multiAgentStepResultMaps(results)
	outputs := make([]string, 0, len(results))
	var errorText string
	success := true
	for _, result := range results {
		if result.Status != "completed" {
			success = false
			if errorText == "" {
				errorText = result.ProviderID + ": " + result.Error
			}
			continue
		}
		if result.Output != "" {
			outputs = append(outputs, result.ProviderID+": "+result.Output)
		}
	}
	return multiAgentResult(plan, mapped, strings.Join(outputs, "\n"), errorText, success)
}

func multiAgentStepResultMaps(results []multiAgentStepResult) []map[string]any {
	mapped := make([]map[string]any, 0, len(results))
	for _, result := range results {
		mapped = append(mapped, result.Map())
	}
	return mapped
}

func multiAgentResult(plan multiAgentPlan, steps []map[string]any, output string, errorText string, success bool) map[string]any {
	status := "completed"
	if !success {
		status = "failed"
	}
	if output == "" && errorText != "" {
		output = errorText
	}
	result := map[string]any{
		"success":                   success,
		"status":                    status,
		"mode":                      multiAgentTargetID,
		"orchestrationMode":         plan.Mode,
		"resolvedExecutionTarget":   multiAgentTargetID,
		"resolvedProviderId":        "",
		"resolvedGatewayProviderId": "",
		"steps":                     steps,
		"sharedWorkspace":           plan.SharedWorkspace,
	}
	if output != "" {
		result["output"] = output
		result["summary"] = output
		result["message"] = output
	}
	if errorText != "" {
		result["error"] = errorText
	}
	return result
}

func renderMultiAgentPrompt(prompt string, values map[string]string) string {
	rendered := prompt
	for key, value := range values {
		rendered = strings.ReplaceAll(rendered, "{{"+key+"}}", value)
	}
	return rendered
}

func multiAgentConversationShouldStop(output string, stopConditions []string) bool {
	normalizedOutput := strings.ToUpper(output)
	for _, condition := range stopConditions {
		condition = strings.TrimSpace(condition)
		if condition == "" {
			continue
		}
		if strings.Contains(normalizedOutput, strings.ToUpper(condition)) {
			return true
		}
	}
	return false
}

func parseMultiAgentStringList(raw any) []string {
	switch values := raw.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			text := strings.TrimSpace(fmt.Sprint(value))
			if text != "" && text != "<nil>" {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func withMultiAgentWorkingDirectory(params map[string]any, workingDirectory string) map[string]any {
	next := make(map[string]any, len(params)+1)
	for key, value := range params {
		next[key] = value
	}
	if workingDirectory != "" {
		next["workingDirectory"] = workingDirectory
	}
	return next
}

func safeMultiAgentPathSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.', r == '_', r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	if builder.Len() == 0 {
		return "default"
	}
	return builder.String()
}
