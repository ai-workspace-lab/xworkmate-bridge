package hermesadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/shared"
)

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
	var stub *stubClient
	stub = &stubClient{initResult: initializeResult{ProtocolVersion: 1}}
	stub.callFn = func(method string, params map[string]any) (map[string]any, error) {
		switch method {
		case "new_session":
			return map[string]any{
				"result": map[string]any{
					"sessionId": "upstream-session-1",
				},
			}, nil
		case "prompt":
			if stub.notificationHandler != nil {
				stub.notificationHandler(map[string]any{
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
	server.upstreamMethod = "prompt"

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
	if len(stub.methods) != 2 || stub.methods[0] != "new_session" || stub.methods[1] != "prompt" {
		t.Fatalf("expected new_session then prompt, got %#v", stub.methods)
	}
}

func TestHandleSessionStartFallsBackToPromptRunner(t *testing.T) {
	stub := &stubClient{initResult: initializeResult{ProtocolVersion: 1}}
	server := NewServer(stub)
	server.upstreamMethod = ""
	server.sessionRunner = func(ctx context.Context, model, prompt, workingDirectory string) (string, error) {
		if workingDirectory != "/tmp/demo" {
			t.Fatalf("expected workingDirectory /tmp/demo, got %q", workingDirectory)
		}
		expectedPrompt := "## User Turn 1\nReply with exactly pong"
		if prompt != expectedPrompt {
			t.Fatalf("unexpected prompt %q", prompt)
		}
		return "pong", nil
	}

	result := server.handleRequest(shared.RPCRequest{
		Method: "session.start",
		Params: map[string]any{
			"sessionId":        "s1",
			"taskPrompt":       "Reply with exactly pong",
			"workingDirectory": "/tmp/demo",
		},
	})
	if got := result["output"]; got != "pong" {
		t.Fatalf("expected output pong, got %#v", result)
	}
}

func TestNewServerDefaultsHermesToUpstreamPrompt(t *testing.T) {
	server := NewServer(&stubClient{})
	if got := server.upstreamMethod; got != "prompt" {
		t.Fatalf("expected default upstream method prompt, got %q", got)
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
