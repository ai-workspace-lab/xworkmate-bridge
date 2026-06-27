package acp

import (
	"slices"
	"strings"
	"testing"
	"time"

	"xworkmate-bridge/internal/shared"
)

func TestNormalizeResultGatewaySuccessEvidenceAdjudication(t *testing.T) {
	cases := []struct {
		name              string
		routingTarget     string
		result            map[string]any
		wantSuccess       bool
		wantStatus        string
		wantCode          string
		wantSuccessSource string
	}{
		{
			name:          "gateway absent success without output or artifacts fails",
			routingTarget: "gateway",
			result:        map[string]any{},
			wantSuccess:   false,
			wantStatus:    string(TaskStateFailed),
			wantCode:      "OPENCLAW_TERMINAL_WITHOUT_EVIDENCE",
		},
		{
			name:              "gateway absent success with output is inferred",
			routingTarget:     "gateway",
			result:            map[string]any{"output": "done"},
			wantSuccess:       true,
			wantStatus:        string(TaskStateCompleted),
			wantSuccessSource: "inferred",
		},
		{
			name:          "gateway absent success with artifacts is inferred",
			routingTarget: "gateway",
			result: map[string]any{
				"artifacts": []any{map[string]any{"relativePath": "reports/final.md"}},
			},
			wantSuccess:       true,
			wantStatus:        string(TaskStateCompleted),
			wantSuccessSource: "inferred",
		},
		{
			name:          "gateway explicit false remains failed",
			routingTarget: "gateway",
			result:        map[string]any{"success": false, "output": "failed"},
			wantSuccess:   false,
			wantStatus:    string(TaskStateFailed),
		},
		{
			name:          "gateway explicit true remains completed",
			routingTarget: "gateway",
			result:        map[string]any{"success": true},
			wantSuccess:   true,
			wantStatus:    string(TaskStateCompleted),
		},
		{
			name:          "non gateway absent success keeps legacy inference",
			routingTarget: "single-agent",
			result:        map[string]any{"output": "done"},
			wantSuccess:   true,
			wantStatus:    string(TaskStateCompleted),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := NewServer()
			orchestrator := NewSessionOrchestrator(server)
			sess := server.getOrCreateSession("session-"+strings.ReplaceAll(tc.name, " ", "-"), "thread")

			got := orchestrator.normalizeResult(
				sess,
				tc.result,
				RoutingResult{TargetID: tc.routingTarget, ProviderID: "provider", GatewayProviderID: "openclaw"},
				"turn-1",
				map[string]any{},
			)

			if parseBool(got["success"]) != tc.wantSuccess {
				t.Fatalf("success = %#v, want %v in %#v", got["success"], tc.wantSuccess, got)
			}
			if status := shared.StringArg(got, "status", ""); status != tc.wantStatus {
				t.Fatalf("status = %q, want %q in %#v", status, tc.wantStatus, got)
			}
			if code := shared.StringArg(got, "code", ""); code != tc.wantCode {
				t.Fatalf("code = %q, want %q in %#v", code, tc.wantCode, got)
			}
			if source := shared.StringArg(got, "successSource", ""); source != tc.wantSuccessSource {
				t.Fatalf("successSource = %q, want %q in %#v", source, tc.wantSuccessSource, got)
			}
		})
	}
}

func TestNormalizeOpenClawTaskGetUnknownArtifactEvidenceKeepsActiveRecordRunning(t *testing.T) {
	payload := map[string]any{
		"success":           false,
		"status":            "unknown",
		"taskStatus":        "unknown",
		"evidence":          "artifacts_present",
		"artifactCount":     1,
		"artifactScope":     "tasks/session/run",
		"artifactDirectory": "/remote/openclaw/workspace/tasks/session/run",
		"artifacts":         []any{map[string]any{"relativePath": "series.config.json"}},
	}
	record := &OpenClawTaskRecord{
		RunID:                  "run",
		SessionKey:             "session",
		GatewayProviderID:      "openclaw",
		RequiresArtifactExport: true,
		DeadlineAt:             time.Now().Add(time.Minute),
	}

	got := normalizeOpenClawTaskGetResult(
		map[string]any{"requiredArtifactExtensions": []any{"pdf"}},
		payload,
		"openclaw",
		record,
	)

	if status := shared.StringArg(got, "status", ""); status != string(TaskStateRunning) {
		t.Fatalf("expected active unknown artifact evidence to remain running, got %#v", got)
	}
	if evidence := shared.StringArg(got, "artifactEvidence", ""); evidence != "artifacts_present" {
		t.Fatalf("expected artifact evidence audit field, got %#v", got)
	}
}

func TestExpectedArtifactDirectoriesDoNotBlockTerminalTaskState(t *testing.T) {
	params := map[string]any{"expectedArtifactDirs": []any{"reports/", "artifacts/"}}
	payload := map[string]any{
		"success":           true,
		"status":            string(TaskStateCompleted),
		"artifactScope":     "tasks/session/run",
		"artifactDirectory": "/remote/openclaw/workspace/tasks/session/run",
		"expectedArtifactDirs": []any{
			"reports/",
			"artifacts/",
		},
	}

	if openClawTaskGetRequiresArtifactExport(params, payload) {
		t.Fatal("expectedArtifactDirs must remain non-blocking scan hints")
	}
	got := normalizeOpenClawTaskGetResult(params, payload, "openclaw", nil)
	if status := shared.StringArg(got, "status", ""); status != string(TaskStateCompleted) {
		t.Fatalf("expected terminal status to remain completed, got %#v", got)
	}
	if parseBool(got["pending"]) {
		t.Fatalf("expected terminal payload not to become pending, got %#v", got)
	}
}

func TestRequiredArtifactExtensionsStillBlockUntilVerified(t *testing.T) {
	params := map[string]any{"requiredArtifactExtensions": []any{"md"}}
	payload := map[string]any{
		"success":           true,
		"status":            string(TaskStateCompleted),
		"artifactScope":     "tasks/session/run",
		"artifactDirectory": "/remote/openclaw/workspace/tasks/session/run",
	}

	if !openClawTaskGetRequiresArtifactExport(params, payload) {
		t.Fatal("requiredArtifactExtensions must remain a blocking delivery contract")
	}
	got := normalizeOpenClawTaskGetResult(params, payload, "openclaw", nil)
	if status := shared.StringArg(got, "status", ""); status != string(TaskStateRunning) {
		t.Fatalf("expected missing required artifact to remain syncing, got %#v", got)
	}
}

func TestNormalizeOpenClawTaskGetUnknownArtifactEvidenceFailsAfterDeadlineWithoutRequiredArtifacts(t *testing.T) {
	payload := map[string]any{
		"success":            false,
		"status":             "unknown",
		"taskStatus":         "unknown",
		"evidence":           "artifacts_present",
		"artifactCount":      1,
		"runId":              "run",
		"openclawSessionKey": "session",
		"artifactScope":      "tasks/session/run",
		"artifactDirectory":  "/remote/openclaw/workspace/tasks/session/run",
		"artifacts":          []any{map[string]any{"relativePath": "series.config.json"}},
	}
	record := &OpenClawTaskRecord{DeadlineAt: time.Now().Add(-time.Minute)}

	got := normalizeOpenClawTaskGetResult(
		map[string]any{"requiredArtifactExtensions": []any{"pdf"}},
		payload,
		"openclaw",
		record,
	)

	if status := shared.StringArg(got, "status", ""); status != string(TaskStateFailed) {
		t.Fatalf("expected expired unknown artifact evidence to fail, got %#v", got)
	}
	if code := shared.StringArg(got, "code", ""); code != "OPENCLAW_TERMINAL_WITHOUT_EVIDENCE" {
		t.Fatalf("expected evidence failure code, got %#v", got)
	}
	if missing := shared.ListArg(got, "missingRequiredExtensions"); !slices.ContainsFunc(missing, func(value any) bool {
		return strings.TrimSpace(shared.StringArg(map[string]any{"value": value}, "value", "")) == "pdf"
	}) {
		t.Fatalf("expected missing pdf extension, got %#v", got)
	}
}

func TestOpenClawArtifactConstraintFieldsArePropagatedAndMarkPartialDelivery(t *testing.T) {
	result := map[string]any{}
	mergeOpenClawArtifactPayload(result, map[string]any{
		"constraintSatisfied":       false,
		"missingRequiredExtensions": []any{"pdf"},
	})
	if got := result["constraintSatisfied"]; got != false {
		t.Fatalf("expected constraintSatisfied=false to propagate, got %#v", result)
	}
	if missing := shared.ListArg(result, "missingRequiredExtensions"); len(missing) != 1 || missing[0] != "pdf" {
		t.Fatalf("expected missingRequiredExtensions to propagate, got %#v", result)
	}

	server := NewServer()
	orchestrator := NewSessionOrchestrator(server)
	sess := server.getOrCreateSession("session-partial-delivery", "thread-partial-delivery")
	got := orchestrator.normalizeResult(
		sess,
		map[string]any{
			"success":                   true,
			"output":                    "created some files",
			"constraintSatisfied":       false,
			"missingRequiredExtensions": []any{"pdf"},
		},
		RoutingResult{TargetID: "gateway", ProviderID: "gateway", GatewayProviderID: "openclaw"},
		"turn-partial-delivery",
		map[string]any{},
	)

	if status := shared.StringArg(got, "status", ""); status != "partially_delivered" {
		t.Fatalf("expected partially_delivered status, got %#v", got)
	}
	if !parseBool(got["success"]) {
		t.Fatalf("partial delivery should preserve success=true, got %#v", got)
	}
}

func TestOpenClawArtifactsSatisfyEveryRequiredExtension(t *testing.T) {
	artifacts := []map[string]any{
		{"relativePath": "exports/final.pdf"},
	}
	if openClawArtifactsSatisfyRequiredExtensions(artifacts, []string{"pdf", "mp4"}) {
		t.Fatalf("expected only pdf artifact to miss mp4 requirement")
	}
	if !openClawArtifactsSatisfyRequiredExtensions(
		append(artifacts, map[string]any{"relativePath": "exports/final.MP4"}),
		[]string{"pdf", "mp4"},
	) {
		t.Fatalf("expected pdf and mp4 artifacts to satisfy both requirements")
	}
}

func TestTaskGetArtifactExportReceivesRequiredArtifactExtensions(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")

	server := NewServer()
	start, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "session.start",
		Params: map[string]any{
			"sessionId":        "session-export-required-exts",
			"threadId":         "thread-export-required-exts",
			"taskPrompt":       "say pong",
			"workingDirectory": t.TempDir(),
			"metadata": map[string]any{
				"xworkmateTaskArtifactContract": map[string]any{
					"requiresExportBeforeFinalResponse": true,
				},
			},
			"routing": map[string]any{
				"routingMode":                "explicit",
				"explicitExecutionTarget":    "gateway",
				"preferredGatewayProviderId": "openclaw",
			},
		},
	}, nil)
	if rpcErr != nil {
		t.Fatalf("expected running task handle, got rpc error: %#v", rpcErr)
	}
	response, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "xworkmate.tasks.get",
		Params: map[string]any{
			"sessionId":                  shared.StringArg(start, "sessionId", ""),
			"threadId":                   shared.StringArg(start, "threadId", ""),
			"turnId":                     shared.StringArg(start, "turnId", ""),
			"runId":                      shared.StringArg(start, "runId", ""),
			"appThreadKey":               shared.StringArg(start, "appThreadKey", ""),
			"openclawSessionKey":         shared.StringArg(start, "openclawSessionKey", ""),
			"artifactScope":              shared.StringArg(start, "artifactScope", ""),
			"artifactDirectory":          shared.StringArg(start, "artifactDirectory", ""),
			"gatewayProviderId":          shared.StringArg(start, "resolvedGatewayProviderId", ""),
			"requiresArtifactExport":     true,
			"requiredArtifactExtensions": []any{"pdf"},
			"expectedFileCountByExtension": map[string]any{
				"pdf": 1,
			},
		},
	}, nil)
	if rpcErr != nil {
		t.Fatalf("expected task lookup response, got rpc error: %#v", rpcErr)
	}
	if status := shared.StringArg(response, "status", ""); status != string(TaskStateRunning) {
		t.Fatalf("expected missing required artifact to keep syncing, got %#v", response)
	}
	exportParams := gateway.LastArtifactExportParams()
	if got := shared.ListArg(exportParams, "requiredArtifactExtensions"); len(got) != 1 || got[0] != "pdf" {
		t.Fatalf("expected requiredArtifactExtensions to reach export, got %#v", exportParams)
	}
	if got := shared.AsMap(exportParams["expectedFileCountByExtension"]); openClawPositiveInt(got["pdf"]) != 1 {
		t.Fatalf("expected expectedFileCountByExtension to reach export, got %#v", exportParams)
	}
}

func TestIsOpenClawUnknownMethodErrorAcceptsNumericGatewayCodes(t *testing.T) {
	const method = "xworkmate.session.prepare"
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{
			name:    "string invalid_request code",
			payload: map[string]any{"code": "INVALID_REQUEST", "message": "unknown method: xworkmate.session.prepare"},
			want:    true,
		},
		{
			name:    "numeric -32002 (real gateway shape that previously hard-failed)",
			payload: map[string]any{"code": float64(-32002), "message": "unknown method: xworkmate.session.prepare"},
			want:    true,
		},
		{
			name:    "numeric -32601 method not found",
			payload: map[string]any{"code": float64(-32601), "message": "Unknown method: xworkmate.session.prepare"},
			want:    true,
		},
		{
			name:    "empty code",
			payload: map[string]any{"message": "unknown method: xworkmate.session.prepare"},
			want:    true,
		},
		{
			name:    "unrelated error must not be swallowed",
			payload: map[string]any{"code": float64(-32002), "message": "gateway socket closed"},
			want:    false,
		},
		{
			name:    "unknown method for a different method name",
			payload: map[string]any{"code": float64(-32601), "message": "unknown method: chat.send"},
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOpenClawUnknownMethodError(tc.payload, method); got != tc.want {
				t.Fatalf("isOpenClawUnknownMethodError(%v) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}
