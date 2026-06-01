package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"xworkmate-bridge/internal/shared"
)

func TestHTTPHandlerRootAndPingExposeRuntimeVersionInfo(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	SetRuntimeVersionInfo(RuntimeVersionInfo{
		Commit:    "0123456",
		Version:   "v1.1.4-protocol4",
		BuildDate: "2026-06-01T00:00:00Z",
	})
	t.Cleanup(func() {
		SetRuntimeVersionInfo(RuntimeVersionInfo{})
	})

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	handler := server.Handler()

	rootRecorder := httptest.NewRecorder()
	rootRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	handler.ServeHTTP(rootRecorder, rootRequest)

	if rootRecorder.Code != http.StatusOK {
		t.Fatalf("expected root 200, got %d", rootRecorder.Code)
	}
	if !strings.Contains(rootRecorder.Body.String(), "xworkmate-bridge is running") {
		t.Fatalf("expected root body to contain service banner, got %q", rootRecorder.Body.String())
	}

	pingRecorder := httptest.NewRecorder()
	pingRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/ping", nil)
	handler.ServeHTTP(pingRecorder, pingRequest)

	if pingRecorder.Code != http.StatusOK {
		t.Fatalf("expected ping 200, got %d", pingRecorder.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(pingRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode ping payload: %v", err)
	}

	if got := payload["status"]; got != "ok" {
		t.Fatalf("expected status ok, got %#v", got)
	}
	if _, ok := payload["image"]; ok {
		t.Fatalf("expected ping payload to omit stale image ref, got %#v", payload["image"])
	}
	if _, ok := payload["tag"]; ok {
		t.Fatalf("expected ping payload to omit stale image tag, got %#v", payload["tag"])
	}
	if got := payload["commit"]; got != "0123456" {
		t.Fatalf("expected binary commit, got %#v", got)
	}
	if got := payload["version"]; got != "v1.1.4-protocol4" {
		t.Fatalf("expected binary version, got %#v", got)
	}
	if got := payload["buildDate"]; got != "2026-06-01T00:00:00Z" {
		t.Fatalf("expected binary build date, got %#v", got)
	}
}

func TestHTTPHandlerRejectsLegacyACPCodexPath(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/acp-server/codex", nil)
	request.Header.Set("Authorization", "Bearer bridge-test-token")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "PROVIDER_DIRECT_PATH_DISABLED") {
		t.Fatalf("expected disabled provider path error, got %q", recorder.Body.String())
	}
}

func TestHTTPHandlerProviderDirectPathRequiresAuthorization(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/acp-server/hermes", nil)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}

func TestHTTPHandlerRPCSSEWritesFinalEnvelopeAndDone(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"cap-1","method":"acp.capabilities","params":{}}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer bridge-test-token")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected JSON-RPC 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("expected event-stream content type, got %q", contentType)
	}
	events := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n\n")
	if len(events) != 2 {
		t.Fatalf("expected final envelope and done events, got %q", recorder.Body.String())
	}
	if !strings.HasPrefix(events[0], "data: ") {
		t.Fatalf("expected first event data line, got %q", events[0])
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(events[0], "data: ")), &envelope); err != nil {
		t.Fatalf("decode final envelope: %v", err)
	}
	if envelope["id"] != "cap-1" {
		t.Fatalf("expected final envelope id cap-1, got %#v", envelope["id"])
	}
	if _, ok := envelope["result"].(map[string]any); !ok {
		t.Fatalf("expected result envelope, got %#v", envelope)
	}
	if events[1] != "data: [DONE]" {
		t.Fatalf("expected done event, got %q", events[1])
	}
}

func TestHTTPHandlerGatewayOpenClawSSEKeepaliveBeforeFinalEnvelopeAndDone(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()
	gateway.agentWaitDelayMs.Store(50)

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	previousInterval := httpSSEKeepaliveInterval
	httpSSEKeepaliveInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		httpSSEKeepaliveInterval = previousInterval
	})

	server := NewServer()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	request, err := http.NewRequest(
		http.MethodPost,
		httpServer.URL+"/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"task-keepalive","method":"session.start","params":{"sessionId":"s1","threadId":"t1","taskPrompt":"Reply pong","workingDirectory":"`+t.TempDir()+`","routing":{"routingMode":"explicit","explicitExecutionTarget":"gateway","preferredGatewayProviderId":"openclaw"}}}`),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer bridge-test-token")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.StatusCode, string(body))
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("expected event-stream content type, got %q", contentType)
	}

	events := strings.Split(strings.TrimSpace(string(body)), "\n\n")
	if len(events) < 4 {
		t.Fatalf("expected accepted, keepalive, final envelope, and done events, got %q", string(body))
	}
	if events[len(events)-1] != "data: [DONE]" {
		t.Fatalf("expected done event, got %q", events[len(events)-1])
	}
	var sawAcceptedBeforeKeepalive bool
	var sawKeepaliveBeforeFinal bool
	var sawFinal bool
	for _, event := range events[:len(events)-1] {
		if !strings.HasPrefix(event, "data: ") {
			t.Fatalf("expected data event, got %q", event)
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(event, "data: ")), &envelope); err != nil {
			t.Fatalf("decode event %q: %v", event, err)
		}
		if envelope["method"] == "xworkmate.bridge.accepted" && !sawKeepaliveBeforeFinal && !sawFinal {
			sawAcceptedBeforeKeepalive = true
		}
		if envelope["method"] == "xworkmate.bridge.keepalive" && !sawFinal {
			sawKeepaliveBeforeFinal = true
		}
		if envelope["id"] == "task-keepalive" {
			sawFinal = true
			if _, ok := envelope["result"].(map[string]any); !ok {
				t.Fatalf("expected result envelope, got %#v", envelope)
			}
		}
	}
	if !sawAcceptedBeforeKeepalive {
		t.Fatalf("expected accepted event before keepalive/final envelope, got %q", string(body))
	}
	if !sawKeepaliveBeforeFinal {
		t.Fatalf("expected keepalive event before final envelope, got %q", string(body))
	}
	if !sawFinal {
		t.Fatalf("expected final task envelope, got %q", string(body))
	}
}

func TestHTTPHandlerGatewayOpenClawAdmissionQueuesExcessConcurrentSSE(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()
	gateway.agentWaitDelayMs.Store(1500)

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_MAX_ACTIVE", "1")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_MAX_QUEUED", "2")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_QUEUE_TIMEOUT", "5s")
	server := NewServer()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	type result struct {
		body string
		err  error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			request, err := http.NewRequest(
				http.MethodPost,
				httpServer.URL+"/acp/rpc",
				strings.NewReader(`{"jsonrpc":"2.0","id":"task-`+strconv.Itoa(index)+`","method":"session.start","params":{"sessionId":"s`+strconv.Itoa(index)+`","threadId":"t`+strconv.Itoa(index)+`","taskPrompt":"Reply pong","workingDirectory":"`+t.TempDir()+`","routing":{"routingMode":"explicit","explicitExecutionTarget":"gateway","preferredGatewayProviderId":"openclaw"}}}`),
			)
			if err != nil {
				results <- result{err: err}
				return
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "text/event-stream")
			request.Header.Set("Authorization", "Bearer bridge-test-token")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				results <- result{err: err}
				return
			}
			if response.StatusCode != http.StatusOK {
				results <- result{err: fmt.Errorf("expected 200, got %d: %s", response.StatusCode, string(body))}
				return
			}
			results <- result{body: string(body)}
		}(index)
	}
	close(start)
	waitForOpenClawGatewayCount(t, func() int { return gateway.ChatSendCount() }, 1)
	time.Sleep(75 * time.Millisecond)
	if got := gateway.ChatSendCount(); got != 1 {
		t.Fatalf("expected admission gate to hold queued chat.send while one is active, got %d", got)
	}
	wg.Wait()
	close(results)

	var sawQueued bool
	var finalCount int
	for item := range results {
		if item.err != nil {
			t.Fatalf("concurrent request failed: %v", item.err)
		}
		if strings.Contains(item.body, `"event":"queued"`) {
			sawQueued = true
		}
		if strings.Contains(item.body, `"result"`) && strings.Contains(item.body, `data: [DONE]`) {
			finalCount += 1
		}
	}
	if !sawQueued {
		t.Fatalf("expected one queued session.update event")
	}
	if finalCount != 2 {
		t.Fatalf("expected both requests to return final result, got %d", finalCount)
	}
	if got := gateway.ChatSendCount(); got != 2 {
		t.Fatalf("expected queued request to run after a slot releases, got %d chat.send calls", got)
	}
}

func TestHTTPHandlerGatewayOpenClawHandlesFiveConcurrentE2ECases(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()
	gateway.agentWaitDelayMs.Store(200)

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_MAX_ACTIVE", "5")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_MAX_QUEUED", "20")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_QUEUE_TIMEOUT", "5s")
	server := NewServer()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	prompts := []string{
		"从单机权限 → 网络边界 → Web安全 → 云身份 → Zero Trust → AI Agent 身份 → AI模型与知识保护 演进 \n制作 使用codex 制作连续制作 7张的一些列图片",
		"参考附件模版制作 ,围绕\n从单机权限 → 网络边界 → Web安全 → 云身份 → Zero Trust → AI Agent 身份 → AI模型与知识保护 演进 \n连续制作 7张的一些列图片",
		"拆章节 -> 每章调用 Codex -> 每章 GPT images2 生成图 -> 汇总排版 -> 输出 PDF\n\n右侧 artifact栏 显示的陈旧文件 make artifact",
		"围绕\n从单机权限 → 网络边界 → Web安全 → 云身份 → Zero Trust → AI Agent 身份 → AI模型与知识保护 演进 右侧是当下 \n测试制作视频",
		"围绕\n\n从单机权限 → 网络边界 → Web安全 → 云身份 → Zero Trust → AI Agent 身份 → AI模型与知识保护 演进 \n\n拆章节 -> 每章调用 Codex -> 每章 GPT images2 生成图 -> 汇总排版 -> 制作视频",
	}
	type result struct {
		body string
		err  error
	}
	results := make(chan result, len(prompts))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index, prompt := range prompts {
		wg.Add(1)
		go func(index int, prompt string) {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(
				`{"jsonrpc":"2.0","id":"e2e-%d","method":"session.start","params":{"sessionId":"e2e-s%d","threadId":"e2e-t%d","taskPrompt":%q,"workingDirectory":%q,"routing":{"routingMode":"explicit","explicitExecutionTarget":"gateway","preferredGatewayProviderId":"openclaw"}}}`,
				index,
				index,
				index,
				prompt,
				t.TempDir(),
			)
			request, err := http.NewRequest(
				http.MethodPost,
				httpServer.URL+"/acp/rpc",
				strings.NewReader(body),
			)
			if err != nil {
				results <- result{err: err}
				return
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "text/event-stream")
			request.Header.Set("Authorization", "Bearer bridge-test-token")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer func() { _ = response.Body.Close() }()
			responseBody, err := io.ReadAll(response.Body)
			if err != nil {
				results <- result{err: err}
				return
			}
			if response.StatusCode != http.StatusOK {
				results <- result{err: fmt.Errorf("expected 200, got %d: %s", response.StatusCode, string(responseBody))}
				return
			}
			results <- result{body: string(responseBody)}
		}(index, prompt)
	}
	close(start)
	waitForOpenClawGatewayCount(t, gateway.ChatSendCount, len(prompts))
	wg.Wait()
	close(results)

	var finalCount int
	for item := range results {
		if item.err != nil {
			t.Fatalf("concurrent e2e request failed: %v", item.err)
		}
		if strings.Contains(item.body, `"event":"queued"`) {
			t.Fatalf("expected five active OpenClaw slots without queueing, got queued event: %s", item.body)
		}
		for _, unexpected := range []string{
			"invalid handshake",
			"SOCKET_CLOSED",
			"ACP_HTTP_CONNECTION_CLOSED",
			"GATEWAY_CONNECT_FAILED",
			"openclaw returned partial artifacts without required final deliverables",
		} {
			if strings.Contains(item.body, unexpected) {
				t.Fatalf("unexpected gateway stability error %q in body: %s", unexpected, item.body)
			}
		}
		if strings.Contains(item.body, `"result"`) && strings.Contains(item.body, `data: [DONE]`) {
			finalCount += 1
		}
	}
	if finalCount != len(prompts) {
		t.Fatalf("expected all five e2e requests to return final result, got %d", finalCount)
	}
	if got := gateway.ConnectCount(); got != 1 {
		t.Fatalf("expected bridge to reuse one established OpenClaw connection, got %d connects", got)
	}
	expectedGatewayTurns := len(prompts) + 1
	if got := gateway.ChatSendCount(); got != expectedGatewayTurns {
		t.Fatalf("expected five primary chat.send calls plus one final-deliverable repair, got %d", got)
	}
	if got := gateway.AgentWaitCount(); got != expectedGatewayTurns {
		t.Fatalf("expected five primary agent.wait calls plus one final-deliverable repair, got %d", got)
	}
}

func TestHTTPHandlerGatewayOpenClawAdmissionRejectsWhenQueueFull(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()
	gateway.agentWaitDelayMs.Store(300)

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_MAX_ACTIVE", "1")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_MAX_QUEUED", "0")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_QUEUE_TIMEOUT", "5s")
	server := NewServer()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	firstRequest, err := http.NewRequest(
		http.MethodPost,
		httpServer.URL+"/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"task-active","method":"session.start","params":{"sessionId":"active","threadId":"active","taskPrompt":"Reply pong","workingDirectory":"`+t.TempDir()+`","routing":{"routingMode":"explicit","explicitExecutionTarget":"gateway","preferredGatewayProviderId":"openclaw"}}}`),
	)
	if err != nil {
		t.Fatalf("build first request: %v", err)
	}
	firstRequest.Header.Set("Content-Type", "application/json")
	firstRequest.Header.Set("Accept", "text/event-stream")
	firstRequest.Header.Set("Authorization", "Bearer bridge-test-token")
	firstDone := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(firstRequest)
		if err != nil {
			firstDone <- err
			return
		}
		defer func() { _ = response.Body.Close() }()
		_, err = io.ReadAll(response.Body)
		firstDone <- err
	}()
	waitForOpenClawGatewayCount(t, func() int { return gateway.ChatSendCount() }, 1)

	secondRequest, err := http.NewRequest(
		http.MethodPost,
		httpServer.URL+"/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"task-rejected","method":"session.start","params":{"sessionId":"rejected","threadId":"rejected","taskPrompt":"Reply pong","workingDirectory":"`+t.TempDir()+`","routing":{"routingMode":"explicit","explicitExecutionTarget":"gateway","preferredGatewayProviderId":"openclaw"}}}`),
	)
	if err != nil {
		t.Fatalf("build second request: %v", err)
	}
	secondRequest.Header.Set("Content-Type", "application/json")
	secondRequest.Header.Set("Accept", "text/event-stream")
	secondRequest.Header.Set("Authorization", "Bearer bridge-test-token")
	response, err := http.DefaultClient.Do(secondRequest)
	if err != nil {
		t.Fatalf("send second request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read second response: %v", err)
	}
	bodyText := string(body)
	if !strings.Contains(bodyText, openClawGatewayBusyErrorCode) {
		t.Fatalf("expected busy error, got %s", bodyText)
	}
	if strings.Contains(bodyText, `"result"`) {
		t.Fatalf("busy response must not return a result envelope: %s", bodyText)
	}
	if got := gateway.ChatSendCount(); got != 1 {
		t.Fatalf("rejected request must not reach chat.send, got %d", got)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first request failed: %v", err)
	}
}

func TestHTTPHandlerGatewayOpenClawFiltersRawGatewayEventsAndKeepsFinalResult(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()
	gateway.largeGatewayPayloadBytes.Store(openClawGatewayMaxNotificationBytes * 2)
	gateway.emitAgentDelta.Store(true)

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	server := NewServer()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	request, err := http.NewRequest(
		http.MethodPost,
		httpServer.URL+"/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"task-filter","method":"session.start","params":{"sessionId":"session-filter","threadId":"thread-filter","taskPrompt":"make artifact","workingDirectory":"`+t.TempDir()+`","routing":{"routingMode":"explicit","explicitExecutionTarget":"gateway","preferredGatewayProviderId":"openclaw"}}}`),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer bridge-test-token")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	bodyText := string(body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.StatusCode, bodyText)
	}
	if len(body) >= openClawGatewayMaxNotificationBytes {
		t.Fatalf("expected compact gateway SSE body, got %d bytes", len(body))
	}
	for _, rawMethod := range []string{
		"xworkmate.gateway.push",
		"xworkmate.gateway.snapshot",
		"xworkmate.gateway.log",
		"largeIgnored",
	} {
		if strings.Contains(bodyText, rawMethod) {
			t.Fatalf("expected raw gateway event %q to be filtered from SSE body: %s", rawMethod, bodyText)
		}
	}

	events := strings.Split(strings.TrimSpace(bodyText), "\n\n")
	if len(events) < 4 {
		t.Fatalf("expected accepted, session.update, final envelope, and done events, got %q", bodyText)
	}
	if events[len(events)-1] != "data: [DONE]" {
		t.Fatalf("expected done event, got %q", events[len(events)-1])
	}
	var sawAccepted bool
	var sawDelta bool
	var sawCompletedSnapshot bool
	var sawFinal bool
	for _, event := range events[:len(events)-1] {
		if !strings.HasPrefix(event, "data: ") {
			t.Fatalf("expected data event, got %q", event)
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(event, "data: ")), &envelope); err != nil {
			t.Fatalf("decode event %q: %v", event, err)
		}
		switch envelope["method"] {
		case "xworkmate.bridge.accepted":
			sawAccepted = true
		case "session.update":
			params := shared.AsMap(envelope["params"])
			if params["type"] == "delta" && params["delta"] == "streamed delta" {
				sawDelta = true
				if got := params["sessionId"]; got != "session-filter" {
					t.Fatalf("expected session-filter session update, got %#v", params)
				}
				if got := params["threadId"]; got != "thread-filter" {
					t.Fatalf("expected thread-filter session update, got %#v", params)
				}
			}
			result := shared.AsMap(params["result"])
			if params["type"] == "status" && params["event"] == "completed" && len(result) > 0 {
				sawCompletedSnapshot = true
				if got := result["resolvedGatewayProviderId"]; got != "openclaw" {
					t.Fatalf("expected completed snapshot to carry openclaw final result, got %#v", result)
				}
				if !strings.Contains(bodyText, openClawArtifactDownloadPath) {
					t.Fatalf("expected completed snapshot to include normalized artifact download URL, got %s", bodyText)
				}
			}
		}
		if envelope["id"] == "task-filter" {
			sawFinal = true
			result := shared.AsMap(envelope["result"])
			if got := result["resolvedGatewayProviderId"]; got != "openclaw" {
				t.Fatalf("expected openclaw final result, got %#v", result)
			}
			if !strings.Contains(bodyText, openClawArtifactDownloadPath) {
				t.Fatalf("expected normalized artifact download URL in final result, got %s", bodyText)
			}
		}
	}
	if !sawAccepted {
		t.Fatalf("expected accepted event, got %q", bodyText)
	}
	if !sawDelta {
		t.Fatalf("expected compact session.update delta, got %q", bodyText)
	}
	if !sawCompletedSnapshot {
		t.Fatalf("expected completed session.update result snapshot, got %q", bodyText)
	}
	if !sawFinal {
		t.Fatalf("expected final result envelope, got %q", bodyText)
	}
	if got := gateway.Methods(); !sameMethods(got, []string{"connect", "xworkmate.artifacts.prepare", "chat.send", "agent.wait", "xworkmate.artifacts.export"}) {
		t.Fatalf("expected artifact workflow methods to prepare before chat.send, got %#v", got)
	}
}

func TestHTTPHandlerGatewayOpenClawForcesGatewayRouting(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	server := NewServer()
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"task-1","method":"session.start","params":{"sessionId":"s1","threadId":"t1","taskPrompt":"Reply pong","workingDirectory":"`+t.TempDir()+`","routing":{"routingMode":"explicit","explicitExecutionTarget":"gateway","preferredGatewayProviderId":"openclaw"}}}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer bridge-test-token")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"resolvedGatewayProviderId":"openclaw"`) {
		t.Fatalf("expected forced OpenClaw gateway result, got %q", recorder.Body.String())
	}
	if gateway.ChatSendCount() != 1 {
		t.Fatalf("expected one OpenClaw chat.send, got %d", gateway.ChatSendCount())
	}
	if gateway.AgentWaitCount() != 1 {
		t.Fatalf("expected one OpenClaw agent.wait, got %d", gateway.AgentWaitCount())
	}
}

func TestHTTPHandlerSessionGetReturnsCompletedOpenClawResult(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	server := NewServer()
	handler := server.Handler()

	startRecorder := httptest.NewRecorder()
	startRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"task-1","method":"session.start","params":{"sessionId":"s1","threadId":"t1","taskPrompt":"Reply pong","workingDirectory":"`+t.TempDir()+`","routing":{"routingMode":"explicit","explicitExecutionTarget":"gateway","preferredGatewayProviderId":"openclaw"}}}`),
	)
	startRequest.Header.Set("Content-Type", "application/json")
	startRequest.Header.Set("Authorization", "Bearer bridge-test-token")
	handler.ServeHTTP(startRecorder, startRequest)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("expected start 200, got %d: %s", startRecorder.Code, startRecorder.Body.String())
	}

	getRecorder := httptest.NewRecorder()
	getRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"get-1","method":"xworkmate.sessions.get","params":{"sessionId":"s1","threadId":"t1"}}`),
	)
	getRequest.Header.Set("Content-Type", "application/json")
	getRequest.Header.Set("Authorization", "Bearer bridge-test-token")
	handler.ServeHTTP(getRecorder, getRequest)

	if getRecorder.Code != http.StatusOK {
		t.Fatalf("expected get 200, got %d: %s", getRecorder.Code, getRecorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(getRecorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode session get response: %v", err)
	}
	result := shared.AsMap(decoded["result"])
	if got := result["status"]; got != "completed" {
		t.Fatalf("expected completed status, got %#v from %#v", got, result)
	}
	task := shared.AsMap(result["task"])
	if got := task["turnId"]; got == "" {
		t.Fatalf("expected retained task turn id, got %#v", task)
	}
	snapshot := shared.AsMap(result["result"])
	if got := snapshot["resolvedGatewayProviderId"]; got != "openclaw" {
		t.Fatalf("expected OpenClaw snapshot, got %#v", snapshot)
	}
	if got := snapshot["output"]; got == "" {
		t.Fatalf("expected output in session snapshot, got %#v", snapshot)
	}
}

func TestSafeSSEStreamDropsLateNotificationsAfterClose(t *testing.T) {
	writer := &panicSSEWriter{header: http.Header{}}
	stream := newSafeSSEStream(context.Background(), writer, safeSSEStreamMeta{})

	stream.close()
	if stream.write(map[string]any{"method": "xworkmate.gateway.push"}) {
		t.Fatal("expected closed stream to drop late notification")
	}
	if writer.writes != 0 {
		t.Fatalf("expected no write after close, got %d", writer.writes)
	}

	openStream := newSafeSSEStream(context.Background(), writer, safeSSEStreamMeta{})
	writer.panicOnWrite = true
	if openStream.write(map[string]any{"method": "xworkmate.gateway.push"}) {
		t.Fatal("expected panic writer to be marked closed")
	}
	if openStream.write(map[string]any{"method": "xworkmate.gateway.push"}) {
		t.Fatal("expected writes after panic to stay closed")
	}
}

type panicSSEWriter struct {
	header       http.Header
	writes       int
	panicOnWrite bool
}

func (w *panicSSEWriter) Header() http.Header {
	return w.header
}

func (w *panicSSEWriter) Write(payload []byte) (int, error) {
	w.writes++
	if w.panicOnWrite {
		panic("closed response writer")
	}
	return len(payload), nil
}

func (w *panicSSEWriter) WriteHeader(int) {}

func TestHTTPHandlerPingRequiresBearerAuthorizationWhenBridgeAuthTokenConfigured(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/ping", nil)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}

func TestHTTPHandlerPingAllowsReviewBearerAuthorizationWhenConfigured(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_REVIEW_AUTH_TOKEN", "review-bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/ping", nil)
	request.Header.Set("Authorization", "Bearer review-bridge-test-token")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 for configured review token, got %d", recorder.Code)
	}
}

func TestHandleWebSocketRejectsUnknownOrigin(t *testing.T) {
	t.Setenv("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus")
	t.Setenv("BRIDGE_AUTH_TOKEN", "")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/acp", nil)
	request.Header.Set("Origin", "https://evil.example.com")

	server.HandleWebSocket(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("expected application/json content type, got %q", got)
	}
}

func TestHandleRPCAllowsPreflightForConfiguredOrigin(t *testing.T) {
	t.Setenv("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus,http://localhost:*")
	t.Setenv("BRIDGE_AUTH_TOKEN", "")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1/acp/rpc", nil)
	request.Header.Set("Origin", "https://xworkmate.svc.plus")
	request.Header.Set("Access-Control-Request-Method", "POST")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://xworkmate.svc.plus" {
		t.Fatalf("expected allow origin header, got %q", got)
	}
}

func TestHandleRPCAllowsUnauthenticatedRequestsWhenBridgeAuthTokenUnset(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"acp.capabilities"}`),
	)
	request.Header.Set("Content-Type", "application/json")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 when BRIDGE_AUTH_TOKEN is unset and no header provided, got %d", recorder.Code)
	}
}

func TestHandleRPCRequiresBearerAuthorizationWhenBridgeAuthTokenConfigured(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	// session.start is a protected method that requires authentication
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"session.start","params":{"sessionId":"test"}}`),
	)
	request.Header.Set("Content-Type", "application/json")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}

func TestHandleRPCCapabilitiesRequiresBearerAuthorizationWhenBridgeAuthTokenConfigured(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"acp.capabilities"}`),
	)
	request.Header.Set("Content-Type", "application/json")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}

func TestHandleRPCAllowsReviewBearerAuthorizationWhenConfigured(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_REVIEW_AUTH_TOKEN", "review-bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"acp.capabilities"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer review-bridge-test-token")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 for configured review token, got %d", recorder.Code)
	}
}

func TestHandleRPCRejectsUnknownOrigin(t *testing.T) {
	t.Setenv("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus")
	t.Setenv("BRIDGE_AUTH_TOKEN", "")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"acp.capabilities"}`),
	)
	request.Header.Set("Origin", "https://evil.example.com")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", recorder.Code)
	}
	var envelope map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if _, ok := envelope["error"]; !ok {
		t.Fatalf("expected JSON-RPC error envelope, got %v", envelope)
	}
}

func TestHandleRPCMethodErrorUsesJSONEnvelope(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/acp/rpc", nil)
	request.Header.Set("Authorization", "Bearer test")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("expected application/json content type, got %q", got)
	}
}

func TestHandleRPCCapabilitiesStillReturnsJSONResult(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"acp.capabilities"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("expected application/json content type, got %q", got)
	}
	if !strings.Contains(recorder.Body.String(), `"providerCatalog"`) {
		t.Fatalf("expected capabilities response, got %q", recorder.Body.String())
	}
}

func TestAuthorizedAllowsUnauthenticatedRequestsWhenBridgeAuthTokenUnset(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/acp", nil)
	if !server.authorized(request) {
		t.Fatal("expected unauthenticated request to be allowed if BRIDGE_AUTH_TOKEN is unset")
	}
}

func TestHandleRPCCapabilitiesReturnsCanonicalProviderContract(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"cap-1","method":"acp.capabilities"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}

	var envelope map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode capabilities response: %v", err)
	}

	result := shared.AsMap(envelope["result"])
	if got := result["singleAgent"]; got != true {
		t.Fatalf("expected singleAgent true, got %v", got)
	}
	availableTargets := mustStringList(t, result["availableExecutionTargets"])
	if !reflect.DeepEqual(availableTargets, []string{"agent", "gateway"}) {
		t.Fatalf("expected canonical execution targets, got %#v", availableTargets)
	}

	providerCatalog := mustObjectList(t, result["providerCatalog"])
	if len(providerCatalog) != 4 {
		t.Fatalf("expected 4 providers, got %#v", providerCatalog)
	}
	wantAgentIDs := []string{"codex", "opencode", "gemini", "hermes"}
	wantAgentLabels := []string{"Codex", "OpenCode", "Gemini", "Hermes"}
	for index, wantID := range wantAgentIDs {
		if got := providerCatalog[index]["providerId"]; got != wantID {
			t.Fatalf("expected provider %q at index %d, got %#v", wantID, index, providerCatalog)
		}
		if got := providerCatalog[index]["label"]; got != wantAgentLabels[index] {
			t.Fatalf("expected label %q at index %d, got %#v", wantAgentLabels[index], index, providerCatalog)
		}
		if targets := mustStringList(t, providerCatalog[index]["targets"]); !reflect.DeepEqual(targets, []string{"agent"}) {
			t.Fatalf("expected agent targets for %q, got %#v", wantID, targets)
		}
	}

	gatewayProviders := mustObjectList(t, result["gatewayProviders"])
	if len(gatewayProviders) != 1 {
		t.Fatalf("expected exactly one gateway provider, got %#v", gatewayProviders)
	}
	if got := gatewayProviders[0]["providerId"]; got != "openclaw" {
		t.Fatalf("expected gateway providerId openclaw, got %#v", gatewayProviders[0])
	}
	if got := gatewayProviders[0]["label"]; got != "OpenClaw" {
		t.Fatalf("expected gateway label OpenClaw, got %#v", gatewayProviders[0])
	}
	if targets := mustStringList(t, gatewayProviders[0]["targets"]); !reflect.DeepEqual(targets, []string{"gateway"}) {
		t.Fatalf("expected gateway targets, got %#v", targets)
	}
}

func TestHandleRPCSessionStartSucceedsWithExplicitProvider(t *testing.T) {
	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer bridge-test-token" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      "task-1",
			"result": map[string]any{
				"success":  true,
				"provider": "opencode",
				"output":   "pong",
			},
		})
	}))
	defer externalServer.Close()

	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")

	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	server := NewServer()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID:          "opencode",
		Label:               "OpenCode",
		Endpoint:            externalServer.URL,
		AuthorizationHeader: "Bearer bridge-test-token",
		Enabled:             true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"task-1","method":"session.start","params":{"sessionId":"s1","threadId":"t1","taskPrompt":"Reply with exactly pong","workingDirectory":"`+t.TempDir()+`","routing":{"routingMode":"explicit","explicitExecutionTarget":"singleAgent","explicitProviderId":"opencode"}}}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer bridge-test-token")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"output":"pong"`) {
		t.Fatalf("expected pong output, got %q", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"provider":"opencode"`) {
		t.Fatalf("expected opencode provider, got %q", recorder.Body.String())
	}
}

func waitForOpenClawGatewayCount(t *testing.T, current func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if current() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for gateway count %d, got %d", want, current())
}

func mustObjectList(t *testing.T, value any) []map[string]any {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("expected object list, got %#v", value)
	}
	items := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		items = append(items, shared.AsMap(item))
	}
	return items
}

func mustStringList(t *testing.T, value any) []string {
	t.Helper()
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			items = append(items, strings.TrimSpace(item.(string)))
		}
		return items
	default:
		t.Fatalf("expected string list, got %#v", value)
		return nil
	}
}

func TestHandleWebSocketRequiresBearerAuthorization(t *testing.T) {
	t.Setenv("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus")
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/acp", nil)
	request.Header.Set("Origin", "https://xworkmate.svc.plus")

	server.HandleWebSocket(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}
