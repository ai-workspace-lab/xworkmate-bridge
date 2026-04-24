package hermesadapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/shared"
)

func isolateHermesConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HERMES_HOME", "")
}

type stubClient struct {
	initResult          initializeResult
	initErr             error
	callResult          map[string]any
	callErr             error
	callFn              func(method string, params map[string]any) (map[string]any, error)
	lastMethod          string
	lastParams          map[string]any
	methods             []string
	notificationHandler func(map[string]any)
}

func (s *stubClient) Initialize() (initializeResult, error) {
	return s.initResult, s.initErr
}

func (s *stubClient) Call(method string, params map[string]any) (map[string]any, error) {
	s.lastMethod = method
	s.lastParams = params
	s.methods = append(s.methods, method)
	if s.callFn != nil {
		return s.callFn(method, params)
	}
	return s.callResult, s.callErr
}

func (s *stubClient) SetNotificationHandler(handler func(map[string]any)) {
	s.notificationHandler = handler
}

func (s *stubClient) Close() error { return nil }

func TestHandleCapabilitiesSynthesizesProviderResponse(t *testing.T) {
	server := NewServer(&stubClient{
		initResult: initializeResult{ProtocolVersion: 1},
	})
	result := server.handleRequest(shared.RPCRequest{Method: "acp.capabilities"})
	if got := result["singleAgent"]; got != true {
		t.Fatalf("expected singleAgent true, got %#v", result)
	}
	providers, _ := result["providers"].([]string)
	if len(providers) != 1 || providers[0] != "hermes" {
		t.Fatalf("expected hermes provider, got %#v", result)
	}
}

func TestHandleRPCSessionStartReturnsUpstreamResult(t *testing.T) {
	isolateHermesConfig(t)

	stub := &stubClient{initResult: initializeResult{ProtocolVersion: 1}}
	stub.callFn = func(method string, params map[string]any) (map[string]any, error) {
		switch method {
		case "session/new":
			return map[string]any{
				"result": map[string]any{
					"sessionId": "upstream-session-1",
				},
			}, nil
		case "session/prompt":
			if stub.notificationHandler != nil {
				stub.notificationHandler(map[string]any{
					"method": "session/update",
					"params": map[string]any{
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"text":          "hello",
						},
					},
				})
			}
			return map[string]any{
				"result": map[string]any{
					"stopReason": "end_turn",
				},
			}, nil
		default:
			return map[string]any{"result": map[string]any{}}, nil
		}
	}
	server := NewServer(stub)
	server.upstreamMethod = "session/prompt"

	body, _ := json.Marshal(shared.RPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "session.start",
		Params: map[string]any{
			"sessionId":  "s1",
			"taskPrompt": "hello",
		},
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/acp/rpc", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	var envelope map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	result := envelope["result"].(map[string]any)
	if got := result["output"]; got != "hello" {
		t.Fatalf("expected output hello, got %#v", result)
	}
	if len(stub.methods) != 2 || stub.methods[0] != "session/new" || stub.methods[1] != "session/prompt" {
		t.Fatalf("expected session/new then session/prompt, got %#v", stub.methods)
	}
}

func TestHandleRPCSessionStartRejectsEmptyUpstreamResponse(t *testing.T) {
	isolateHermesConfig(t)

	stub := &stubClient{initResult: initializeResult{ProtocolVersion: 1}}
	stub.callFn = func(method string, params map[string]any) (map[string]any, error) {
		switch method {
		case "session/new":
			return map[string]any{
				"result": map[string]any{
					"sessionId": "upstream-session-1",
				},
			}, nil
		case "session/prompt":
			return map[string]any{
				"result": map[string]any{},
			}, nil
		default:
			return map[string]any{"result": map[string]any{}}, nil
		}
	}
	server := NewServer(stub)
	server.upstreamMethod = "session/prompt"

	body, _ := json.Marshal(shared.RPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "session.start",
		Params: map[string]any{
			"sessionId":  "s1",
			"taskPrompt": "hello",
		},
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/acp/rpc", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	var envelope map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	result := envelope["result"].(map[string]any)
	if got := result["success"]; got != false {
		t.Fatalf("expected success false, got %#v", result)
	}
	if got := result["error"]; got != "hermes upstream returned empty response" {
		t.Fatalf("expected empty-response error, got %#v", result)
	}
}

func TestNewServerDefaultsHermesToSessionPrompt(t *testing.T) {
	server := NewServer(&stubClient{})
	if got := server.upstreamMethod; got != "session/prompt" {
		t.Fatalf("expected default upstream method session/prompt, got %q", got)
	}
}

func TestHandleRPCSessionMessageReusesUpstreamSession(t *testing.T) {
	isolateHermesConfig(t)

	stub := &stubClient{initResult: initializeResult{ProtocolVersion: 1}}
	promptCalls := 0
	stub.callFn = func(method string, params map[string]any) (map[string]any, error) {
		switch method {
		case "session/new":
			return map[string]any{
				"result": map[string]any{
					"sessionId": "upstream-session-1",
				},
			}, nil
		case "session/prompt":
			promptCalls++
			if got := params["sessionId"]; got != "upstream-session-1" {
				t.Fatalf("expected upstream session id to persist, got %#v", got)
			}
			if stub.notificationHandler != nil {
				text := "first"
				if promptCalls > 1 {
					text = "second"
				}
				stub.notificationHandler(map[string]any{
					"method": "session/update",
					"params": map[string]any{
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"text":          text,
						},
					},
				})
			}
			return map[string]any{
				"result": map[string]any{
					"stopReason": "end_turn",
				},
			}, nil
		default:
			return map[string]any{"result": map[string]any{}}, nil
		}
	}

	server := NewServer(stub)
	server.upstreamMethod = "session/prompt"

	startBody, _ := json.Marshal(shared.RPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "session.start",
		Params: map[string]any{
			"sessionId":        "s1",
			"taskPrompt":       "first",
			"workingDirectory": "/tmp/demo",
		},
	})
	startReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/acp/rpc", bytes.NewReader(startBody))
	startReq.Header.Set("Authorization", "Bearer test-token")
	startRec := httptest.NewRecorder()
	server.HandleRPC(startRec, startReq)

	if startRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for session.start, got %d", startRec.Code)
	}
	var startEnvelope map[string]any
	if err := json.NewDecoder(startRec.Body).Decode(&startEnvelope); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	startResult := startEnvelope["result"].(map[string]any)
	if got := startResult["output"]; got != "first" {
		t.Fatalf("expected first output, got %#v", startResult)
	}

	messageBody, _ := json.Marshal(shared.RPCRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "session.message",
		Params: map[string]any{
			"sessionId":        "s1",
			"taskPrompt":       "second",
			"workingDirectory": "/tmp/demo",
		},
	})
	messageReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/acp/rpc", bytes.NewReader(messageBody))
	messageReq.Header.Set("Authorization", "Bearer test-token")
	messageRec := httptest.NewRecorder()
	server.HandleRPC(messageRec, messageReq)

	if messageRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for session.message, got %d", messageRec.Code)
	}
	var messageEnvelope map[string]any
	if err := json.NewDecoder(messageRec.Body).Decode(&messageEnvelope); err != nil {
		t.Fatalf("decode message response: %v", err)
	}
	messageResult := messageEnvelope["result"].(map[string]any)
	if got := messageResult["output"]; got != "second" {
		t.Fatalf("expected second output, got %#v", messageResult)
	}
	if len(stub.methods) != 3 || stub.methods[0] != "session/new" || stub.methods[1] != "session/prompt" || stub.methods[2] != "session/prompt" {
		t.Fatalf("expected session/new then two session/prompt calls, got %#v", stub.methods)
	}
}

func TestHandleRPCSessionStartUsesConfiguredHermesModelBeforePrompt(t *testing.T) {
	home := t.TempDir()
	hermesHome := filepath.Join(home, ".hermes")
	if err := os.MkdirAll(hermesHome, 0o755); err != nil {
		t.Fatalf("mkdir hermes home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hermesHome, "config.yaml"), []byte("model:\n  default: hermes-default-model\n"), 0o644); err != nil {
		t.Fatalf("write hermes config: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("HERMES_HOME", "")

	stub := &stubClient{initResult: initializeResult{ProtocolVersion: 1}}
	stub.callFn = func(method string, params map[string]any) (map[string]any, error) {
		switch method {
		case "session/new":
			return map[string]any{
				"result": map[string]any{
					"sessionId": "upstream-session-1",
				},
			}, nil
		case "session/set_model":
			if got := params["sessionId"]; got != "upstream-session-1" {
				t.Fatalf("expected sessionId upstream-session-1, got %#v", got)
			}
			if got := params["modelId"]; got != "hermes-default-model" {
				t.Fatalf("expected configured hermes model, got %#v", got)
			}
			return map[string]any{"result": map[string]any{}}, nil
		case "session/prompt":
			if stub.notificationHandler != nil {
				stub.notificationHandler(map[string]any{
					"method": "session/update",
					"params": map[string]any{
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"text":          "ok",
						},
					},
				})
			}
			return map[string]any{
				"result": map[string]any{
					"stopReason": "end_turn",
				},
			}, nil
		default:
			return map[string]any{"result": map[string]any{}}, nil
		}
	}

	server := NewServer(stub)
	server.upstreamMethod = "session/prompt"

	body, _ := json.Marshal(shared.RPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "session.start",
		Params: map[string]any{
			"sessionId":        "s-model",
			"taskPrompt":       "hello",
			"workingDirectory": "/tmp/demo",
		},
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/acp/rpc", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if len(stub.methods) != 3 || stub.methods[0] != "session/new" || stub.methods[1] != "session/set_model" || stub.methods[2] != "session/prompt" {
		t.Fatalf("expected session/new then session/set_model then session/prompt, got %#v", stub.methods)
	}
}

func TestHandleWebSocketCapabilities(t *testing.T) {
	server := NewServer(&stubClient{initResult: initializeResult{ProtocolVersion: 1}})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.HandleWebSocket(w, r)
	}))
	defer httpServer.Close()

	wsURL := "ws" + httpServer.URL[len("http"):]
	header := http.Header{}
	header.Set("Authorization", "Bearer test-token")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(shared.RPCRequest{
		JSONRPC: "2.0",
		ID:      "cap-1",
		Method:  "acp.capabilities",
		Params:  map[string]any{},
	}); err != nil {
		t.Fatalf("write json: %v", err)
	}
	var envelope map[string]any
	if err := conn.ReadJSON(&envelope); err != nil {
		t.Fatalf("read json: %v", err)
	}
	result := envelope["result"].(map[string]any)
	providers := result["providers"].([]any)
	if len(providers) != 1 || providers[0] != "hermes" {
		t.Fatalf("expected hermes provider over websocket, got %#v", result)
	}
}
