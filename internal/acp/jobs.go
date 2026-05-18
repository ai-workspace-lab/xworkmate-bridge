package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"xworkmate-bridge/internal/shared"
)

const (
	defaultJobTimeout       = 10 * time.Minute
	defaultWebhookMaxTries  = 3
	defaultWebhookRetryWait = 2 * time.Second
)

type jobManager struct {
	server *Server
	mu     sync.Mutex
	jobs   map[string]*bridgeJob
}

type bridgeJob struct {
	JobID        string
	SessionID    string
	ThreadID     string
	ProviderID   string
	Prompt       string
	WorkingDir   string
	Status       string
	Result       map[string]any
	Error        string
	CreatedAt    time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
	Timeout      time.Duration
	CallbackURL  string
	WebhookTries int
	WebhookSent  bool
	Target       string
	Channel      string
	AccountID    string
	Params       map[string]any
}

func newJobManager(server *Server) *jobManager {
	return &jobManager{server: server, jobs: make(map[string]*bridgeJob)}
}

func (s *Server) handleJobMethod(ctx context.Context, method string, params map[string]any, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	if s.jobs == nil {
		s.jobs = newJobManager(s)
	}
	switch method {
	case "xworkmate.jobs.submit":
		return s.jobs.submit(ctx, params, notify), nil
	case "xworkmate.jobs.get":
		return s.jobs.get(params), nil
	case "xworkmate.jobs.list":
		return s.jobs.list(), nil
	case "xworkmate.jobs.stats":
		return s.jobs.stats(), nil
	default:
		return nil, &shared.RPCError{Code: -32601, Message: "unknown jobs method: " + method}
	}
}

func (m *jobManager) submit(ctx context.Context, params map[string]any, notify func(map[string]any)) map[string]any {
	m.failStuck()
	jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
	providerID := strings.TrimSpace(firstNonEmptyString(params, "providerId", "agent", "provider"))
	routing := shared.AsMap(params["routing"])
	if providerID == "" {
		providerID = strings.TrimSpace(shared.StringArg(routing, "explicitProviderId", ""))
	}
	timeout := time.Duration(parsePositiveInt(params["timeoutMs"])) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultJobTimeout
	}
	job := &bridgeJob{
		JobID:       jobID,
		SessionID:   strings.TrimSpace(shared.StringArg(params, "sessionId", jobID)),
		ThreadID:    strings.TrimSpace(shared.StringArg(params, "threadId", jobID)),
		ProviderID:  providerID,
		Prompt:      strings.TrimSpace(shared.StringArg(params, "taskPrompt", shared.StringArg(params, "prompt", ""))),
		WorkingDir:  strings.TrimSpace(shared.StringArg(params, "workingDirectory", shared.StringArg(params, "cwd", ""))),
		Status:      "pending",
		CreatedAt:   time.Now(),
		Timeout:     timeout,
		CallbackURL: strings.TrimSpace(shared.StringArg(params, "callbackUrl", shared.StringArg(params, "webhookUrl", ""))),
		Target:      strings.TrimSpace(shared.StringArg(params, "target", shared.StringArg(params, "discordTarget", ""))),
		Channel:     strings.TrimSpace(shared.StringArg(params, "channel", "discord")),
		AccountID:   strings.TrimSpace(shared.StringArg(params, "accountId", "")),
		Params:      cloneJobParams(params),
	}
	m.mu.Lock()
	m.jobs[job.JobID] = job
	m.mu.Unlock()
	go m.run(ctx, job, notify)
	return map[string]any{"jobId": job.JobID, "status": job.Status, "sessionId": job.SessionID, "providerId": job.ProviderID}
}

func (m *jobManager) run(parent context.Context, job *bridgeJob, notify func(map[string]any)) {
	m.setRunning(job)
	ctx, cancel := context.WithTimeout(parent, job.Timeout)
	defer cancel()
	params := cloneJobParams(job.Params)
	params["sessionId"] = job.SessionID
	params["threadId"] = job.ThreadID
	params["taskPrompt"] = job.Prompt
	params["workingDirectory"] = job.WorkingDir
	if job.ProviderID != "" && !isMultiAgentSessionRequest(params) {
		params["routing"] = map[string]any{
			"routingMode":             "explicit",
			"explicitExecutionTarget": "singleAgent",
			"explicitProviderId":      job.ProviderID,
		}
	}
	result, rpcErr := m.server.orchestrator.Process(ctx, "session.start", params, notify)
	m.mu.Lock()
	defer m.mu.Unlock()
	job.CompletedAt = time.Now()
	if rpcErr != nil {
		job.Status = "failed"
		job.Error = rpcErr.Message
	} else {
		job.Result = result
		if parseBool(result["success"]) {
			job.Status = "completed"
		} else {
			job.Status = "failed"
			job.Error = firstNonEmptyString(result, "error", "message", "unavailableMessage")
		}
	}
	go m.sendCallbacks(job)
}

func (m *jobManager) setRunning(job *bridgeJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job.Status = "running"
	job.StartedAt = time.Now()
}

func (m *jobManager) get(params map[string]any) map[string]any {
	m.failStuck()
	jobID := strings.TrimSpace(shared.StringArg(params, "jobId", ""))
	m.mu.Lock()
	defer m.mu.Unlock()
	if job := m.jobs[jobID]; job != nil {
		return job.mapPayload()
	}
	return map[string]any{"status": "not_found", "jobId": jobID}
}

func (m *jobManager) list() map[string]any {
	m.failStuck()
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs := make([]map[string]any, 0, len(m.jobs))
	counts := map[string]int{"pending": 0, "running": 0, "completed": 0, "failed": 0}
	for _, job := range m.jobs {
		jobs = append(jobs, job.mapPayload())
		counts[job.Status]++
	}
	summary := make(map[string]any, len(counts))
	for key, value := range counts {
		summary[key] = value
	}
	return map[string]any{"jobs": jobs, "summary": summary}
}

func (m *jobManager) stats() map[string]any {
	summary := m.list()["summary"]
	return map[string]any{"summary": summary}
}

func (m *jobManager) failStuck() {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.jobs {
		if job.Status != "running" && job.Status != "pending" {
			continue
		}
		timeout := job.Timeout
		if timeout <= 0 {
			timeout = defaultJobTimeout
		}
		if now.Sub(job.CreatedAt) > timeout {
			job.Status = "failed"
			job.Error = "job exceeded timeout"
			job.CompletedAt = now
			go m.sendCallbacks(job)
		}
	}
}

func (m *jobManager) sendCallbacks(job *bridgeJob) {
	if job == nil {
		return
	}
	if job.CallbackURL != "" {
		m.sendWebhook(job)
	}
	if job.Target != "" {
		_, _ = m.server.invokeOpenClawTool(context.Background(), map[string]any{
			"tool":      "message",
			"action":    "send",
			"channel":   job.Channel,
			"accountId": job.AccountID,
			"args": map[string]any{
				"channel": job.Channel,
				"target":  job.Target,
				"message": job.markdownCard(),
			},
		})
	}
}

func (m *jobManager) sendWebhook(job *bridgeJob) {
	payload, _ := json.Marshal(job.mapPayload())
	for attempt := 1; attempt <= defaultWebhookMaxTries; attempt++ {
		req, err := http.NewRequest(http.MethodPost, job.CallbackURL, bytes.NewReader(payload))
		if err != nil {
			break
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		job.WebhookTries = attempt
		if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			job.WebhookSent = true
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			return
		}
		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
		}
		time.Sleep(defaultWebhookRetryWait)
	}
}

func (j *bridgeJob) mapPayload() map[string]any {
	payload := map[string]any{
		"jobId":        j.JobID,
		"status":       j.Status,
		"sessionId":    j.SessionID,
		"threadId":     j.ThreadID,
		"providerId":   j.ProviderID,
		"createdAt":    j.CreatedAt.UTC().Format(time.RFC3339Nano),
		"webhookSent":  j.WebhookSent,
		"webhookTries": j.WebhookTries,
	}
	if !j.StartedAt.IsZero() {
		payload["startedAt"] = j.StartedAt.UTC().Format(time.RFC3339Nano)
		payload["elapsedMs"] = time.Since(j.StartedAt).Milliseconds()
	}
	if !j.CompletedAt.IsZero() {
		payload["completedAt"] = j.CompletedAt.UTC().Format(time.RFC3339Nano)
		payload["durationMs"] = j.CompletedAt.Sub(j.CreatedAt).Milliseconds()
	}
	if j.Result != nil {
		payload["result"] = j.Result
	}
	if j.Error != "" {
		payload["error"] = j.Error
	}
	return payload
}

func (j *bridgeJob) markdownCard() string {
	title := fmt.Sprintf("### XWorkmate job %s: %s", j.JobID, j.Status)
	if j.Result != nil {
		if summary := firstNonEmptyString(j.Result, "summary", "output", "message"); summary != "" {
			return title + "\n\n" + summary
		}
	}
	if j.Error != "" {
		return title + "\n\n" + j.Error
	}
	return title
}

func (s *Server) invokeOpenClawTool(ctx context.Context, params map[string]any) (map[string]any, *shared.RPCError) {
	payload := map[string]any{
		"tool":   strings.TrimSpace(shared.StringArg(params, "tool", "")),
		"action": strings.TrimSpace(shared.StringArg(params, "action", "")),
		"args":   shared.AsMap(params["args"]),
	}
	if payload["tool"] == "" {
		return nil, &shared.RPCError{Code: -32602, Message: "TOOL_REQUIRED"}
	}
	toolURL := strings.TrimSpace(os.Getenv("OPENCLAW_TOOLS_INVOKE_URL"))
	if toolURL != "" {
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, toolURL, bytes.NewReader(body))
		if err != nil {
			return nil, &shared.RPCError{Code: -32002, Message: err.Error()}
		}
		req.Header.Set("Content-Type", "application/json")
		if token := strings.TrimSpace(os.Getenv("OPENCLAW_TOOLS_TOKEN")); token != "" {
			req.Header.Set("Authorization", bearerHeader(token))
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, &shared.RPCError{Code: -32002, Message: err.Error()}
		}
		defer func() { _ = response.Body.Close() }()
		var decoded map[string]any
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			return nil, &shared.RPCError{Code: -32002, Message: err.Error()}
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return decoded, &shared.RPCError{Code: -32002, Message: "tools invoke failed"}
		}
		return decoded, nil
	}
	if s.gateway == nil {
		return nil, &shared.RPCError{Code: -32001, Message: "GATEWAY_NOT_INITIALIZED"}
	}
	if rpcErr := ensureProductionGatewayConnected(s, "openclaw", nil); rpcErr != nil {
		return nil, rpcErr
	}
	result := s.gateway.RequestByMode("openclaw", "tools.invoke", payload, 30*time.Second, nil)
	if !result.OK {
		return nil, gatewayRPCError(result.Error, "tools invoke failed")
	}
	return shared.AsMap(result.Payload), nil
}

func bearerHeader(token string) string {
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return token
	}
	return "Bearer " + token
}

func cloneJobParams(params map[string]any) map[string]any {
	next := make(map[string]any, len(params))
	for key, value := range params {
		next[key] = value
	}
	return next
}
